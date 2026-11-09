package app

import (
	"slices"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
)

func TestComputerTools(t *testing.T) {
	cfg, err := config.Parse([]byte(`version: 1
providers:
  anthropic: {type: anthropic, keys_env: [ANTHROPIC_API_KEY]}
  openai: {type: openai, keys_env: [OPENAI_API_KEY]}
agents:
  native: {model: anthropic/claude-opus-5-5, toolsets: [core, computer]}
  older: {model: anthropic/claude-opus-4-7, toolsets: [core, computer]}
  mixed: {model: anthropic/claude-opus-5-5, fallbacks: [openai/gpt-5], toolsets: [core, computer]}
  plain: {model: anthropic/claude-opus-5-5}
computer: {backend: xvfb, screen: 1024x768, max_actions: 40, confirm_apps: [Terminal]}
`))
	if err != nil {
		t.Fatal(err)
	}
	spec := func(name string) *agent.Spec {
		ac := cfg.Agents[name]
		s := &agent.Spec{Toolsets: ac.Toolsets, ServerTools: []string{"web_search"}}
		computerTools(cfg, ac, s)
		return s
	}
	if s := spec("native"); !slices.Equal(s.Toolsets, []string{"core", "computer-open"}) || !slices.Equal(s.ServerTools, []string{"web_search", "computer"}) {
		t.Errorf("native: %v %v", s.Toolsets, s.ServerTools)
	}
	for _, name := range []string{"older", "mixed"} {
		if s := spec(name); !slices.Equal(s.Toolsets, []string{"core", "computer", "computer-open"}) || !slices.Equal(s.ServerTools, []string{"web_search"}) {
			t.Errorf("%s: %v %v", name, s.Toolsets, s.ServerTools)
		}
	}
	if s := spec("plain"); slices.Contains(s.Toolsets, "computer-open") || len(s.ServerTools) != 1 {
		t.Errorf("plain: %v %v", s.Toolsets, s.ServerTools)
	}
	// The config's own toolsets are left alone.
	if !slices.Equal(cfg.Agents["native"].Toolsets, []string{"core", "computer"}) {
		t.Errorf("config changed: %v", cfg.Agents["native"].Toolsets)
	}
	off := false
	cfg.Computer.Native = &off
	if s := spec("native"); !slices.Contains(s.Toolsets, "computer") || len(s.ServerTools) != 1 {
		t.Errorf("native off: %v %v", s.Toolsets, s.ServerTools)
	}
}

func TestComputerConfig(t *testing.T) {
	base := "version: 1\nproviders: {a: {type: anthropic, keys_env: [K]}}\nagents: {f: {model: a/m}}\n"
	cfg, err := config.Parse([]byte(base + "computer: {backend: xvfb}\n"))
	if err != nil {
		t.Fatal(err)
	}
	k := cfg.ComputerConfig()
	if k.Display != ":99" || k.Screen != "1280x800" || k.MaxActions != 150 || !*k.Native || k.Timeout == 0 {
		t.Errorf("defaults: %+v", k)
	}
	for _, bad := range []string{
		"computer: {backend: windows}\n",
		"computer: {backend: xvfb, screen: big}\n",
		"computer: {backend: linux, screen: 1280x800}\n",
		"computer: {backend: macos, display: ':1'}\n",
		"computer: {backend: linux, display: 'nope'}\n",
		"computer: {max_actions: -2}\n",
		"computer: {confirm_apps: ['']}\n",
		"computer: {unknown: 1}\n",
	} {
		if _, err := config.Parse([]byte(base + bad)); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}
