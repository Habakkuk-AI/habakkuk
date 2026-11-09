package config

import (
	"strings"
	"testing"
	"time"
)

func TestTemplateIsValid(t *testing.T) {
	cfg, err := Parse([]byte(Template))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agents["habakkuk"].Permission != "workspace" || cfg.Retention.Days != 90 {
		t.Fatalf("defaults not applied: %+v", cfg.Agents["habakkuk"])
	}
}

func TestUnknownKeyIsAnError(t *testing.T) {
	_, err := Parse([]byte(strings.Replace(Template, "effort: high", "efort: high", 1)))
	if err == nil || !strings.Contains(err.Error(), "efort") {
		t.Fatalf("misspelled key accepted: %v", err)
	}
}

func TestUnknownProviderInModel(t *testing.T) {
	_, err := Parse([]byte(strings.Replace(Template, "model: anthropic/", "model: nope/", 1)))
	if err == nil || !strings.Contains(err.Error(), `unknown provider "nope"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestChannelValidation(t *testing.T) {
	base := strings.Replace(Template, "\nworkers:", `
channels:
  tg:
    type: telegram
    token_env: TG
    routes: {"-100": ghost}
  sl:
    type: slack
    app_token_env: APP
  wa:
    type: whatsapp
    groups: sometimes
  dc:
    type: discord
  sg:
    type: signal
    account: "15551234567"
  mx:
    type: matrix
    homeserver: matrix.example.org
    token_env: MX
    account: habakkuk

workers:`, 1)
	_, err := Parse([]byte(base))
	if err == nil {
		t.Fatal("bad channels accepted")
	}
	for _, want := range []string{`route -100: agent "ghost"`, "slack needs app_token_env and bot_tokens_env", `groups "sometimes"`,
		"token_env is required for discord", "signal needs account", "matrix needs homeserver", `account "habakkuk" must be a Matrix user id`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
	good := strings.Replace(Template, "\nworkers:", `
channels:
  sl:
    type: slack
    app_token_env: APP
    bot_tokens_env: [B1, B2]
    routes: {TB: habakkuk}
  dc:
    type: discord
    token_env: DISCORD_BOT_TOKEN
  sg:
    type: signal
    account: "+15551234567"
  mx:
    type: matrix
    homeserver: https://matrix.example.org
    token_env: MATRIX_ACCESS_TOKEN
    account: "@habakkuk:example.org"
workers:`, 1)
	cfg, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	ch := cfg.Channels["sl"]
	if ch.Agent != "habakkuk" || ch.Groups != "mention" || ch.SessionIdle != 24*time.Hour || ch.AgentFor("TB/C1") != "habakkuk" {
		t.Fatalf("defaults: %+v", ch)
	}
}

func TestMCPValidation(t *testing.T) {
	withMCP := func(toolsets, servers string) string {
		s := strings.Replace(Template, "    workspace: ~/habakkuk-workspace\n",
			"    workspace: ~/habakkuk-workspace\n    toolsets: "+toolsets+"\n", 1)
		return strings.Replace(s, "\nworkers:", "\nmcp_servers:"+servers+"\nworkers:", 1)
	}
	good := withMCP(`[core, "mcp:gh", "mcp:search"]`, `
  gh:
    command: github-mcp-server
    args: [stdio]
    env: {GITHUB_PERSONAL_ACCESS_TOKEN: GITHUB_TOKEN}
    approve: [create_pull_request]
  search:
    url: https://mcp.example.com/mcp
    headers: {Authorization: SEARCH_AUTH}
    risk: read
    timeout: 30s
`)
	cfg, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if gh := cfg.MCP["gh"]; gh.Risk != "network" || gh.Timeout != 120*time.Second {
		t.Fatalf("defaults not applied: %+v", gh)
	}
	if s := cfg.MCP["search"]; s.Risk != "read" || s.Timeout != 30*time.Second {
		t.Fatalf("explicit values lost: %+v", s)
	}

	bad := withMCP(`[core, "mcp:ghost"]`, `
  both:
    command: x
    url: https://x
  neither:
    risk: read
  risky:
    command: x
    risk: dangerous
  hdr:
    command: x
    headers: {A: B}
  scheme:
    url: ftp://x
  "bad.name":
    command: x
  blank:
    command: x
    env: {TOKEN: ""}
`)
	_, err = Parse([]byte(bad))
	if err == nil {
		t.Fatal("bad mcp servers accepted")
	}
	for _, want := range []string{
		`toolset "mcp:ghost" names no configured MCP server`,
		"mcp server both: set exactly one of command or url",
		"mcp server neither: set exactly one of command or url",
		`mcp server risky: risk "dangerous"`,
		"mcp server hdr: headers apply to url servers only",
		`mcp server scheme: url "ftp://x"`,
		"mcp server bad.name: name must be",
		`mcp server blank: env "TOKEN"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
}

func TestDefaultToolsets(t *testing.T) {
	cfg, err := Parse([]byte(Template))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Agents["habakkuk"].Toolsets, ","); got != "core,memory,skills,schedule,delegate,board" {
		t.Fatalf("default toolsets = %s", got)
	}
	// A config that lists toolsets keeps exactly its list.
	cfg, err = Parse([]byte(strings.Replace(Template, "effort: high", "effort: high\n    toolsets: [core]", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Agents["habakkuk"].Toolsets, ","); got != "core" {
		t.Fatalf("explicit toolsets = %s", got)
	}
}

func TestCompactAt(t *testing.T) {
	parse := func(model, extra string) (*Agent, error) {
		cfg, err := Parse([]byte(strings.Replace(Template, "model: anthropic/claude-opus-5-5", "model: "+model+extra, 1)))
		if err != nil {
			return nil, err
		}
		return cfg.Agents["habakkuk"], nil
	}
	for model, want := range map[string]int{"anthropic/claude-opus-5-5": 600000, "openrouter/some-model": 150000} {
		if a, err := parse(model, ""); err != nil || a.CompactAt != want {
			t.Errorf("%s: compact_at = %+v, %v; want %d", model, a, err, want)
		}
	}
	for extra, ok := range map[string]bool{
		"\n    compact_at: 200000": true, "\n    compact_at: -1": true,
		"\n    compact_at: 10": false, "\n    compact_at: -5": false,
	} {
		if _, err := parse("anthropic/claude-opus-5-5", extra); (err == nil) != ok {
			t.Errorf("%q: err = %v", extra, err)
		}
	}
}

func TestApprovals(t *testing.T) {
	cfg, err := Parse([]byte(Template))
	if err != nil {
		t.Fatal(err)
	}
	if a := cfg.Agents["habakkuk"]; a.Approvals != "human" || a.ReviewerModel != "" {
		t.Fatalf("approvals default = %q, reviewer_model = %q", a.Approvals, a.ReviewerModel)
	}
	with := func(lines string) string {
		return strings.Replace(Template, "effort: high", "effort: high\n"+lines, 1)
	}
	if _, err := Parse([]byte(with("    approvals: reviewer_then_human\n    reviewer_model: openrouter/judge"))); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		"    approvals: robot":              `approvals "robot"`,
		"    reviewer_model: nope/judge":    `unknown provider "nope"`,
		"    reviewer_model: no-slash-here": "want <provider>/<model>",
	} {
		if _, err := Parse([]byte(with(in))); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", in, err, want)
		}
	}
}
