package config

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const pluginYAML = `name: tracker
description: Issue tracker tools
mcp_servers:
  tracker:
    command: ./bin/tracker-mcp
    args: ["--root", "${PLUGIN_DIR}/data"]
    env: {TRACKER_TOKEN: TRACKER_TOKEN}
hooks:
  - event: turn_end
    command: ./hooks/log.sh
commands:
  standup:
    description: Write my standup
    prompt: "Write my standup for {{args}}"
  sync:
    run: ./sync.sh
`

func writePlugin(t *testing.T, home, name, manifest string, enabled, trusted bool) string {
	t.Helper()
	dir := filepath.Join(home, "plugins", name)
	os.MkdirAll(filepath.Join(dir, "skills", "triage"), 0o755)
	os.WriteFile(filepath.Join(dir, PluginManifestName), []byte(manifest), 0o644)
	lock := `{"version":1,"packages":{"` + name + `":{"source":"x","sha256":"y","enabled":` +
		map[bool]string{true: "true", false: "false"}[enabled] + `,"trusted":` + map[bool]string{true: "true", false: "false"}[trusted] + `}}}`
	os.WriteFile(filepath.Join(home, "plugins", ".lock.json"), []byte(lock), 0o600)
	return dir
}

func TestPluginsMergeAtLoad(t *testing.T) {
	home := t.TempDir()
	cfgText := strings.Replace(Template, "# Messaging channels.", `  coder:
    model: anthropic/claude-opus-5-5
    toolsets: [core, "mcp:tracker"]

# Messaging channels.`, 1)
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfgText), 0o600)

	// Not trusted: not loaded, and the explicit toolset is an error.
	dir := writePlugin(t, home, "tracker", pluginYAML, true, false)
	if _, err := Load(home); err == nil || !strings.Contains(err.Error(), `"mcp:tracker" names no configured MCP server`) {
		t.Fatalf("untrusted plugin loaded: %v", err)
	}
	writePlugin(t, home, "tracker", pluginYAML, true, true)
	cfg, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.MCP["tracker"]
	if s == nil || s.Command != filepath.Join(dir, "bin/tracker-mcp") || s.Args[1] != dir+"/data" || s.Risk != "network" {
		t.Fatalf("server %+v", s)
	}
	if !slices.Contains(cfg.Agents["habakkuk"].Toolsets, "mcp:tracker") || !slices.Contains(cfg.Agents["habakkuk"].Toolsets, "memory") {
		t.Fatalf("default agent toolsets %v", cfg.Agents["habakkuk"].Toolsets)
	}
	if got := cfg.Agents["coder"].Toolsets; len(got) != 2 {
		t.Fatalf("explicit toolsets changed: %v", got)
	}
	// On Windows the hook runs through PowerShell, so only its count is the same.
	if len(cfg.Hooks) != 1 || (runtime.GOOS != "windows" && !strings.HasPrefix(cfg.Hooks[0].Command, "cd '"+dir+"' && ./hooks/log.sh")) {
		t.Fatalf("hooks %+v", cfg.Hooks)
	}
	if len(cfg.Plugins) != 1 || cfg.Plugins[0].SkillsDir != filepath.Join(dir, "skills") || len(cfg.Plugins[0].Manifest.Commands) != 2 {
		t.Fatalf("plugins %+v", cfg.Plugins)
	}

	// A broken manifest is reported, not fatal.
	writePlugin(t, home, "tracker", "name: tracker\nbogus: 1\n", true, true)
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte(Template), 0o600)
	cfg, err = Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.PluginProblems(); len(p) != 1 || !strings.Contains(p[0], "bogus") {
		t.Fatalf("problems %v", p)
	}
}

func TestPluginManifestValidation(t *testing.T) {
	for _, bad := range []string{
		"name: Bad Name\n",
		"name: x\nmcp_servers:\n  a: {command: x, url: https://y}\n",
		"name: x\ncommands:\n  c: {prompt: a, run: b}\n",
		"name: x\ncommands:\n  C!: {prompt: a}\n",
		"name: x\nhooks:\n  - {event: nope, command: x}\n",
		"name: x\nskills: ../elsewhere\n",
	} {
		if _, err := ParsePluginManifest([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	m, err := ParsePluginManifest([]byte(pluginYAML))
	if err != nil || m.MCPServers["tracker"].Timeout == 0 {
		t.Fatalf("%+v %v", m, err)
	}
	if PluginServerName("p", "a", 2) != "p-a" || PluginServerName("p", "a", 1) != "p" || PluginServerName("p", "p", 3) != "p" {
		t.Fatal("server names")
	}
}

func TestInsertMCPServer(t *testing.T) {
	s := &MCPServer{Command: "npx", Args: []string{"-y", "@scope/server"}, Env: map[string]string{"API_KEY": "API_KEY"}}
	// No active section: appended, comments untouched.
	out, err := InsertMCPServer([]byte(Template), "thing", s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), strings.TrimRight(Template, "\n")) {
		t.Fatal("the existing text changed")
	}
	cfg, err := Parse(out)
	if err != nil || cfg.MCP["thing"] == nil || cfg.MCP["thing"].Args[1] != "@scope/server" {
		t.Fatalf("%v %+v", err, cfg)
	}

	// A block section: the entry goes after its last entry, before the
	// comment that opens the next section.
	text := strings.Replace(Template, "# Hooks run a command", `mcp_servers:
    first:
        url: https://mcp.example.com/mcp   # keep me
        # a note inside

# Hooks run a command`, 1)
	out, err = InsertMCPServer([]byte(text), "second", &MCPServer{URL: "https://two.example.com/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "        # a note inside\n    second:\n        url: https://two.example.com/mcp\n\n# Hooks run a command") ||
		!strings.Contains(got, "# keep me") {
		t.Fatalf("block insert:\n%s", got[strings.Index(got, "mcp_servers:"):][:300])
	}
	if cfg, err := Parse(out); err != nil || len(cfg.MCP) != 2 {
		t.Fatalf("%v", err)
	}
	if _, err := InsertMCPServer(out, "second", s); err == nil {
		t.Fatal("duplicate accepted")
	}

	// Flow style goes through the node tree.
	text = strings.Replace(Template, "\nworkers:", "\nmcp_servers: {}\n\nworkers:", 1)
	out, err = InsertMCPServer([]byte(text), "thing", s)
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err := Parse(out); err != nil || cfg.MCP["thing"] == nil {
		t.Fatalf("flow: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "# Each provider names the environment variables") {
		t.Fatal("comments lost")
	}
}

func TestAddMCPServerWritesBackup(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte(Template), 0o600)
	if err := AddMCPServer(home, "x", &MCPServer{URL: "https://x.example.com/mcp"}); err != nil {
		t.Fatal(err)
	}
	if bak, _ := os.ReadFile(filepath.Join(home, "config.yaml.bak")); string(bak) != Template {
		t.Fatal("no backup")
	}
	if err := AddMCPServer(home, "y", &MCPServer{URL: "ftp://bad"}); err == nil {
		t.Fatal("invalid server written")
	}
	if cfg, err := Load(home); err != nil || cfg.MCP["x"] == nil || cfg.MCP["y"] != nil {
		t.Fatalf("%v", err)
	}
}

func TestSkillsSection(t *testing.T) {
	text := strings.Replace(Template, "\nworkers:", `
skills:
  registries:
    - {name: team, url: "https://skills.example.com/index.json"}
  curate: weekly
workers:`, 1)
	cfg, err := Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	// The public directory comes first, then the configured registries.
	if regs := cfg.SkillRegistries(); len(regs) != 2 || regs[0].Name != "default" || regs[0].URL != DefaultRegistryURL || regs[1].Name != "team" {
		t.Fatalf("registries %+v", regs)
	}
	if !registryURL(DefaultRegistryURL) {
		t.Fatalf("the built-in registry %q is not an https URL", DefaultRegistryURL)
	}
	if cfg.CurateEvery() == 0 || cfg.ArchiveAfter() == 0 {
		t.Fatal("curate defaults")
	}
	if !cfg.BundledSkills() || len(cfg.DisabledSkills()) != 0 {
		t.Fatal("bundled skills are on and nothing is turned off by default")
	}
	// "off" removes the public directory, a URL replaces it, and skills can
	// be turned off by name or all the bundled ones at once.
	with := func(section string) *Config {
		t.Helper()
		cfg, err := Parse([]byte(strings.Replace(Template, "\nworkers:", "\nskills:\n  "+section+"\nworkers:", 1)))
		if err != nil {
			t.Fatalf("%s: %v", section, err)
		}
		return cfg
	}
	if regs := with(`default_registry: "off"`).SkillRegistries(); len(regs) != 0 {
		t.Fatalf("off left %+v", regs)
	}
	if regs := with(`default_registry: "https://skills.example.com/all.json"`).SkillRegistries(); len(regs) != 1 || regs[0].URL != "https://skills.example.com/all.json" {
		t.Fatalf("replaced: %+v", regs)
	}
	if cfg := with("bundled: false\n  disabled: [daily-brief, code-review]"); cfg.BundledSkills() || strings.Join(cfg.DisabledSkills(), ",") != "daily-brief,code-review" {
		t.Fatalf("bundled %v disabled %v", cfg.BundledSkills(), cfg.DisabledSkills())
	}
	for _, bad := range []string{"curate: hourly", "registries: [{name: team, url: 'http://example.com/i.json'}]", "default_registry: nope", "disabled: [Not_A_Name]", "registries: [{name: default, url: 'https://example.com/i.json'}]"} {
		if _, err := Parse([]byte(strings.Replace(Template, "\nworkers:", "\nskills:\n  "+bad+"\nworkers:", 1))); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
