package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
)

// fixtureKey is in every made-up key and token of the coding-tool
// fixtures; none may reach the plan or the Habakkuk home.
const fixtureKey = "Ab3dE5fG7h"

func skillNames(a *Agent) []string {
	return names(a.Skills, func(s Skill) string { return s.Name })
}

func noteTitles(a *Agent) []string {
	return names(a.Notes, func(n Note) string { return n.Title })
}

func leftoverText(f *Found) string {
	return strings.Join(names(f.Leftovers, func(l Leftover) string { return l.What + ": " + l.Instead }), "\n")
}

func TestReadClaudeCode(t *testing.T) {
	f, err := Read(options(ClaudeCode, fixture(t, ClaudeCode), false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if len(f.Agents) != 1 {
		t.Fatalf("%d agents, want 1", len(f.Agents))
	}
	a := f.Agents[0]
	if a.Name != "habakkuk" || !a.Main || a.Model != "anthropic/claude-sonnet-5-5" {
		t.Errorf("agent = %s main=%v model=%q", a.Name, a.Main, a.Model)
	}
	if !strings.Contains(a.Files["AGENTS.md"], "Run the tests before you say a change is done.") {
		t.Errorf("AGENTS.md = %q", a.Files["AGENTS.md"])
	}
	if got, want := skillNames(a), []string{"pdf-notes", "git-commit", "review"}; !slices.Equal(got, want) {
		t.Errorf("skills = %v, want %v", got, want)
	}
	if got, want := noteTitles(a), []string{"Oven schedule", "Short answers"}; !slices.Equal(got, want) {
		t.Errorf("notes = %v, want %v (the MEMORY.md index is left out)", got, want)
	}
	if got := names(f.MCP, func(m *MCPServer) string { return m.Name }); !slices.Equal(got, []string{"github", "tracker"}) {
		t.Errorf("mcp = %v", got)
	}
	if !slices.Equal(a.MCP, []string{"github", "tracker"}) {
		t.Errorf("agent mcp = %v", a.MCP)
	}
	if len(f.Secrets) != 0 {
		t.Errorf("secrets = %v; a coding tool's keys are never taken", sortedKeys(f.Secrets))
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "GITHUB_PERSONAL_ACCESS_TOKEN", "TRACKER_TOKEN"} {
		if !slices.Contains(f.Missing, k) {
			t.Errorf("missing = %v; want %s listed for the user to set", f.Missing, k)
		}
	}
	skipped := strings.Join(f.Skipped, "\n")
	for _, w := range []string{"/empty: it holds no text", "/pdf-notes: a skill named pdf-notes", "MCP server parked"} {
		if !strings.Contains(skipped, w) {
			t.Errorf("skipped %q lack %q", skipped, w)
		}
	}
	left := leftoverText(f)
	for _, w := range []string{"Claude Code's agents", "hooks in Claude Code's settings.json", "Claude Code sign-in", "past coding sessions"} {
		if !strings.Contains(left, w) {
			t.Errorf("leftovers %q lack %q", left, w)
		}
	}
	// A slash command becomes a skill that says what it was.
	var review Skill
	for _, s := range a.Skills {
		if s.Name == "review" {
			review = s
		}
	}
	data, err := os.ReadFile(filepath.Join(review.Dir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"name: review", "description: Review the current change for bugs", "slash command /review in Claude Code",
		"$ARGUMENTS", "([file or folder])", "List real bugs first"} {
		if !strings.Contains(string(data), w) {
			t.Errorf("review SKILL.md lacks %q:\n%s", w, data)
		}
	}
	tmp := filepath.Dir(review.Dir)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("Close left %s", tmp)
	}
}

func TestReadClaudeCodeProject(t *testing.T) {
	src, project := fixture(t, ClaudeCode), fixture(t, "claude-code-project")
	// The project's own servers in .claude.json, keyed by its folder.
	state := map[string]any{"projects": map[string]any{project: map[string]any{
		"mcpServers": map[string]any{"preview": map[string]any{"type": "sse", "url": "https://preview.example/sse"}},
	}}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(src, ".claude.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	o := options(ClaudeCode, src, false)
	o.Project = project
	f, err := Read(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	a := f.Agents[0]
	agents := a.Files["AGENTS.md"]
	if !strings.Contains(agents, "Answer in plain English") || !strings.Contains(agents, "## Instructions for "+project+" (CLAUDE.md)\n\nThis is the Corner Bakery ordering site.") {
		t.Errorf("AGENTS.md = %q", agents)
	}
	if got := names(f.MCP, func(m *MCPServer) string { return m.Name }); !slices.Equal(got, []string{"orders-db", "preview"}) {
		t.Errorf("mcp = %v", got)
	}
	for _, s := range []string{"deploy-check", "ship"} {
		if !slices.Contains(skillNames(a), s) {
			t.Errorf("skills %v lack %s", skillNames(a), s)
		}
	}
	if a.Workspace != project {
		t.Errorf("workspace = %q, want the project", a.Workspace)
	}
}

func TestReadCodex(t *testing.T) {
	f, err := Read(options(Codex, fixture(t, Codex), false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	a := f.Agents[0]
	if a.Model != "chatgpt/gpt-5.6-sol" {
		t.Errorf("model = %q; Codex signed in with ChatGPT", a.Model)
	}
	if got := names(f.Providers, func(p *Provider) string { return p.Name + "/" + p.Auth }); !slices.Equal(got, []string{"chatgpt/chatgpt"}) {
		t.Errorf("providers = %v", got)
	}
	if !strings.Contains(a.Files["AGENTS.md"], "Keep diffs small.") {
		t.Errorf("AGENTS.md = %q", a.Files["AGENTS.md"])
	}
	if got, want := skillNames(a), []string{"release-notes", "standup"}; !slices.Equal(got, want) {
		t.Errorf("skills = %v, want %v (the built-in .system skills are left out)", got, want)
	}
	if got := noteTitles(a); len(got) != 1 || !strings.HasPrefix(got[0], "Corner Bakery takes wholesale orders") {
		t.Errorf("notes = %v", got)
	}
	var docs, tracker *MCPServer
	for _, m := range f.MCP {
		switch m.Name {
		case "docs":
			docs = m
		case "tracker":
			tracker = m
		}
	}
	if docs == nil || docs.Command != "npx" || docs.Env["DOCS_TOKEN"] != "DOCS_TOKEN" {
		t.Errorf("docs = %+v; its literal token moves to a variable", docs)
	}
	if tracker == nil || tracker.Headers["Authorization"] != "Bearer ${TRACKER_TOKEN}" {
		t.Errorf("tracker = %+v", tracker)
	}
	if len(f.MCP) != 2 {
		t.Errorf("mcp = %v; the turned-off server is left out", names(f.MCP, func(m *MCPServer) string { return m.Name }))
	}
	if len(f.Secrets) != 0 || !slices.Contains(f.Missing, "DOCS_TOKEN") || !slices.Contains(f.Missing, "TRACKER_TOKEN") {
		t.Errorf("secrets = %v, missing = %v", sortedKeys(f.Secrets), f.Missing)
	}
	left := leftoverText(f)
	for _, w := range []string{"chatgpt subscription sign-in", "notify command", "command rules"} {
		if !strings.Contains(left, w) {
			t.Errorf("leftovers %q lack %q", left, w)
		}
	}
}

func TestReadCodexAPIKeyAndCustomProvider(t *testing.T) {
	src := fixture(t, Codex)
	if err := os.Remove(filepath.Join(src, "auth.json")); err != nil {
		t.Fatal(err)
	}
	f, err := Read(options(Codex, src, false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if a := f.Agents[0]; a.Model != "openai/gpt-5.6-sol" || !slices.Contains(f.Missing, "OPENAI_API_KEY") {
		t.Errorf("model = %q, missing = %v", a.Model, f.Missing)
	}

	cfg := "model = \"qwen3.8\"\nmodel_provider = \"studio\"\n[model_providers.studio]\nname = \"Studio\"\nbase_url = \"https://studio.example.net/v1\"\nenv_key = \"STUDIO_KEY\"\n"
	if err := os.WriteFile(filepath.Join(src, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err = Read(options(Codex, src, false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if a := f.Agents[0]; a.Model != "studio/qwen3.8" || !slices.Contains(f.Missing, "STUDIO_KEY") {
		t.Errorf("model = %q, missing = %v", a.Model, f.Missing)
	}
}

// Whatever the answer to the keys question, a coding tool's keys and
// sign-ins stay where they are.
func TestCodingToolsNeverTakeKeys(t *testing.T) {
	for _, from := range []string{ClaudeCode, Codex} {
		t.Run(from, func(t *testing.T) {
			home := t.TempDir()
			p := mustPlan(t, from, fixture(t, from), home, false)
			t.Cleanup(func() { p.Found.Close() })
			var out bytes.Buffer
			for _, mode := range []string{"", "copy", "skip"} {
				p.Write(&out, mode)
			}
			if strings.Contains(out.String(), fixtureKey) || strings.Contains(out.String(), "sam@example.com") {
				t.Errorf("the plan shows a key or account:\n%s", out.String())
			}
			res, err := p.Apply(context.Background(), true)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Copied) > 0 {
				t.Errorf("copied %v", res.Copied)
			}
			err = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				data, err := os.ReadFile(path)
				if err == nil && bytes.Contains(data, []byte(fixtureKey)) {
					t.Errorf("%s holds a key from the fixture", path)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestApplyClaudeCode(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	p := mustPlan(t, ClaudeCode, fixture(t, ClaudeCode), home, false)
	t.Cleanup(func() { p.Found.Close() })
	res, err := p.Apply(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for it, err := range res.Failed {
		t.Errorf("%s %s: %v", it.Kind, it.Name, err)
	}
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatalf("config.yaml does not load: %v", err)
	}
	ag := cfg.Agents["habakkuk"]
	if ag == nil || ag.Model != "anthropic/claude-sonnet-5-5" || !slices.Contains(ag.Toolsets, "mcp:github") {
		t.Fatalf("agent = %+v", ag)
	}
	if cfg.MCP["github"] == nil || cfg.MCP["github"].Env["GITHUB_PERSONAL_ACCESS_TOKEN"] != "GITHUB_PERSONAL_ACCESS_TOKEN" {
		t.Errorf("github server = %+v", cfg.MCP["github"])
	}
	for path, want := range map[string]string{
		"agents/habakkuk/AGENTS.md":  "Run the tests",
		"skills/pdf-notes/SKILL.md":  "page number",
		"skills/review/SKILL.md":     "slash command /review",
		"skills/git-commit/SKILL.md": "/git:commit",
	} {
		if got := readFile(t, filepath.Join(home, path)); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want %q in it", path, got, want)
		}
	}
	m, err := memory.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if n, err := m.Read(ctx, "habakkuk", memory.Slugify("Oven schedule")); err != nil || !strings.Contains(n.Body, "deck oven") {
		t.Errorf("note = %+v, %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".env")); !os.IsNotExist(err) {
		t.Errorf("the import wrote a .env (%v)", err)
	}
}

func TestDetectCodingTools(t *testing.T) {
	user := t.TempDir()
	for _, p := range []string{".claude/CLAUDE.md", ".codex/config.toml"} {
		path := filepath.Join(user, p)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	o := Options{UserHome: user, Getenv: func(string) string { return "" }}
	if got := Detect(o); !slices.Equal(got, []string{ClaudeCode, Codex}) {
		t.Errorf("Detect = %v", got)
	}
	env := map[string]string{"CLAUDE_CONFIG_DIR": "~/elsewhere/claude", "CODEX_HOME": "/opt/codex"}
	o.Getenv = func(k string) string { return env[k] }
	if got := DefaultSource(ClaudeCode, o); got != filepath.Join(user, "elsewhere", "claude") {
		t.Errorf("claude-code home = %s", got)
	}
	if got := DefaultSource(Codex, o); got != "/opt/codex" {
		t.Errorf("codex home = %s", got)
	}
}
