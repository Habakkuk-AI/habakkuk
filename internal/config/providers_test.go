package config

import (
	"strings"
	"testing"
)

func parseProviders(t *testing.T, providers string) (*Config, error) {
	t.Helper()
	return Parse([]byte("version: 1\nproviders:\n" + providers + `
agents:
  habakkuk:
    model: p/m
`))
}

func TestPresetResolution(t *testing.T) {
	cfg, err := parseProviders(t, `
  p:
    preset: groq
  groq: {}
  ollama: {}
  mine:
    preset: openrouter
    keys_env: [MY_KEY]
    base_url: https://proxy.example/v1
`)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]struct{ typ, base, key string }{
		"p":      {TypeOpenAI, "https://api.groq.com/openai/v1", "GROQ_API_KEY"},
		"groq":   {TypeOpenAI, "https://api.groq.com/openai/v1", "GROQ_API_KEY"},
		"ollama": {TypeOpenAI, "http://localhost:11434/v1", "OLLAMA_API_KEY"},
		"mine":   {TypeOpenAI, "https://proxy.example/v1", "MY_KEY"},
	} {
		p := cfg.Providers[name]
		if p.Type != want.typ || p.BaseURL != want.base || len(p.KeysEnv) != 1 || p.KeysEnv[0] != want.key {
			t.Errorf("%s = %+v, want %+v", name, p, want)
		}
	}
	if !cfg.Providers["ollama"].KeyOptional() || cfg.Providers["groq"].KeyOptional() {
		t.Error("ollama should run without a key and groq should not")
	}
}

func TestEveryPresetIsComplete(t *testing.T) {
	for _, name := range PresetNames() {
		pr := Presets[name]
		switch pr.Type {
		case TypeAnthropic, TypeOpenAI, TypeGemini, TypeBedrock:
		default:
			t.Errorf("%s: type %q", name, pr.Type)
		}
		if pr.Type == TypeOpenAI && pr.BaseURL == "" && !pr.NeedsBaseURL {
			t.Errorf("%s: openai preset without a base URL", name)
		}
		if pr.KeyEnv == "" && !pr.KeyOptional {
			t.Errorf("%s: needs a key but names no variable", name)
		}
	}
	for _, want := range []string{"openrouter", "groq", "together", "fireworks", "deepseek", "xai", "mistral", "cerebras",
		"deepinfra", "nvidia", "huggingface", "perplexity", "moonshot", "zai", "minimax", "qwen", "dashscope", "ollama",
		"lmstudio", "vllm", "llamacpp", "litellm", "vercel", "cloudflare"} {
		if _, ok := Presets[want]; !ok {
			t.Errorf("missing preset %s", want)
		}
	}
}

func TestProviderValidation(t *testing.T) {
	for _, tc := range []struct{ providers, want string }{
		{"  p:\n    preset: nosuch\n", `unknown preset "nosuch"`},
		{"  p:\n    type: openai\n", "keys_env is empty"},
		{"  p:\n    type: cohere\n    keys_env: [K]\n", `type "cohere"`},
		{"  p: {}\n", "set type"},
		{"  p:\n    preset: cloudflare\n", "base_url is required"},
		{"  p:\n    auth: chatgpt\n    keys_env: [K]\n", "either auth or keys_env"},
		{"  p:\n    type: anthropic\n    auth: claude-token\n", "Anthropic's terms"},
		{"  p:\n    type: anthropic\n    auth: chatgpt\n", "auth chatgpt needs type responses"},
		{"  p:\n    type: openai\n    keys_env: [K]\n    region: us-east-1\n", "region applies to bedrock only"},
	} {
		_, err := parseProviders(t, tc.providers)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want %q", tc.providers, err, tc.want)
		}
	}
}

func TestLoginsAndKeylessProviders(t *testing.T) {
	cfg, err := parseProviders(t, `
  p:
    auth: chatgpt
  copilot:
    auth: copilot
  aws:
    type: bedrock
    region: eu-west-1
  vertex:
    type: gemini
    keys_env: [GEMINI_API_KEY]
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["p"].Type != TypeResponses || cfg.Providers["copilot"].Type != TypeOpenAI {
		t.Fatalf("auth did not imply a type: %+v %+v", cfg.Providers["p"], cfg.Providers["copilot"])
	}
	if len(cfg.Providers["copilot"].KeysEnv) != 0 {
		t.Fatal("a login must not get a preset's key variable")
	}
}

func TestResolveModel(t *testing.T) {
	cfg, err := parseProviders(t, "  p:\n    preset: groq\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.ResolveModel("p/llama-4"); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.ResolveModel("q/llama-4"); err == nil {
		t.Fatal("unknown provider accepted")
	}
	if _, err := cfg.ResolveModel("llama-4"); err == nil {
		t.Fatal("missing provider accepted")
	}
}
