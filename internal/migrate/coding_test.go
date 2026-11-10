package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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
	if got := names(f.MCP, func(m *MCPServer) string { return m.Name }); !slices.Equal(got, []string{"calendar", "db-cli", "github", "search", "tracker", "warehouse"}) {
		t.Errorf("mcp = %v", got)
	}
	// Servers with possible keys in their command line or URL are found,
	// then left out of the plan: keys never move from a coding tool.
	p, err := NewPlan(f, t.TempDir(), Options{Secrets: "copy"})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"db-cli", "search", "warehouse"} {
		if it := item(p, "mcp", n); it == nil || it.Status != Rejected || !strings.Contains(it.Detail, "keys never move from Claude Code") {
			t.Errorf("%s = %+v", n, it)
		}
	}
	if pa := p.Found.Agents[0]; !slices.Equal(pa.MCP, []string{"calendar", "github", "tracker"}) {
		t.Errorf("agent mcp = %v", pa.MCP)
	}
	for _, m := range f.MCP {
		if m.Name == "calendar" && !m.OAuth {
			t.Error("calendar: Claude Code holds a sign-in for it, so it signs in with OAuth")
		}
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
	for _, w := range []string{"/empty: it holds no text", "/pdf-notes: a skill named pdf-notes", "MCP server parked",
		"MCP server legacy: it uses the older SSE transport"} {
		if !strings.Contains(skipped, w) {
			t.Errorf("skipped %q lack %q", skipped, w)
		}
	}
	left := leftoverText(f)
	for _, w := range []string{"Claude Code's agents", "hooks in Claude Code's settings.json", "Claude Code sign-in", "past coding sessions",
		"the sign-in to MCP server calendar: hk mcp login calendar"} {
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
	if review.Dir != "" {
		t.Errorf("the review skill is in %s; skills made from commands stay in memory until the import", review.Dir)
	}
	data := []byte(review.Text)
	for _, w := range []string{"name: review", "description: Review the current change for bugs", "slash command /review in Claude Code",
		"$ARGUMENTS", "([file or folder])", "List real bugs first"} {
		if !strings.Contains(string(data), w) {
			t.Errorf("review SKILL.md lacks %q:\n%s", w, data)
		}
	}
}

func TestReadClaudeCodeProject(t *testing.T) {
	src, project := fixture(t, ClaudeCode), fixture(t, "claude-code-project")
	// The repository's own .claude/settings.json turns on every server and
	// names rogue: that is whoever wrote the repository speaking, not you.
	writeState := func(pm map[string]any) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"projects": map[string]any{project: pm}})
		if err := os.WriteFile(filepath.Join(src, ".claude.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	local := map[string]any{"preview": map[string]any{"type": "http", "url": "https://preview.example/mcp"}}
	o := options(ClaudeCode, src, false)
	o.Project = project
	mcpNames := func() (*Found, []string) {
		t.Helper()
		f, err := Read(o)
		if err != nil {
			t.Fatal(err)
		}
		return f, names(f.MCP, func(m *MCPServer) string { return m.Name })
	}

	// A folder never opened in Claude Code: none of its .mcp.json servers.
	writeState(nil)
	if _, got := mcpNames(); len(got) != 0 {
		t.Errorf("a repository never opened in Claude Code: mcp = %v", got)
	}
	// Opened, but the trust prompt never accepted: still none.
	writeState(map[string]any{"enabledMcpjsonServers": []any{"orders-db"}})
	if _, got := mcpNames(); len(got) != 0 {
		t.Errorf("without the trust prompt accepted: mcp = %v", got)
	}
	// Trusted, with one server approved, and one added for the folder.
	writeState(map[string]any{"hasTrustDialogAccepted": true, "enabledMcpjsonServers": []any{"orders-db"}, "mcpServers": local})
	f, got := mcpNames()
	if !slices.Equal(got, []string{"orders-db", "preview"}) {
		t.Errorf("mcp = %v", got)
	}
	if !strings.Contains(strings.Join(f.Skipped, "\n"), "MCP server rogue in "+filepath.Join(project, ".mcp.json")+": you never approved it") {
		t.Errorf("skipped = %q", f.Skipped)
	}
	a := f.Agents[0]
	// The project's instructions are about that project: an untrusted note,
	// not the agent's standing instructions.
	if agents := a.Files["AGENTS.md"]; strings.Contains(agents, "pnpm") || !strings.Contains(agents, "Answer in plain English") {
		t.Errorf("AGENTS.md = %q", agents)
	}
	var note *Note
	for i := range a.Notes {
		if strings.HasPrefix(a.Notes[i].Title, "Instructions for claude-code-project") {
			note = &a.Notes[i]
		}
	}
	if note == nil || !strings.Contains(note.Body, "Use pnpm, never npm.") || !slices.Contains(note.Tags, "project") || !note.Untrusted {
		t.Errorf("project note = %+v; notes %v", note, noteTitles(a))
	}
	for _, s := range []string{"deploy-check", "ship"} {
		if !slices.Contains(skillNames(a), s) {
			t.Errorf("skills %v lack %s", skillNames(a), s)
		}
	}
	if a.Workspace != "~/habakkuk-workspace" {
		t.Errorf("workspace = %q; the project does not become the workspace", a.Workspace)
	}
	// The plan shows a project's server in full; the note is saved untrusted.
	home := t.TempDir()
	p, err := NewPlan(f, home, o)
	if err != nil {
		t.Fatal(err)
	}
	if it := item(p, "mcp", "orders-db"); it == nil || it.Detail != "orders-mcp --read-only (from the project "+project+")" {
		t.Errorf("orders-db = %+v", it)
	}
	if _, err := p.Apply(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	m, err := memory.Open(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if n, err := m.Read(context.Background(), "habakkuk", memory.Slugify(note.Title)); err != nil || !n.Untrusted() {
		t.Errorf("the project note = %+v, %v; want it saved untrusted", n, err)
	}

	// Approval of every server, from your own record, but one turned off.
	writeState(map[string]any{"hasTrustDialogAccepted": true, "enableAllProjectMcpServers": true, "disabledMcpjsonServers": []any{"orders-db"}})
	if _, got := mcpNames(); !slices.Equal(got, []string{"rogue"}) {
		t.Errorf("with all enabled but orders-db: mcp = %v", got)
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
	if got := names(f.MCP, func(m *MCPServer) string { return m.Name }); !slices.Equal(got, []string{"docs", "files", "git", "shop", "tracker"}) {
		t.Errorf("mcp = %v; the turned-off server and the app's own are left out", got)
	}
	p, err := NewPlan(f, t.TempDir(), Options{Secrets: "copy"})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"git", "shop"} {
		if it := item(p, "mcp", n); it == nil || it.Status != Rejected {
			t.Errorf("%s = %+v; a server with a key in its command line or URL is left out", n, it)
		}
	}
	for _, m := range f.MCP {
		if m.Name == "files" && (!slices.Equal(m.Allow, []string{"search", "read"}) || !slices.Equal(m.Approve, []string{"delete"})) {
			t.Errorf("files allow %v approve %v", m.Allow, m.Approve)
		}
	}
	skipped := strings.Join(f.Skipped, "\n")
	for _, w := range []string{"MCP server node_repl (Codex): it runs a program inside a desktop app"} {
		if !strings.Contains(skipped, w) {
			t.Errorf("skipped %q lack %q", skipped, w)
		}
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

func TestPlainArgs(t *testing.T) {
	for _, c := range []struct {
		command string
		args    []string
		want    string // p plain, k keyed
	}{
		{"npx", []string{"-y", "@modelcontextprotocol/server-github"}, "pp"},
		{"uvx", []string{"mcp-server-time", "--local-timezone"}, "pp"},
		{"pipx", []string{"run", "mcp-server-fetch"}, "pp"},
		{"npx", []string{"-y", "pkg", "8f14e45f-ceea-467a-9575-0b6b5c4e7a91"}, "ppk"},
		{"pkg", []string{"-k", "shortsecretvalue94"}, "pk"},
		{"pkg", []string{"--key=k1515value"}, "k"},
		{"pkg", []string{"sbp_0123456789abcdef0123456789abcdef01234598"}, "k"},
		{"env", []string{"TOKEN=plainsecret99", "node", "server.js"}, "kkk"},
		{"npx", []string{"mcp-remote", "https://x.example/mcp", "--header", "Authorization: Bearer abcdefgh12345695"}, "pkpk"},
		{"node", []string{"/opt/servers/files/index.js", "~/notes", "./data"}, "ppp"},
	} {
		var got strings.Builder
		for _, p := range plainArgs(c.command, c.args) {
			if p {
				got.WriteByte('p')
			} else {
				got.WriteByte('k')
			}
		}
		if got.String() != c.want {
			t.Errorf("%s %v = %s, want %s", c.command, c.args, got.String(), c.want)
		}
	}
}

func TestSplitURL(t *testing.T) {
	for raw, want := range map[string][2]string{
		"https://ok.example/mcp":                                                 {"https://ok.example/mcp", ""},
		"https://mcp.example.com/v1/mcp":                                         {"https://mcp.example.com/v1/mcp", ""},
		"https://mcp.example.com/s/8f14e45f-ceea-467a-9575-0b6b5c4e7a92/mcp":     {"https://mcp.example.com", "/s/8f14e45f-ceea-467a-9575-0b6b5c4e7a92/mcp"},
		"https://mcp.example.com/mcp?customerId=abc":                             {"https://mcp.example.com", "/mcp?customerId=abc"},
		"https://f.example/mcp#tok=frag1616value":                                {"https://f.example", "/mcp#tok=frag1616value"},
		"https://user:pw1313xx@r.example/mcp":                                    {"https://", "user:pw1313xx@r.example/mcp"},
		"https://mcp.zap.example/api/mcp/s/AbCdEfGh1234IjKlMnOp5678QrStUv90/mcp": {"https://mcp.zap.example", "/api/mcp/s/AbCdEfGh1234IjKlMnOp5678QrStUv90/mcp"},
	} {
		keep, rest := splitURL(raw)
		if [2]string{keep, rest} != want {
			t.Errorf("splitURL(%s) = %q, %q; want %q", raw, keep, rest, want)
		}
	}
}

// verifyServers are servers whose values a check could mistake for plain
// ones; every value marked secret must stay out of config.yaml and the
// plan.
var verifyServers = map[string]map[string]any{
	"s1-uuid-arg":          {"command": "npx", "args": []any{"-y", "pkg", "8f14e45f-ceea-467a-9575-0b6b5c4e7a91"}},
	"s2-uuid-path":         {"type": "http", "url": "https://mcp.example.com/s/8f14e45f-ceea-467a-9575-0b6b5c4e7a92/mcp"},
	"s3-query-id":          {"type": "http", "url": "https://mcp.example.com/mcp?customerId=8f14e45f-ceea-467a-9575-0b6b5c4e7a93"},
	"s4-short-flag":        {"command": "pkg", "args": []any{"-k", "shortsecretvalue94"}},
	"s5-bearer-arg":        {"command": "npx", "args": []any{"mcp-remote", "https://x.example/mcp", "--header", "Authorization: Bearer abcdefgh12345695"}},
	"s7-env-literal":       {"command": "pkg", "env": map[string]any{"SERVICE_TOKEN": "literalvalue97xyz"}},
	"s8-lower-hex":         {"command": "pkg", "args": []any{"sbp_0123456789abcdef0123456789abcdef01234598"}},
	"s9-command":           {"command": "env", "args": []any{"TOKEN=plainsecret99", "node", "server.js"}},
	"s10-hdr-literal":      {"type": "http", "url": "https://h.example/mcp", "headers": map[string]any{"X-Api-Key": "hdrliteral1010"}},
	"s11-tmpl-mixed":       {"type": "http", "url": "https://t.example/mcp", "headers": map[string]any{"Authorization": "Basic ${USERNAME}:mixedliteral1111"}},
	"s13-userinfo-arg":     {"command": "npx", "args": []any{"mcp-remote", "https://user:pw1313xx@r.example/mcp"}},
	"s14-path-token-mixed": {"type": "http", "url": "https://mcp.zap.example/api/mcp/s/AbCdEfGh1234IjKlMnOp5678QrStUv90/mcp"},
	"s15-equals-short":     {"command": "pkg", "args": []any{"--key=k1515value"}},
	"s16-fragment":         {"type": "http", "url": "https://f.example/mcp#tok=frag1616value"},
	"s17-streamable":       {"type": "http", "url": "https://ok.example/mcp"},
	"s18-bearer-ref":       {"type": "http", "url": "https://b.example/mcp", "headers": map[string]any{"Authorization": "Bearer ${B_TOKEN}"}},
	"s19-cmd-space":        {"command": "TOKEN=x node", "args": []any{"server.js"}},
}

var verifySecrets = []string{"8f14e45f-ceea-467a-9575-0b6b5c4e7a91", "8f14e45f-ceea-467a-9575-0b6b5c4e7a92", "8f14e45f-ceea-467a-9575-0b6b5c4e7a93",
	"shortsecretvalue94", "abcdefgh12345695", "literalvalue97xyz", "sbp_0123456789abcdef0123456789abcdef01234598", "plainsecret99",
	"hdrliteral1010", "mixedliteral1111", "pw1313xx", "AbCdEfGh1234IjKlMnOp5678QrStUv90", "k1515value", "frag1616value"}

// configText is what config.yaml would hold for a server.
func configText(m *MCPServer) string {
	return fmt.Sprint(m.Command, m.Args, m.URL, m.Env, m.Headers)
}

func TestReadMCPKeyed(t *testing.T) {
	f := &Found{}
	plain := map[string]bool{"s7-env-literal": true, "s17-streamable": true, "s18-bearer-ref": true}
	got := map[string]*MCPServer{}
	for _, raw := range sortedKeys(verifyServers) {
		if s := f.readMCP(raw, verifyServers[raw], map[string]string{"USERNAME": "sam"}, map[string]bool{}); s != nil {
			got[raw] = s
			if keyed := len(s.Keyed) > 0; keyed == plain[raw] {
				t.Errorf("%s: keyed %v (%v)", raw, keyed, s.Keyed)
			}
			for _, v := range verifySecrets {
				if strings.Contains(configText(s), v) {
					t.Errorf("%s: config.yaml would hold %s: %s", raw, v, configText(s))
				}
			}
		}
	}
	if got["s19-cmd-space"] != nil {
		t.Error("a command that is not a plain program was imported")
	}
	if s := got["s4-short-flag"]; s == nil || !slices.Equal(s.Args, []string{"-k", "${MCP_S4_SHORT_FLAG_ARG2}"}) || f.Secrets["MCP_S4_SHORT_FLAG_ARG2"] != "shortsecretvalue94" {
		t.Errorf("s4 = %+v", s)
	}
	if s := got["s2-uuid-path"]; s == nil || s.URL != "https://mcp.example.com${MCP_S2_UUID_PATH_URL}" {
		t.Errorf("s2 = %+v", s)
	}
	if s := got["s10-hdr-literal"]; s == nil || s.Headers["X-Api-Key"] != "MCP_S10_HDR_LITERAL_HEADER_1" || f.Secrets["MCP_S10_HDR_LITERAL_HEADER_1"] != "hdrliteral1010" {
		t.Errorf("s10 = %+v", s)
	}
	if v := f.Secrets["MCP_S11_TMPL_MIXED_HEADER_1"]; v != "Basic sam:mixedliteral1111" {
		t.Errorf("s11 value = %q; its reference is filled from the source", v)
	}
	if s := got["s18-bearer-ref"]; s == nil || s.Headers["Authorization"] != "Bearer ${B_TOKEN}" {
		t.Errorf("s18 = %+v", s)
	}
}

// From an agent's home, a keyed server comes along only with the keys; its
// values go to .env and config.yaml names them. Without the keys it is
// left out with a hint, and its values appear nowhere.
func TestKeyedServerNeedsKeys(t *testing.T) {
	ctx := context.Background()
	src := fixture(t, Hermes)
	path := filepath.Join(src, "config.yaml")
	cfg := readFile(t, path)
	cfg = strings.Replace(cfg, "mcp_servers:\n", "mcp_servers:\n  shortflag:\n    command: pkg\n    args: [\"-k\", \"shortsecretvalue94\"]\n  zap:\n    url: https://mcp.zap.example/api/mcp/s/AbCdEfGh1234IjKlMnOp5678QrStUv90/mcp\n", 1)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"skip", "copy"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			o := options(Hermes, src, false)
			o.Secrets = mode
			f, err := Read(o)
			if err != nil {
				t.Fatal(err)
			}
			p, err := NewPlan(f, home, o)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			p.Write(&out, mode)
			if _, err := p.Apply(ctx, mode == "copy"); err != nil {
				t.Fatal(err)
			}
			conf := readFile(t, filepath.Join(home, "config.yaml"))
			for _, v := range verifySecrets {
				if strings.Contains(out.String(), v) || strings.Contains(conf, v) {
					t.Errorf("%s is in the plan or config.yaml", v)
				}
			}
			env, _ := os.ReadFile(filepath.Join(home, ".env"))
			if mode == "skip" {
				if it := item(p, "mcp", "shortflag"); it == nil || it.Status != Rejected || !strings.Contains(it.Detail, "hk mcp add shortflag") {
					t.Errorf("shortflag = %+v", it)
				}
				if strings.Contains(conf, "shortflag") || strings.Contains(conf, "mcp:shortflag") || strings.Contains(string(env), "shortsecretvalue94") {
					t.Errorf("a keyed server or its key was written:\n%s", conf)
				}
				return
			}
			if !strings.Contains(conf, "${MCP_SHORTFLAG_ARG2}") || !strings.Contains(conf, "https://mcp.zap.example${MCP_ZAP_URL}") {
				t.Errorf("config.yaml:\n%s", conf)
			}
			if !strings.Contains(string(env), "MCP_SHORTFLAG_ARG2=shortsecretvalue94") || !strings.Contains(string(env), "MCP_ZAP_URL=/api/mcp/s/AbCdEfGh1234IjKlMnOp5678QrStUv90/mcp") {
				t.Errorf(".env lacks the keyed values")
			}
		})
	}
}

// A skill kept elsewhere and linked into the skills folder comes along,
// whether the link is in the skills folder or a category folder, or links
// a whole category; a link that leads nowhere is named.
func TestSymlinkedSkill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need extra rights on Windows")
	}
	src := fixture(t, ClaudeCode)
	elsewhere := t.TempDir()
	mk := func(name string) string {
		dir := filepath.Join(elsewhere, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+filepath.Base(name)+"\ndescription: x\n---\n\nRead it.\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	link := func(target, at string) {
		if err := os.MkdirAll(filepath.Dir(at), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, at); err != nil {
			t.Fatal(err)
		}
	}
	link(mk("price-list"), filepath.Join(src, "skills", "price-list"))
	link(mk("oven-log"), filepath.Join(src, "skills", "kitchen", "oven-log"))
	mk(filepath.Join("shared-category", "rota"))
	link(filepath.Join(elsewhere, "shared-category"), filepath.Join(src, "skills", "shared"))
	link(filepath.Join(elsewhere, "gone"), filepath.Join(src, "skills", "gone"))
	f, err := Read(options(ClaudeCode, src, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"price-list", "oven-log", "rota"} {
		if !slices.Contains(skillNames(f.Agents[0]), n) {
			t.Errorf("skills = %v; want the linked %s", skillNames(f.Agents[0]), n)
		}
	}
	if !strings.Contains(strings.Join(f.Skipped, "\n"), "skill link "+filepath.Join(src, "skills", "gone")+": it leads nowhere") {
		t.Errorf("skipped = %q; want the broken link named", f.Skipped)
	}
}

func TestCodingToolInternals(t *testing.T) {
	for _, c := range []struct{ from, name string }{
		{ClaudeCode, "backups"}, {ClaudeCode, ".credentials.json"}, {Codex, "state_5.sqlite"}, {Codex, "memories_1.sqlite"},
		{Codex, "browser"}, {Codex, "installation_id"}, {Codex, "auth.json"}, {Codex, "chrome-browser-profile"}, {Codex, "logs_2.db"},
	} {
		if why := internalWhy(c.from, c.name); !strings.Contains(why, "keys and sign-ins") {
			t.Errorf("%s %s: %q; want it kept as key material", c.from, c.name, why)
		}
	}
	for _, c := range []struct{ from, name string }{{Codex, "sqlite"}, {Codex, "session_index.jsonl"}, {Codex, "plugins"},
		{Codex, "computer-use"}, {Codex, "node_repl"}, {Codex, "state"}, {Codex, "process_manager"}, {Codex, "ipc"}} {
		if why := internalWhy(c.from, c.name); why == "" {
			t.Errorf("%s %s may be copied", c.from, c.name)
		}
	}
	if why := internalWhy(ClaudeCode, "scripts"); why != "" {
		t.Errorf("scripts: %q; the user's own folder may move", why)
	}
}
