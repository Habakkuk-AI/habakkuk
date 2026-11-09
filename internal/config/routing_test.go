package config

import (
	"strings"
	"testing"
)

func parseAgent(t *testing.T, agent string) (*Config, error) {
	t.Helper()
	return Parse([]byte(`version: 1
providers:
  anthropic: {}
  deepseek: {}
agents:
  habakkuk:
    model: anthropic/claude-opus-5-5
` + agent))
}

func TestFastModelConfig(t *testing.T) {
	cfg, err := parseAgent(t, "    fast_model: deepseek/deepseek-flash\n")
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Agents["habakkuk"]
	if a.Routing != "auto" || a.EscalateAfter != 4 {
		t.Fatalf("defaults: routing %q escalate_after %d", a.Routing, a.EscalateAfter)
	}

	cfg, err = parseAgent(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if a := cfg.Agents["habakkuk"]; a.Routing != "" {
		t.Fatalf("no fast model, routing %q", a.Routing)
	}

	for agent, want := range map[string]string{
		"    fast_model: nowhere/x\n":                          `unknown provider "nowhere"`,
		"    fast_model: flash\n":                              "habakkuk",
		"    routing: auto\n":                                  "needs a fast_model",
		"    fast_model: deepseek/f\n    routing: cheap\n":     "must be auto, fast or smart",
		"    fast_model: deepseek/f\n    escalate_after: -1\n": "at least 1",
	} {
		if _, err := parseAgent(t, agent); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err %v, want %q", agent, err, want)
		}
	}
}
