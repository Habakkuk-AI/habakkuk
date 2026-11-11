package migrate

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/app"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/work/schedule"

	"gopkg.in/yaml.v3"
)

var testNow = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

// copyHome copies a fixture home, turning its stored dotenv files (the
// repository ignores .env) into .env, and names stored as dot-NAME (the
// repository ignores .claude/) into .NAME.
func copyHome(dst, src string) error {
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		return err
	}
	var renames [][2]string
	err := filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch name := d.Name(); {
		case name == "dotenv":
			renames = append(renames, [2]string{p, filepath.Join(filepath.Dir(p), ".env")})
		case strings.HasPrefix(name, "dot-"):
			renames = append(renames, [2]string{p, filepath.Join(filepath.Dir(p), "."+strings.TrimPrefix(name, "dot-"))})
		}
		return nil
	})
	if err != nil {
		return err
	}
	slices.Reverse(renames) // inner names first
	for _, r := range renames {
		if err := os.Rename(r[0], r[1]); err != nil {
			return err
		}
	}
	return nil
}

// fixture copies a synthetic source home from testdata so a test may
// change it.
func fixture(t *testing.T, name string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), name)
	if err := copyHome(dst, filepath.Join("testdata", name)); err != nil {
		t.Fatal(err)
	}
	return dst
}

func options(from, src string, force bool) Options {
	return Options{From: from, Source: src, Force: force, UserHome: filepath.Join(os.TempDir(), "no-such-user-home"),
		Getenv: func(string) string { return "" }, Now: func() time.Time { return testNow }}
}

func mustPlan(t *testing.T, from, src, home string, force bool) *Plan {
	t.Helper()
	o := options(from, src, force)
	f, err := Read(o)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPlan(f, home, o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func item(p *Plan, kind, name string) *Item {
	for _, it := range p.Items {
		if it.Kind == kind && it.Name == name {
			return it
		}
	}
	return nil
}

func status(p *Plan, kind, name string) string {
	if it := item(p, kind, name); it != nil {
		return it.Status
	}
	return "missing"
}

func names[T any](list []T, name func(T) string) []string {
	var out []string
	for _, x := range list {
		out = append(out, name(x))
	}
	return out
}

func TestRead(t *testing.T) {
	type agentWant struct{ name, model, fallbacks, files, skills string }
	cases := []struct {
		from      string
		agents    []agentWant
		providers []string
		mcp       []string
		channels  []string
		schedules []string // name=spec
		secrets   []string
		leftovers []string // substrings
		skipped   []string // substrings
	}{{
		from: Hermes,
		agents: []agentWant{
			{"habakkuk", "openrouter/anthropic/claude-sonnet-4", "anthropic/claude-haiku-4-5", "AGENTS.md,SOUL.md", "notes-helper*,risky*"},
			{"coder", "anthropic/claude-opus-4-6", "", "SOUL.md", "go-review"},
		},
		providers: []string{"anthropic", "openrouter"},
		mcp:       []string{"github", "notion", "time"},
		channels:  []string{"telegram", "whatsapp"},
		schedules: []string{"Morning brief=CRON_TZ=America/Toronto 0 8 * * 1-5", "Inbox sweep=@every 30m", "Digest script=30 18 * * *"},
		secrets:   []string{"ANTHROPIC_API_KEY", "GITHUB_PERSONAL_ACCESS_TOKEN", "MCP_NOTION_URL", "NOTION_TOKEN", "OPENROUTER_API_KEY", "TELEGRAM_BOT_TOKEN"},
		leftovers: []string{"older sessions/ folder", "WhatsApp linked-device session", "board cards"},
		skipped:   []string{"1 skill that came with", "MCP server retired", "One-off long past", "1 schedule of agent habakkuk: turned off"},
	}, {
		from: OpenClaw,
		agents: []agentWant{
			{"habakkuk", "anthropic/claude-opus-4-6", "groq/llama-3.3-70b-versatile", "AGENTS.md,SOUL.md", "shared-skill*,local-skill"},
			{"work", "my-studio/qwen3.8", "", "SOUL.md", ""},
		},
		providers: []string{"anthropic", "groq", "my-studio"},
		mcp:       []string{"docs", "remote"},
		channels:  []string{"discord-ops", "telegram", "whatsapp"},
		schedules: []string{"Weekly money summary=CRON_TZ=Europe/Lisbon 0 9 * * 1", "Disk check=@every 1h0m0s"},
		secrets:   []string{"ANTHROPIC_API_KEY", "DISCORD_BOT_TOKEN_OPS", "GROQ_API_KEY", "MCP_REMOTE_TOKEN", "MCP_REMOTE_URL", "STUDIO_KEY", "TELEGRAM_BOT_TOKEN"},
		leftovers: []string{"heartbeat checklist", "imessage channel", "WhatsApp linked-device session"},
		skipped:   []string{"MCP server parked", "1 schedule: turned off"},
	}}
	for _, c := range cases {
		t.Run(c.from, func(t *testing.T) {
			f, err := Read(options(c.from, fixture(t, c.from), false))
			if err != nil {
				t.Fatal(err)
			}
			if len(f.Agents) != len(c.agents) {
				t.Fatalf("agents %v, want %d", names(f.Agents, func(a *Agent) string { return a.Name }), len(c.agents))
			}
			for i, w := range c.agents {
				a := f.Agents[i]
				skills := names(a.Skills, func(s Skill) string {
					if s.Shared {
						return s.Name + "*"
					}
					return s.Name
				})
				got := agentWant{a.Name, a.Model, strings.Join(a.Fallbacks, ","), strings.Join(sortedKeys(a.Files), ","), strings.Join(skills, ",")}
				if got != w {
					t.Errorf("agent %d = %+v, want %+v", i, got, w)
				}
			}
			check := func(what string, got, want []string) {
				t.Helper()
				if !slices.Equal(got, want) {
					t.Errorf("%s = %q, want %q", what, got, want)
				}
			}
			check("providers", names(f.Providers, func(p *Provider) string { return p.Name }), c.providers)
			check("mcp", names(f.MCP, func(m *MCPServer) string { return m.Name }), c.mcp)
			check("channels", names(f.Channels, func(c *Channel) string { return c.Name }), c.channels)
			check("schedules", names(f.Schedules, func(s *Schedule) string { return s.Name + "=" + s.Spec }), c.schedules)
			check("secrets", sortedKeys(f.Secrets), c.secrets)
			left := strings.Join(names(f.Leftovers, func(l Leftover) string { return l.What }), "\n")
			for _, w := range c.leftovers {
				if !strings.Contains(left, w) {
					t.Errorf("leftovers %q lack %q", left, w)
				}
			}
			skipped := strings.Join(f.Skipped, "\n")
			for _, w := range c.skipped {
				if !strings.Contains(skipped, w) {
					t.Errorf("skipped %q lack %q", skipped, w)
				}
			}
		})
	}
}

// TestReadDetails checks the mapping of the less obvious fields.
func TestReadDetails(t *testing.T) {
	h, err := Read(options(Hermes, fixture(t, Hermes), false))
	if err != nil {
		t.Fatal(err)
	}
	main := h.Agents[0]
	if !strings.Contains(main.Profile, "- Name is Ada; lives in Toronto.") || !strings.HasPrefix(main.Profile, "## About you") {
		t.Errorf("profile = %q", main.Profile)
	}
	if len(main.Notes) != 4 || main.Notes[0].Title != "The home server runs Debian 13 and is" {
		t.Errorf("notes = %+v", main.Notes)
	}
	tg := h.Channels[0]
	if tg.TokenEnv != "TELEGRAM_BOT_TOKEN" || !slices.Equal(tg.AllowFrom, []string{"4242", "5151"}) || tg.Agent != "habakkuk" {
		t.Errorf("telegram = %+v", tg)
	}
	gh := h.MCP[0]
	if gh.Env["GITHUB_PERSONAL_ACCESS_TOKEN"] != "GITHUB_PERSONAL_ACCESS_TOKEN" {
		t.Errorf("github env = %v; a literal secret must move to a variable", gh.Env)
	}
	if h.MCP[1].Headers["Authorization"] != "Bearer ${NOTION_TOKEN}" {
		t.Errorf("notion headers = %v", h.MCP[1].Headers)
	}
	sweep := h.Schedules[1]
	if sweep.To == nil || sweep.To.Account != "telegram" || sweep.To.Conversation != "4242" || !strings.HasPrefix(sweep.Prompt, "Use the notes-helper skill.") {
		t.Errorf("inbox sweep = %+v (to %+v)", sweep, sweep.To)
	}
	digest := h.Schedules[2]
	if !strings.HasPrefix(digest.Command, "python3 ") || !strings.HasSuffix(strings.Trim(digest.Command, `'"`), filepath.Join("scripts", "digest.py")) || digest.OnOutput != "silent" {
		t.Errorf("digest = %+v", digest)
	}

	oc, err := Read(options(OpenClaw, fixture(t, OpenClaw), false))
	if err != nil {
		t.Fatal(err)
	}
	m := oc.Agents[0]
	if m.Identity == nil || m.Identity.Name != "Clawd" || m.Identity.Emoji != "🦞" || m.Identity.Style != "dry wit" {
		t.Errorf("identity = %+v", m.Identity)
	}
	if !strings.Contains(m.Files["AGENTS.md"], "## Tools\n\nThe printer is called office-laser.") {
		t.Errorf("AGENTS.md = %q", m.Files["AGENTS.md"])
	}
	if got := names(m.Notes, func(n Note) string { return n.Title }); !slices.Equal(got, []string{"Home network", "Decisions", "Daily notes 2026-11-01"}) {
		t.Errorf("notes = %q", got)
	}
	var ops *Channel
	for _, c := range oc.Channels {
		if c.Name == "discord-ops" {
			ops = c
		}
	}
	if ops == nil || ops.Agent != "work" || ops.TokenEnv != "DISCORD_BOT_TOKEN_OPS" {
		t.Errorf("discord-ops = %+v", ops)
	}
	if oc.Secrets["TELEGRAM_BOT_TOKEN"] != "789:fixture-tg" {
		t.Error("the telegram token referenced as ${TG_TOKEN} was not carried into TELEGRAM_BOT_TOKEN")
	}
	disk := oc.Schedules[1]
	if disk.Agent != "work" || disk.Command != "df -h /" || disk.OnOutput != "silent" {
		t.Errorf("disk check = %+v", disk)
	}
}

// A key one home copies is not "left behind" because another profile's .env
// holds it too without using it: here a writer profile on OpenRouter keeps
// ANTHROPIC_API_KEY, which the main home's fallback copies.
func TestOtherEnvSkipsKeysCopiedElsewhere(t *testing.T) {
	src := fixture(t, Hermes)
	dir := filepath.Join(src, "profiles", "writer")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"config.yaml": "model:\n  default: anthropic/claude-sonnet-4\n  provider: openrouter\n",
		".env":        "ANTHROPIC_API_KEY=sk-ant-fixture\nOPENROUTER_API_KEY=sk-or-fixture\nWRITER_ONLY=1\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h, err := Read(options(Hermes, src, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.Secrets["ANTHROPIC_API_KEY"]; !ok {
		t.Fatal("ANTHROPIC_API_KEY was not copied; the fixture changed")
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENROUTER_API_KEY"} {
		if slices.Contains(h.OtherEnv, k) {
			t.Errorf("OtherEnv = %v; %s was copied, so it is not left behind", h.OtherEnv, k)
		}
	}
	for _, k := range []string{"WEATHER_API_KEY", "WRITER_ONLY"} {
		if !slices.Contains(h.OtherEnv, k) {
			t.Errorf("OtherEnv = %v; want the unused %s listed", h.OtherEnv, k)
		}
	}
}

func TestPlanNeverShowsSecrets(t *testing.T) {
	for _, from := range []string{Hermes, OpenClaw} { // coding tools: TestCodingToolsNeverTakeKeys
		t.Run(from, func(t *testing.T) {
			home := t.TempDir()
			p := mustPlan(t, from, fixture(t, from), home, false)
			var out bytes.Buffer
			for _, mode := range []string{"", "copy", "skip"} {
				p.Write(&out, mode)
			}
			text := out.String()
			if len(p.Found.Secrets) == 0 {
				t.Fatal("the fixture should hold secrets")
			}
			for k, v := range p.Found.Secrets {
				if strings.Contains(text, v) {
					t.Errorf("the plan prints the value of %s", k)
				}
				if !strings.Contains(text, k) {
					t.Errorf("the plan does not name %s", k)
				}
			}
			for _, leak := range []string{"Ab3dE5fG7h", "fixture-"} {
				if strings.Contains(text, leak) {
					t.Errorf("the plan prints %q", leak)
				}
			}
		})
	}
}

func TestPlanWritesNothing(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	for _, from := range Sources {
		mustPlan(t, from, fixture(t, from), home, false)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("planning created %s (%v)", home, err)
	}
}

func listSchedules(t *testing.T, home string) []schedule.Schedule {
	t.Helper()
	db, err := app.OpenControl(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	list, err := schedule.NewStore(db, time.UTC).List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestApply(t *testing.T) {
	cases := []struct {
		from      string
		files     map[string]string // path under home -> substring
		absent    []string
		notes     map[string]int // agent -> notes
		channel   string         // name -> agent it should have
		agent     string
		schedules int
	}{{
		from: Hermes,
		files: map[string]string{
			"agents/habakkuk/SOUL.md":                "You are Wren",
			"agents/habakkuk/AGENTS.md":              "British English",
			"agents/habakkuk/USER.md":                "Prefers short answers",
			"agents/coder/SOUL.md":                   "focused coding assistant",
			"skills/notes-helper/SKILL.md":           "tidy summary",
			"agents/coder/skills/go-review/SKILL.md": "go vet",
			".env":                                   "TELEGRAM_BOT_TOKEN=123456:fixture-telegram",
		},
		absent:    []string{"skills/risky", "skills/bundled-one", "agents/coder/skills/notes-helper"},
		notes:     map[string]int{"habakkuk": 3, "coder": 1},
		channel:   "telegram",
		agent:     "habakkuk",
		schedules: 3,
	}, {
		from: OpenClaw,
		files: map[string]string{
			"agents/habakkuk/IDENTITY.md":                 "name: Clawd",
			"agents/habakkuk/USER.md":                     "## Preferences",
			"agents/work/SOUL.md":                         "on-call rota",
			"skills/shared-skill/SKILL.md":                "polite reply",
			"agents/habakkuk/skills/local-skill/SKILL.md": "receipts",
			".env": "STUDIO_KEY=studio-fixture-7777",
		},
		notes:     map[string]int{"habakkuk": 3},
		channel:   "discord-ops",
		agent:     "work",
		schedules: 2,
	}}
	for _, c := range cases {
		t.Run(c.from, func(t *testing.T) {
			home := t.TempDir()
			p := mustPlan(t, c.from, fixture(t, c.from), home, false)
			res, err := p.Apply(context.Background(), true)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Failed) > 0 {
				for it, err := range res.Failed {
					t.Errorf("%s %s failed: %v", it.Kind, it.Name, err)
				}
			}
			cfg, err := config.Load(home)
			if err != nil {
				t.Fatalf("config.yaml does not load: %v", err)
			}
			if ch := cfg.Channels[c.channel]; ch == nil || ch.Agent != c.agent {
				t.Errorf("channel %s = %+v, want agent %s", c.channel, ch, c.agent)
			}
			for _, a := range p.Found.Agents {
				if len(a.MCP) > 0 && !slices.Contains(cfg.Agents[a.Name].Toolsets, "mcp:"+a.MCP[0]) {
					t.Errorf("agent %s toolsets %v lack mcp:%s", a.Name, cfg.Agents[a.Name].Toolsets, a.MCP[0])
				}
			}
			for path, want := range c.files {
				if got := readFile(t, filepath.Join(home, path)); !strings.Contains(got, want) {
					t.Errorf("%s = %q, want it to contain %q", path, got, want)
				}
			}
			for _, path := range c.absent {
				if _, err := os.Stat(filepath.Join(home, path)); err == nil {
					t.Errorf("%s should not have been imported", path)
				}
			}
			for ag, n := range c.notes {
				entries, _ := os.ReadDir(filepath.Join(home, "agents", ag, "memory"))
				if len(entries) != n {
					t.Errorf("agent %s has %d notes, want %d", ag, len(entries), n)
				}
			}
			if runtime.GOOS != "windows" {
				info, err := os.Stat(filepath.Join(home, ".env"))
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Errorf(".env mode = %v (%v), want 0600", info.Mode().Perm(), err)
				}
			}
			for k, v := range p.Found.Secrets {
				if strings.Contains(readFile(t, filepath.Join(home, "config.yaml")), v) {
					t.Errorf("config.yaml holds the value of %s", k)
				}
			}
			list := listSchedules(t, home)
			if len(list) != c.schedules || len(res.Paused) != c.schedules {
				t.Fatalf("%d schedules (%d reported), want %d", len(list), len(res.Paused), c.schedules)
			}
			for _, sc := range list {
				if sc.Enabled {
					t.Errorf("schedule %q is enabled; imports must be paused", sc.Name)
				}
			}
		})
	}
}

func TestApplyTwiceChangesNothing(t *testing.T) {
	for _, from := range Sources {
		t.Run(from, func(t *testing.T) {
			home, src := t.TempDir(), fixture(t, from)
			if _, err := mustPlan(t, from, src, home, false).Apply(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			cfg1 := readFile(t, filepath.Join(home, "config.yaml"))
			p := mustPlan(t, from, src, home, false)
			for _, it := range p.Items {
				if it.Changes() {
					t.Errorf("second run would change %s %s (%s: %s)", it.Kind, it.Name, it.Status, it.Detail)
				}
			}
			if p.Config != nil {
				t.Error("second run would rewrite config.yaml")
			}
			if _, err := p.Apply(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			if readFile(t, filepath.Join(home, "config.yaml")) != cfg1 {
				t.Error("config.yaml changed on the second run")
			}
			if _, err := os.Stat(filepath.Join(home, "config.yaml.bak")); err == nil {
				t.Error("a second run with nothing to do wrote config.yaml.bak")
			}
			if n := len(listSchedules(t, home)); n != len(p.Found.Schedules) {
				t.Errorf("%d schedules after two runs, want %d", n, len(p.Found.Schedules))
			}
		})
	}
}

func TestNoOverwriteWithoutForce(t *testing.T) {
	ctx := context.Background()
	home, src := t.TempDir(), fixture(t, Hermes)
	write := func(rel, text string) {
		t.Helper()
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("agents/habakkuk/SOUL.md", "You are mine; leave me be.\n")
	write("skills/notes-helper/SKILL.md", "---\nname: notes-helper\ndescription: my own\n---\n\nMine.\n")
	write("agents/habakkuk/memory/weekly-report-goes-out-on-thursdays.md", "---\ntitle: Weekly report goes out on Thursdays\n---\n\nActually on Mondays now.\n")
	write(".env", "OPENROUTER_API_KEY=sk-or-mine\n")
	db, err := app.OpenControl(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	old, err := schedule.NewStore(db, time.UTC).Create(ctx, schedule.Schedule{Agent: "habakkuk", Name: "Morning brief", Spec: "0 7 * * *", Prompt: "Mine."})
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	p := mustPlan(t, Hermes, src, home, false)
	for _, c := range []struct{ kind, name string }{
		{"file", "SOUL.md"}, {"skill", "notes-helper"}, {"note", "Weekly report goes out on Thursdays"},
		{"secret", "OPENROUTER_API_KEY"}, {"schedule", "Morning brief"},
	} {
		if got := status(p, c.kind, c.name); got != Conflict {
			t.Errorf("%s %s: %s, want %s", c.kind, c.name, got, Conflict)
		}
	}
	if _, err := p.Apply(ctx, true); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(home, "agents/habakkuk/SOUL.md")); got != "You are mine; leave me be.\n" {
		t.Errorf("SOUL.md was overwritten: %q", got)
	}
	if got := readFile(t, filepath.Join(home, "skills/notes-helper/SKILL.md")); !strings.Contains(got, "Mine.") {
		t.Errorf("the skill was overwritten: %q", got)
	}
	if got := readFile(t, filepath.Join(home, ".env")); !strings.Contains(got, "OPENROUTER_API_KEY=sk-or-mine") {
		t.Errorf(".env value was overwritten: %q", got)
	}
	if !slices.ContainsFunc(listSchedules(t, home), func(s schedule.Schedule) bool { return s.ID == old.ID }) {
		t.Error("the existing schedule was replaced without --force")
	}

	p = mustPlan(t, Hermes, src, home, true)
	if got := status(p, "file", "SOUL.md"); got != Replace {
		t.Fatalf("with force: SOUL.md %s, want %s", got, Replace)
	}
	if _, err := p.Apply(ctx, true); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(home, "agents/habakkuk/SOUL.md")); !strings.Contains(got, "Wren") {
		t.Errorf("with force, SOUL.md = %q", got)
	}
	if got := readFile(t, filepath.Join(home, "agents/habakkuk/SOUL.md.bak")); !strings.Contains(got, "leave me be") {
		t.Errorf("SOUL.md.bak = %q", got)
	}
	if got := readFile(t, filepath.Join(home, "skills/notes-helper.bak/SKILL.md")); !strings.Contains(got, "Mine.") {
		t.Errorf("the replaced skill was not kept: %q", got)
	}
	if got := readFile(t, filepath.Join(home, ".env")); !strings.Contains(got, "OPENROUTER_API_KEY=sk-or-fixture-1111") {
		t.Errorf("with force .env = %q", got)
	}
	if slices.ContainsFunc(listSchedules(t, home), func(s schedule.Schedule) bool { return s.ID == old.ID }) {
		t.Error("with force, the old schedule is still there")
	}
}

func TestExistingConfigKeepsComments(t *testing.T) {
	home := t.TempDir()
	old := "# my notes about this file\nversion: 1\n\nproviders:\n  anthropic:\n    preset: anthropic # keep me\n\nagents:\n  habakkuk:\n    model: anthropic/claude-opus-4-6\n"
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	p := mustPlan(t, Hermes, fixture(t, Hermes), home, false)
	if got := status(p, "agent", "habakkuk"); got != Conflict {
		t.Errorf("agent habakkuk with another model: %s, want %s", got, Conflict)
	}
	if got := status(p, "agent", "coder"); got != New {
		t.Errorf("agent coder: %s, want %s", got, New)
	}
	if got := status(p, "provider", "anthropic"); got != Same {
		t.Errorf("provider anthropic: %s, want %s", got, Same)
	}
	if _, err := p.Apply(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, filepath.Join(home, "config.yaml"))
	for _, want := range []string{"# my notes about this file", "# keep me", "model: anthropic/claude-opus-4-6", "coder:", "mcp:github"} {
		if !strings.Contains(got, want) {
			t.Errorf("config.yaml lacks %q:\n%s", want, got)
		}
	}
	if readFile(t, filepath.Join(home, "config.yaml.bak")) != old {
		t.Error("config.yaml.bak is not the previous config")
	}
	if _, err := os.Stat(filepath.Join(home, ".env")); err == nil {
		t.Error("keys were written without copySecrets")
	}
}

func TestSecretsSkipListsNames(t *testing.T) {
	home := t.TempDir()
	res, err := mustPlan(t, OpenClaw, fixture(t, OpenClaw), home, false).Apply(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".env")); err == nil {
		t.Error(".env was written with secrets skipped")
	}
	if !slices.Contains(res.ToSet, "STUDIO_KEY") || !slices.Contains(res.ToSet, "TELEGRAM_BOT_TOKEN") || len(res.Copied) != 0 {
		t.Errorf("to set %v, copied %v", res.ToSet, res.Copied)
	}
}

func TestBrokenSources(t *testing.T) {
	cases := []struct {
		name    string
		from    string
		break_  func(dir string) error
		wantErr string // "" means it reads, with skip containing wantSkip
		skip    string
	}{
		{"hermes config", Hermes, func(d string) error {
			return os.WriteFile(filepath.Join(d, "config.yaml"), []byte("model: [unclosed\n"), 0o600)
		}, "config.yaml does not parse as YAML", ""},
		{"hermes profile config", Hermes, func(d string) error {
			return os.WriteFile(filepath.Join(d, "profiles", "coder", "config.yaml"), []byte(":\n\t- x"), 0o600)
		}, "does not parse as YAML", ""},
		{"openclaw config", OpenClaw, func(d string) error {
			return os.WriteFile(filepath.Join(d, "openclaw.json"), []byte("{ agents: { entries: ["), 0o600)
		}, "openclaw.json does not parse as JSON5", ""},
		{"hermes jobs", Hermes, func(d string) error {
			return os.WriteFile(filepath.Join(d, "cron", "jobs.json"), []byte("{not json"), 0o600)
		}, "", "jobs.json is not valid JSON"},
		{"openclaw jobs", OpenClaw, func(d string) error {
			return os.WriteFile(filepath.Join(d, "cron", "jobs.json"), []byte("[{"), 0o600)
		}, "", "jobs.json is not valid JSON"},
		{"hermes empty home", Hermes, func(d string) error {
			if err := os.RemoveAll(d); err != nil {
				return err
			}
			return os.MkdirAll(d, 0o700)
		}, "", ""},
		{"missing home", Hermes, os.RemoveAll, "no Hermes Agent home at", ""},
		{"home is a file", OpenClaw, func(d string) error {
			if err := os.RemoveAll(d); err != nil {
				return err
			}
			return os.WriteFile(d, []byte("x"), 0o600)
		}, "is not a folder", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := fixture(t, c.from)
			if err := c.break_(dir); err != nil {
				t.Fatal(err)
			}
			f, err := Read(options(c.from, dir, false))
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.skip != "" && !strings.Contains(strings.Join(f.Skipped, "\n"), c.skip) {
				t.Errorf("skipped %q lacks %q", f.Skipped, c.skip)
			}
			// What could be read still plans cleanly.
			if _, err := NewPlan(f, t.TempDir(), options(c.from, dir, false)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestEmptyHomeWithoutModel: with no model to set up, the files are
// imported and config.yaml is left for hk setup.
func TestEmptyHomeWithoutModel(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "SOUL.md"), []byte("You are Quill.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".env"), []byte("TELEGRAM_BOT_TOKEN=1:x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	p := mustPlan(t, Hermes, src, home, false)
	if p.Config != nil || p.NoConfig == "" || p.ConfigErr != nil {
		t.Fatalf("config %q, noconfig %q, err %v", p.Config, p.NoConfig, p.ConfigErr)
	}
	if got := status(p, "channel", "telegram"); got != Rejected {
		t.Errorf("channel telegram: %s, want %s", got, Rejected)
	}
	if _, err := p.Apply(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(home, "agents/habakkuk/SOUL.md")); got != "You are Quill.\n" {
		t.Errorf("SOUL.md = %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, "config.yaml")); err == nil {
		t.Error("config.yaml was written without a model")
	}
}

func TestOpenClawScheduleDatabase(t *testing.T) {
	src := fixture(t, OpenClaw)
	if err := os.MkdirAll(filepath.Join(src, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(src, "state", "openclaw.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	job, _ := json.Marshal(map[string]any{
		"id": "j9", "name": "Evening wrap",
		"schedule": map[string]any{"kind": "cron", "expr": "0 21 * * *"},
		"payload":  map[string]any{"kind": "agentTurn", "message": "Wrap up the day."},
		"delivery": map[string]any{"mode": "announce", "to": "telegram:4242"},
	})
	for _, q := range []string{
		`CREATE TABLE cron_jobs (store_key TEXT NOT NULL, job_id TEXT NOT NULL, name TEXT NOT NULL, enabled INTEGER NOT NULL,
			agent_id TEXT, payload_kind TEXT NOT NULL, job_json TEXT NOT NULL, sort_order INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL, PRIMARY KEY (store_key, job_id))`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO cron_jobs VALUES ('default', 'j9', 'Evening wrap', 1, 'work', 'agentTurn', ?, 0, 1)`, string(job)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	f, err := Read(options(OpenClaw, src, false))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Schedules) != 1 {
		t.Fatalf("schedules %v: the database should win over the older jobs.json", names(f.Schedules, func(s *Schedule) string { return s.Name }))
	}
	sc := f.Schedules[0]
	if sc.Name != "Evening wrap" || sc.Agent != "work" || sc.Spec != "0 21 * * *" || sc.To == nil || sc.To.Account != "telegram" || sc.To.Conversation != "4242" {
		t.Errorf("schedule = %+v (to %+v)", sc, sc.To)
	}
}

func TestDetect(t *testing.T) {
	user := t.TempDir()
	env := map[string]string{}
	o := Options{UserHome: user, Getenv: func(k string) string { return env[k] }}
	if got := Detect(o); len(got) != 0 {
		t.Fatalf("empty home: %v", got)
	}
	if err := copyHome(filepath.Join(user, ".openclaw"), filepath.Join("testdata", OpenClaw)); err != nil {
		t.Fatal(err)
	}
	if got := Detect(o); !slices.Equal(got, []string{OpenClaw}) {
		t.Errorf("detect = %v", got)
	}
	other := filepath.Join(user, "elsewhere")
	if err := copyHome(other, filepath.Join("testdata", Hermes)); err != nil {
		t.Fatal(err)
	}
	env["HERMES_HOME"] = other
	if got := Detect(o); !slices.Equal(got, []string{Hermes, OpenClaw}) {
		t.Errorf("with HERMES_HOME: %v", got)
	}
	env["OPENCLAW_STATE_DIR"] = filepath.Join(user, "nothing-here")
	if got := Detect(o); !slices.Equal(got, []string{Hermes}) {
		t.Errorf("with OPENCLAW_STATE_DIR: %v", got)
	}
	if got := DefaultSource(OpenClaw, Options{UserHome: user, Getenv: func(k string) string {
		return map[string]string{"OPENCLAW_PROFILE": "work"}[k]
	}}); got != filepath.Join(user, ".openclaw-work") {
		t.Errorf("profile home = %s", got)
	}
}

func TestJSON5(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{a: 1, b: 'two', c: [1, 2,], }`, `{"a":1,"b":"two","c":[1,2]}`},
		{"// note\n{ /* block\n comment */ \"k\": 0x1F, n: -.5, t: true, z: null }", `{"k":31,"n":-0.5,"t":true,"z":null}`},
		{`{url: "https://x.example/a//b", s: 'it\'s', e: "a\"b"}`, `{"url":"https://x.example/a//b","s":"it's","e":"a\"b"}`},
		{`{inf: Infinity, big: +12}`, `{"inf":null,"big":12}`},
	}
	for _, c := range cases {
		var got, want any
		if err := decodeJSON5([]byte(c.in), &got); err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if err := json.Unmarshal([]byte(c.want), &want); err != nil {
			t.Fatal(err)
		}
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		if string(g) != string(w) {
			t.Errorf("%s = %s, want %s", c.in, g, w)
		}
	}
	for _, bad := range []string{`{a: "unclosed}`, `{a: /* open`, `{a: @}`} {
		var v any
		if err := decodeJSON5([]byte(bad), &v); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}

// Facts in the memory store database become notes, tagged with their
// category and tags; a fact that repeats a MEMORY.md entry is not doubled.
func TestHermesFactStore(t *testing.T) {
	src := fixture(t, Hermes)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(src, "memory_store.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE facts (fact_id INTEGER PRIMARY KEY, content TEXT, category TEXT, tags TEXT, trust_score REAL, created_at TEXT)`,
		`INSERT INTO facts (content, category, tags, created_at) VALUES ('Ada wants a phone call, not a message, for anything before 8am.', 'user_pref', 'Ada,mornings', '2026-09-01 07:00:00')`,
		`INSERT INTO facts (content, category, tags, created_at) VALUES ('The home server runs Debian 13 and is reached as nas.local.', 'general', '', '2026-09-02 07:00:00')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	h, err := Read(options(Hermes, src, false))
	if err != nil {
		t.Fatal(err)
	}
	main := h.Agents[0]
	if len(main.Notes) != 5 {
		t.Fatalf("notes = %d (%v); want the 4 MEMORY.md entries plus 1 new fact", len(main.Notes), names(main.Notes, func(n Note) string { return n.Title }))
	}
	var got *Note
	for i := range main.Notes {
		if strings.HasPrefix(main.Notes[i].Body, "Ada wants a phone call") {
			got = &main.Notes[i]
		}
	}
	if got == nil {
		t.Fatal("the fact was not imported")
	}
	for _, want := range []string{"imported", "user_pref", "ada", "mornings"} {
		if !slices.Contains(got.Tags, want) {
			t.Errorf("tags = %v, want %q among them", got.Tags, want)
		}
	}
}

// Imported script schedules get room to run: ten minutes, or their interval
// when it is shorter, never below the two-minute default.
func TestImportedScriptTimeout(t *testing.T) {
	for spec, want := range map[string]time.Duration{
		"*/10 * * * *": 10 * time.Minute,
		"@every 5m":    5 * time.Minute,
		"@every 1m":    2 * time.Minute,
		"@every 3h":    10 * time.Minute,
	} {
		if got := importedScriptTimeout(spec); got != want {
			t.Errorf("%s: %v, want %v", spec, got, want)
		}
	}
}

// A source's list of calls allowed without asking is named as left
// behind, never imported as an approval rule; the rules you set in
// config.yaml stay as they are, even with force.
func TestAllowListsStayBehind(t *testing.T) {
	home := t.TempDir()
	old := "version: 1\nproviders:\n  anthropic:\n    preset: anthropic\nagents:\n  habakkuk:\n    model: anthropic/claude-opus-4-6\n    allow:\n      - exact: git status\n"
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	hermes := fixture(t, Hermes)
	cfg := readFile(t, filepath.Join(hermes, "config.yaml")) + "\ncommand_allowlist:\n  - rm -rf build\n"
	if err := os.WriteFile(filepath.Join(hermes, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	openclaw := fixture(t, OpenClaw)
	if err := os.WriteFile(filepath.Join(openclaw, "exec-approvals.json"), []byte(`{"version":1,"agents":{"main":{"allowlist":[{"pattern":"/usr/bin/curl"}]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ from, src, want string }{
		{Hermes, hermes, "the commands agent habakkuk ran without asking (command_allowlist)"},
		{OpenClaw, openclaw, "the commands allowed without asking (exec-approvals.json)"},
		{ClaudeCode, fixture(t, ClaudeCode), "the tools and commands allowed without asking in Claude Code's settings.json"},
		{Codex, fixture(t, Codex), "Codex's command rules"},
	} {
		p := mustPlan(t, c.from, c.src, home, true)
		var l *Leftover
		for i := range p.Found.Leftovers {
			if p.Found.Leftovers[i].What == c.want {
				l = &p.Found.Leftovers[i]
			}
		}
		if l == nil || !strings.Contains(l.Instead, "not imported") || !strings.Contains(l.Instead, ".allow in config.yaml") {
			t.Errorf("%s: leftovers = %+v; want %q", c.from, p.Found.Leftovers, c.want)
		}
		if _, err := p.Apply(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		var got struct {
			Agents map[string]struct {
				Allow []map[string]string `yaml:"allow"`
			} `yaml:"agents"`
		}
		if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(home, "config.yaml"))), &got); err != nil {
			t.Fatal(err)
		}
		for name, a := range got.Agents {
			if want := name == "habakkuk"; len(a.Allow) != map[bool]int{true: 1}[want] || want && a.Allow[0]["exact"] != "git status" {
				t.Errorf("%s: agent %s allow = %v", c.from, name, a.Allow)
			}
		}
		_ = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
			if err == nil && d.Name() == "approvals.json" {
				t.Errorf("%s: an approval rule file was written: %s", c.from, path)
			}
			return nil
		})
	}
}
