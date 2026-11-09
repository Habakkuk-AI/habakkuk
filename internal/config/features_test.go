package config

import (
	"strings"
	"testing"
	"time"
)

func TestFeatureDefaults(t *testing.T) {
	cfg, err := Parse([]byte(mediaBase + `
channels:
  tg: {type: telegram, token_env: T, reactions: true}
video: {provider: gemini}
`))
	if err != nil {
		t.Fatal(err)
	}
	if v := cfg.Video; v.KeyEnv != "GEMINI_API_KEY" || v.Seconds != 8 || v.Timeout != 10*time.Minute {
		t.Fatalf("video: %+v", v)
	}
	if !cfg.Channels["tg"].Reactions {
		t.Fatal("reactions not read")
	}
	if a := cfg.Agents["habakkuk"]; a.ToolSearchOver != DefaultToolSearchOver || a.TitleModel != "" {
		t.Fatalf("agent: %+v", a)
	}
	cfg, err = Parse([]byte(strings.Replace(mediaBase, "{model: p/m}", "{model: p/m, tool_search_over: -1, title_model: p/small}", 1)))
	if err != nil || cfg.Agents["habakkuk"].ToolSearchOver != -1 || cfg.Agents["habakkuk"].TitleModel != "p/small" {
		t.Fatalf("explicit: %+v %v", cfg.Agents["habakkuk"], err)
	}
}

func TestFeatureValidation(t *testing.T) {
	for extra, want := range map[string]string{
		"video: {provider: runway}":              "video: provider",
		"video: {provider: openai, seconds: 99}": "seconds must be between",
	} {
		if _, err := Parse([]byte(mediaBase + extra + "\n")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", extra, want, err)
		}
	}
	for agent, want := range map[string]string{
		"{model: p/m, tool_search_over: -5}":  "tool_search_over",
		"{model: p/m, title_model: nope/x}":   "title_model",
		"{model: p/m, title_model: no-slash}": "title_model",
	} {
		_, err := Parse([]byte(strings.Replace(mediaBase, "{model: p/m}", agent, 1)))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", agent, want, err)
		}
	}
	if _, err := Parse([]byte(strings.Replace(mediaBase, "{model: p/m}", "{model: p/m, title_model: off}", 1))); err != nil {
		t.Errorf("title_model off: %v", err)
	}
}
