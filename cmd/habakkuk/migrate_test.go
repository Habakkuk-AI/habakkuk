package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/migrate"
)

// sourceHome copies a fixture from internal/migrate/testdata into a fresh
// user home under the source's default folder name.
func sourceHome(t *testing.T, from string) (userHome, dir string) {
	t.Helper()
	userHome = t.TempDir()
	name := "." + from
	if from == "claude-code" {
		name = ".claude"
	}
	dir = filepath.Join(userHome, name)
	if err := copyHome(dir, filepath.Join("..", "..", "internal", "migrate", "testdata", from)); err != nil {
		t.Fatal(err)
	}
	return userHome, dir
}

// copyHome copies a fixture home, turning its stored dotenv files (the
// repository ignores .env) into .env, and names stored as dot-NAME into
// .NAME.
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
	slices.Reverse(renames)
	for _, r := range renames {
		if err := os.Rename(r[0], r[1]); err != nil {
			return err
		}
	}
	return nil
}

func noEnv(string) string { return "" }

// fixtureSecrets are values in the fixtures that must never be printed.
var fixtureSecrets = []string{"sk-or-fixture-1111", "sk-ant-fixture-2222", "123456:fixture-telegram", "ntn-fixture-3333",
	"ghp_fixtureTokenValue123", "Ab3dE5fG7hJ9", "sk-ant-oc-fixture-6666", "789:fixture-tg", "studio-fixture-7777",
	"remote-fixture-8888", "gsk-fixture-4444", "discord-fixture-5555"}

func assertNoSecrets(t *testing.T, out string) {
	t.Helper()
	for _, s := range fixtureSecrets {
		if strings.Contains(out, s) {
			t.Errorf("output shows a secret value (%s...)", s[:4])
		}
	}
}

func TestMigrateDryRunWritesNothing(t *testing.T) {
	for _, from := range migrate.Sources {
		t.Run(from, func(t *testing.T) {
			user, _ := sourceHome(t, from)
			home := filepath.Join(t.TempDir(), "habakkuk")
			// Nothing in the temporary folder either (skills made from slash
			// commands stay in memory).
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			var out bytes.Buffer
			if err := runMigrate(context.Background(), home, []string{"--dry-run", "--from", from}, strings.NewReader(""), &out, noEnv, user); err != nil {
				t.Fatal(err)
			}
			if left, _ := os.ReadDir(tmp); len(left) > 0 {
				t.Errorf("a dry run wrote %v into the temporary folder", left)
			}
			if !strings.Contains(out.String(), "Dry run: nothing was written.") {
				t.Errorf("output:\n%s", out.String())
			}
			if _, err := os.Stat(home); !os.IsNotExist(err) {
				t.Errorf("a dry run created %s", home)
			}
			assertNoSecrets(t, out.String())
		})
	}
}

func TestMigrateYes(t *testing.T) {
	cases := []struct {
		from, secrets string
		wantEnv       bool
		wantOut       []string
	}{
		{"hermes", "copy", true, []string{"Copied ANTHROPIC_API_KEY", "hk schedules resume sch_", "hk doctor", "hk serve", "hk whatsapp link"}},
		{"hermes", "skip", false, []string{"Set these in", "TELEGRAM_BOT_TOKEN", "Schedules are imported paused"}},
		{"openclaw", "", false, []string{"Set these in", "STUDIO_KEY", "the imessage channel"}},
	}
	for _, c := range cases {
		t.Run(c.from+"/"+c.secrets, func(t *testing.T) {
			user, _ := sourceHome(t, c.from)
			home := t.TempDir()
			args := []string{"--from", c.from, "-y"}
			if c.secrets != "" {
				args = append(args, "--secrets", c.secrets)
			}
			var out bytes.Buffer
			if err := runMigrate(context.Background(), home, args, strings.NewReader(""), &out, noEnv, user); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			for _, w := range c.wantOut {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, out.String())
				}
			}
			assertNoSecrets(t, out.String())
			info, err := os.Stat(filepath.Join(home, ".env"))
			if c.wantEnv != (err == nil) {
				t.Fatalf(".env exists: %v, want %v", err == nil, c.wantEnv)
			}
			if c.wantEnv && runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
				t.Errorf(".env mode %v", info.Mode().Perm())
			}
			if _, err := config.Load(home); err != nil {
				t.Fatalf("config.yaml does not load: %v", err)
			}
			// Again: nothing to do.
			out.Reset()
			if err := runMigrate(context.Background(), home, args, strings.NewReader(""), &out, noEnv, user); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "Nothing to import") {
				t.Errorf("second run:\n%s", out.String())
			}
		})
	}
}

func TestMigrateAsks(t *testing.T) {
	user, _ := sourceHome(t, "hermes")
	home := t.TempDir()
	var out bytes.Buffer
	if err := runMigrate(context.Background(), home, nil, strings.NewReader("n\n"), &out, noEnv, user); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Import now? (Y/n)") || !strings.Contains(out.String(), "Nothing was written.") {
		t.Errorf("output:\n%s", out.String())
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Errorf("declining wrote %d entries", len(entries))
	}
	out.Reset()
	if err := runMigrate(context.Background(), home, nil, strings.NewReader("\nn\n"), &out, noEnv, user); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Copy the 6 keys and tokens into") {
		t.Errorf("output:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".env")); err == nil {
		t.Error("keys were copied after answering no")
	}
	if _, err := os.Stat(filepath.Join(home, "agents", "habakkuk", "SOUL.md")); err != nil {
		t.Error("the import did not run after answering yes")
	}
}

func TestMigratePicksSource(t *testing.T) {
	user, _ := sourceHome(t, "openclaw")
	var out bytes.Buffer
	if err := runMigrate(context.Background(), t.TempDir(), []string{"--dry-run"}, nil, &out, noEnv, user); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Found OpenClaw at") {
		t.Errorf("output:\n%s", out.String())
	}
	if err := copyHome(filepath.Join(user, ".hermes"), filepath.Join("..", "..", "internal", "migrate", "testdata", "hermes")); err != nil {
		t.Fatal(err)
	}
	err := runMigrate(context.Background(), t.TempDir(), []string{"--dry-run"}, nil, &out, noEnv, user)
	if err == nil || !strings.Contains(err.Error(), "pass --from hermes or --from openclaw") {
		t.Errorf("both present: %v", err)
	}
	err = runMigrate(context.Background(), t.TempDir(), []string{"--dry-run"}, nil, &out, noEnv, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "found no setup to import") {
		t.Errorf("none present: %v", err)
	}
	for _, args := range [][]string{{"--from", "other"}, {"--secrets", "maybe"}, {"--history", "some"}, {"--project", "/no/such/folder"},
		{"--from", migrate.Sources[1], "--project", t.TempDir()},
		{"--agent", "a/b"}, {"extra"}} {
		if err := runMigrate(context.Background(), t.TempDir(), args, nil, &out, noEnv, user); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
}

// TestSetupOffersImport: a new home's setup imports only when asked (or,
// with -y, given -migrate), and then edits the config the import wrote.
func TestSetupOffersImport(t *testing.T) {
	user, _ := sourceHome(t, "hermes")
	cases := []struct {
		name  string
		args  []string
		stdin string
		want  bool
	}{
		{"yes without -migrate", []string{"-y"}, "", false},
		{"yes with -migrate", []string{"-y", "-migrate"}, "", true},
		{"interactive yes", nil, "y\n", true},
		{"interactive no", nil, "n\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			var out bytes.Buffer
			s, err := newSetup(context.Background(), home, c.args, strings.NewReader(c.stdin), &out)
			if err != nil {
				t.Fatal(err)
			}
			s.getenv, s.userHome = noEnv, user
			if err := s.load(); err != nil {
				t.Fatal(err)
			}
			if err := s.offerImport(); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			_, err = os.Stat(filepath.Join(home, "config.yaml"))
			if got := err == nil; got != c.want {
				t.Fatalf("imported: %v, want %v\n%s", got, c.want, out.String())
			}
			if c.want {
				if s.cfg == nil || s.cfg.Agents["habakkuk"] == nil {
					t.Error("setup did not reload the imported config")
				}
				if _, err := os.Stat(filepath.Join(home, ".env")); err != nil {
					t.Error("the keys were not copied")
				}
				assertNoSecrets(t, out.String())
			}
		})
	}
}

// An agent found beside coding tools is the one imported; the coding tools
// are named so they can be imported next.
func TestMigratePrefersAgentOverCodingTools(t *testing.T) {
	agentA, agentB := migrate.Sources[0], migrate.Sources[1]
	user, _ := sourceHome(t, agentA)
	if err := copyHome(filepath.Join(user, ".claude"), filepath.Join("..", "..", "internal", "migrate", "testdata", "claude-code")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runMigrate(context.Background(), t.TempDir(), []string{"--dry-run"}, nil, &out, noEnv, user); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Found Claude Code too; run hk migrate again with --from claude-code to import it.") ||
		!strings.Contains(out.String(), "Found "+migrate.Label(agentA)+" at") {
		t.Errorf("output:\n%s", out.String())
	}
	for from, want := range map[string]string{
		agentA + ",claude-code":          agentA,
		"claude-code":                    "claude-code",
		"claude-code,codex":              "pass --from claude-code or --from codex",
		agentA + "," + agentB + ",codex": "pass --from " + agentA + " or --from " + agentB,
	} {
		got, _, err := choose(strings.Split(from, ","))
		if err != nil {
			got = err.Error()
		}
		if !strings.Contains(got, want) {
			t.Errorf("choose(%s) = %q, want %q", from, got, want)
		}
	}
}

func TestMigrateClaudeCode(t *testing.T) {
	user, _ := sourceHome(t, "claude-code")
	project := t.TempDir()
	if err := copyHome(project, filepath.Join("..", "..", "internal", "migrate", "testdata", "claude-code-project")); err != nil {
		t.Fatal(err)
	}
	// You opened the project in Claude Code, trusted it and approved
	// orders-db there.
	statePath := filepath.Join(user, ".claude", ".claude.json")
	var state map[string]any
	if data, err := os.ReadFile(statePath); err != nil || json.Unmarshal(data, &state) != nil {
		t.Fatalf("read %s: %v", statePath, err)
	}
	state["projects"] = map[string]any{project: map[string]any{"hasTrustDialogAccepted": true, "enabledMcpjsonServers": []string{"orders-db"}}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "habakkuk")
	var out bytes.Buffer
	args := []string{"--from", "claude-code", "--project", project, "--secrets", "copy", "-y"}
	if err := runMigrate(context.Background(), home, args, nil, &out, noEnv, user); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	text := out.String()
	for _, want := range []string{"Found Claude Code at", "Adding the project in " + project, "+ review: shared",
		"+ orders-db: orders-mcp --read-only (from the project " + project + ")",
		"Set these in", "ANTHROPIC_API_KEY", "Imported 1 agent"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Ab3dE5fG7h") || strings.Contains(text, "Copied ") && strings.Contains(text, ".env.") {
		t.Errorf("a key was shown or copied:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(home, ".env")); !os.IsNotExist(err) {
		t.Errorf("--secrets copy wrote a .env for a coding tool (%v)", err)
	}
	if data, err := os.ReadFile(filepath.Join(home, "agents", "habakkuk", "AGENTS.md")); err != nil || strings.Contains(string(data), "pnpm") {
		t.Errorf("AGENTS.md = %q, %v; a project's instructions stay out of it", data, err)
	}
	notes, _ := filepath.Glob(filepath.Join(home, "agents", "habakkuk", "memory", "instructions-for-*.md"))
	if len(notes) != 1 {
		t.Errorf("project notes = %v; want the project's CLAUDE.md as one note", notes)
	}
	if !strings.Contains(text, "MCP server rogue in") {
		t.Errorf("the unapproved project server is not named:\n%s", text)
	}
}

func TestMigrateHistoryFlag(t *testing.T) {
	user, _ := sourceHome(t, migrate.Sources[1])
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--dry-run"}, "Conversation history (imported read-only)\n  + habakkuk: 4 conversations (8 messages) to import"},
		{[]string{"--dry-run", "--history", "skip"}, "conversation history: left in"},
	} {
		var out bytes.Buffer
		if err := runMigrate(context.Background(), t.TempDir(), c.args, nil, &out, noEnv, user); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), c.want) {
			t.Errorf("%v: output lacks %q:\n%s", c.args, c.want, out.String())
		}
	}
	home := t.TempDir()
	var out bytes.Buffer
	if err := runMigrate(context.Background(), home, []string{"-y"}, nil, &out, noEnv, user); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "5 past conversations (10 messages) as read-only sessions") {
		t.Errorf("report:\n%s", out.String())
	}
}

// Setup offers a coding tool's import when no agent is on the computer,
// in its own words.
func TestSetupOffersCodingTool(t *testing.T) {
	user, _ := sourceHome(t, "codex")
	home := t.TempDir()
	var out bytes.Buffer
	s, err := newSetup(context.Background(), home, nil, strings.NewReader("n\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	s.getenv, s.userHome = noEnv, user
	if err := s.load(); err != nil {
		t.Fatal(err)
	}
	if err := s.offerImport(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Found a Codex setup in "+filepath.Join(user, ".codex")+". Import its instructions, MCP servers, skills and memory first?") {
		t.Errorf("output:\n%s", out.String())
	}
}

// A server whose command line holds a key comes along only with the keys:
// answering no to the keys leaves it out, and its value appears nowhere.
func TestMigrateKeyedServerFollowsKeysAnswer(t *testing.T) {
	user, dir := sourceHome(t, migrate.Sources[0])
	path := filepath.Join(dir, "config.yaml")
	cfg, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg = []byte(strings.Replace(string(cfg), "mcp_servers:\n", "mcp_servers:\n  shortflag:\n    command: pkg\n    args: [\"-k\", \"shortsecretvalue94\"]\n", 1))
	if err := os.WriteFile(path, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, input string
		args        []string
		kept        bool
	}{
		{"answer no", "y\nn\nn\n", nil, false},
		{"answer yes", "y\ny\nn\n", nil, true},
		{"-y without --secrets", "", []string{"-y"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			var out bytes.Buffer
			if err := runMigrate(context.Background(), home, c.args, strings.NewReader(c.input), &out, noEnv, user); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			conf, _ := os.ReadFile(filepath.Join(home, "config.yaml"))
			env, _ := os.ReadFile(filepath.Join(home, ".env"))
			if strings.Contains(out.String()+string(conf), "shortsecretvalue94") {
				t.Errorf("the key is in the output or config.yaml")
			}
			if got := strings.Contains(string(conf), "shortflag:"); got != c.kept {
				t.Errorf("shortflag in config.yaml: %v, want %v\n%s", got, c.kept, out.String())
			}
			if got := strings.Contains(string(env), "MCP_SHORTFLAG_ARG2=shortsecretvalue94"); got != c.kept {
				t.Errorf("the key in .env: %v, want %v", got, c.kept)
			}
			if c.name == "answer no" && !strings.Contains(out.String(), "Without the keys, these MCP servers are left out: notion, shortflag.") {
				t.Errorf("output:\n%s", out.String())
			}
		})
	}
}
