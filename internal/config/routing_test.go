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

func TestJudgeConfig(t *testing.T) {
	cfg, err := parseAgent(t, "judge: {}\n")
	if err != nil {
		t.Fatal(err)
	}
	j := cfg.Judge
	if j.Model != "jev-latest" || j.KeyEnv != "TYPESAFE_API_KEY" || j.TimeoutMS != 800 || !*j.Routing || !*j.Approvals || !*j.Memory || j.ApproveAbove != 0.97 {
		t.Fatalf("defaults: %+v", j)
	}
	cfg, err = parseAgent(t, "judge: {approvals: false}\n")
	if err != nil || *cfg.Judge.Approvals {
		t.Fatalf("approvals off: %v %+v", err, cfg.Judge)
	}
	for agent, want := range map[string]string{
		"judge: {approve_above: 0.5}\n":      "approve_above",
		"judge: {timeout_ms: 20}\n":          "timeout_ms",
		"judge: {skip_memory_below: 0.9}\n":  "skip_memory_below",
		"judge: {key_env: \"not a name\"}\n": "key_env",
	} {
		if _, err := parseAgent(t, agent); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err %v, want %q", agent, err, want)
		}
	}
}
