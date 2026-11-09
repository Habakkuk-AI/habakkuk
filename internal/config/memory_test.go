package config

import (
	"strings"
	"testing"
	"time"
)

const memoryBase = `version: 1
providers:
  p: {type: anthropic, keys_env: [K]}
  oa: {type: openai, keys_env: [OPENAI_API_KEY]}
  local: {preset: ollama}
  gem: {preset: gemini}
  chat: {auth: chatgpt}
agents:
  habakkuk: {model: p/m}
`

func TestMemoryDefaults(t *testing.T) {
	cfg, err := Parse([]byte(memoryBase + "memory:\n  review: true\n  embeddings: {provider: local, model: nomic-embed-text}\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Memory
	if !m.Review || m.ReviewIdle != 15*time.Minute || m.StaleDays != 365 || m.Embeddings.Model != "nomic-embed-text" {
		t.Fatalf("defaults: %+v", m)
	}
	if at, ok := m.NightlyAt(); !ok || at != 3*time.Hour+30*time.Minute {
		t.Fatalf("nightly %v %v", at, ok)
	}
	cfg, err = Parse([]byte(memoryBase + "memory: {nightly: \"off\", embeddings: {provider: gem, model: gemini-embedding-001, dimensions: 768}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Memory.NightlyAt(); ok {
		t.Fatal("nightly off is on")
	}
	if cfg, _ := Parse([]byte(memoryBase)); cfg.Memory != nil {
		t.Fatal("memory section without one in the file")
	}
}

func TestMemoryValidation(t *testing.T) {
	for section, want := range map[string]string{
		"memory: {embeddings: {provider: nope, model: x}}":               `provider "nope" is not configured`,
		"memory: {embeddings: {provider: p, model: x}}":                  "must be of type openai or gemini",
		"memory: {embeddings: {provider: chat, model: x}}":               "signs in with a login",
		"memory: {embeddings: {provider: oa}}":                           "model is required",
		"memory: {review: true, review_model: nope/x}":                   `unknown provider "nope"`,
		"memory: {review: true, review_model: justamodel}":               "want <provider>/<model>",
		"memory: {review: true, review_idle: 10s}":                       "review_idle must be at least 1m",
		"memory: {review: true, nightly: \"25:99\"}":                     "must be HH:MM or off",
		"memory: {stale_days: -5}":                                       "stale_days",
		"memory: {embeddings: {provider: oa, model: x, dimensions: -1}}": "dimensions must not be negative",
	} {
		_, err := Parse([]byte(memoryBase + section + "\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", section, err, want)
		}
	}
	if _, err := Parse([]byte(memoryBase + "memory: {review: true, review_model: oa/gpt-mini, stale_days: -1}\n")); err != nil {
		t.Fatal(err)
	}
}

func TestTemplateMemoryExampleParses(t *testing.T) {
	var lines []string
	in := false
	for _, l := range strings.Split(Template, "\n") {
		if strings.HasPrefix(l, "# memory:") {
			in = true
		} else if in && !strings.HasPrefix(l, "#   ") {
			break
		}
		if in {
			lines = append(lines, strings.TrimPrefix(l, "# "))
		}
	}
	if len(lines) < 3 {
		t.Fatal("no memory example in the template")
	}
	doc := strings.Replace(Template, "\nworkers:", "\n"+strings.Join(lines, "\n")+"\n\nworkers:", 1)
	doc = strings.Replace(doc, "providers:\n", "providers:\n  openai: {type: openai, keys_env: [OPENAI_API_KEY]}\n", 1)
	if _, err := Parse([]byte(doc)); err != nil {
		t.Fatalf("template example: %v", err)
	}
}
