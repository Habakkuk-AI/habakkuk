package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
)

// Provider types an endpoint can speak.
const (
	TypeAnthropic = "anthropic" // Claude Messages API
	TypeOpenAI    = "openai"    // any OpenAI-compatible Chat Completions API
	TypeResponses = "responses" // OpenAI Responses API
	TypeGemini    = "gemini"    // Google Gemini generateContent API
	TypeBedrock   = "bedrock"   // AWS Bedrock Converse API
)

// Logins a provider can use instead of API keys (`habakkuk auth login NAME`).
const (
	AuthChatGPT = "chatgpt" // ChatGPT subscription, Responses API
	AuthCopilot = "copilot" // GitHub Copilot subscription, OpenAI-compatible API
)

// Preset fills in a well-known provider's type, base URL and key variable,
// so a config can say `preset: groq` (or name the provider groq) and nothing
// else.
type Preset struct {
	Name    string
	Type    string
	BaseURL string
	KeyEnv  string // default keys_env; "" for local servers
	// KeyOptional means the server works without a key (local servers); a
	// key is sent only when the variable is set.
	KeyOptional bool
	// NeedsBaseURL means the URL is account-specific and must be given.
	NeedsBaseURL bool
	Note         string
}

// Presets is the table of known providers, by name. Aliases share an entry.
var Presets = map[string]Preset{}

func init() {
	for _, p := range []struct {
		names []string
		Preset
	}{
		{[]string{"anthropic"}, Preset{Type: TypeAnthropic, KeyEnv: "ANTHROPIC_API_KEY"}},
		{[]string{"openai"}, Preset{Type: TypeOpenAI, BaseURL: "https://api.openai.com/v1", KeyEnv: "OPENAI_API_KEY"}},
		{[]string{"gemini", "google"}, Preset{Type: TypeGemini, KeyEnv: "GEMINI_API_KEY"}},
		{[]string{"bedrock"}, Preset{Type: TypeBedrock, KeyOptional: true, Note: "AWS credential chain; region from base_url or AWS_REGION"}},
		{[]string{"openrouter"}, Preset{Type: TypeOpenAI, BaseURL: "https://openrouter.ai/api/v1", KeyEnv: "OPENROUTER_API_KEY"}},
		{[]string{"groq"}, Preset{Type: TypeOpenAI, BaseURL: "https://api.groq.com/openai/v1", KeyEnv: "GROQ_API_KEY"}},
		{[]string{"together"}, Preset{Type: TypeOpenAI, BaseURL: "https://api.together.ai/v1", KeyEnv: "TOGETHER_API_KEY"}},
		{[]string{"fireworks"}, Preset{Type: TypeOpenAI, BaseURL: "https://api.fireworks.ai/inference/v1", KeyEnv: "FIREWORKS_API_KEY"}},
		{[]string{"deepseek"}, Preset{Type: TypeOpenAI, BaseURL: "https://api.deepseek.com", KeyEnv: "DEEPSEEK_API_KEY"}},
		{[]string{"xai", "grok"}, Preset{Type: TypeOpenAI, BaseURL: "https://api.x.ai/v1", KeyEnv: "XAI_API_KEY"}},
		{[]string{"mistral"}, Preset{Type: TypeOpenAI, BaseURL: "https://api.mistral.ai/v1", KeyEnv: "MISTRAL_API_KEY"}},
		{[]string{"cerebras"}, Preset{Type: TypeOpenAI, BaseURL: "https://api.cerebras.ai/v1", KeyEnv: "CEREBRAS_API_KEY"}},
		{[]string{"deepinfra"}, Preset{Type: TypeOpenAI, BaseURL: "https://api.deepinfra.com/v1/openai", KeyEnv: "DEEPINFRA_API_KEY"}},
		{[]string{"nvidia", "nim"}, Preset{Type: TypeOpenAI, BaseURL: "https://integrate.api.nvidia.com/v1", KeyEnv: "NVIDIA_API_KEY"}},
		{[]string{"huggingface", "hf"}, Preset{Type: TypeOpenAI, BaseURL: "https://router.huggingface.co/v1", KeyEnv: "HF_TOKEN"}},
		{[]string{"perplexity"}, Preset{Type: TypeOpenAI, BaseURL: "https://api.perplexity.ai", KeyEnv: "PERPLEXITY_API_KEY"}},
		{[]string{"moonshot", "kimi"}, Preset{Type: TypeOpenAI, BaseURL: "https://api.moonshot.ai/v1", KeyEnv: "MOONSHOT_API_KEY"}},
		{[]string{"zai", "glm"}, Preset{Type: TypeOpenAI, BaseURL: "https://api.z.ai/api/paas/v4", KeyEnv: "ZAI_API_KEY"}},
		{[]string{"minimax"}, Preset{Type: TypeOpenAI, BaseURL: "https://api.minimax.io/v1", KeyEnv: "MINIMAX_API_KEY"}},
		{[]string{"qwen", "dashscope"}, Preset{Type: TypeOpenAI, BaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", KeyEnv: "DASHSCOPE_API_KEY",
			Note: "keys are region-bound; set base_url to your workspace's compatible-mode endpoint if needed"}},
		{[]string{"ollama"}, Preset{Type: TypeOpenAI, BaseURL: "http://localhost:11434/v1", KeyEnv: "OLLAMA_API_KEY", KeyOptional: true}},
		{[]string{"lmstudio"}, Preset{Type: TypeOpenAI, BaseURL: "http://localhost:1234/v1", KeyEnv: "LM_API_TOKEN", KeyOptional: true}},
		{[]string{"vllm"}, Preset{Type: TypeOpenAI, BaseURL: "http://localhost:8000/v1", KeyEnv: "VLLM_API_KEY", KeyOptional: true}},
		{[]string{"llamacpp", "llama.cpp"}, Preset{Type: TypeOpenAI, BaseURL: "http://localhost:9931/v1", KeyEnv: "LLAMA_API_KEY", KeyOptional: true,
			Note: "llama-server's default port; older builds use 8080"}},
		{[]string{"litellm"}, Preset{Type: TypeOpenAI, BaseURL: "http://localhost:4000/v1", KeyEnv: "LITELLM_API_KEY", KeyOptional: true}},
		{[]string{"vercel", "vercel-ai-gateway"}, Preset{Type: TypeOpenAI, BaseURL: "https://ai-gateway.vercel.sh/v1", KeyEnv: "AI_GATEWAY_API_KEY"}},
		{[]string{"cloudflare", "cloudflare-ai-gateway"}, Preset{Type: TypeOpenAI, KeyEnv: "CLOUDFLARE_AI_GATEWAY_KEY", NeedsBaseURL: true,
			Note: "base_url: https://gateway.ai.cloudflare.com/v1/ACCOUNT_ID/GATEWAY_ID/compat; models as provider/model"}},
	} {
		for _, n := range p.names {
			pr := p.Preset
			pr.Name = p.names[0]
			Presets[n] = pr
		}
	}
}

// PresetNames lists the canonical preset names, sorted.
func PresetNames() []string {
	var out []string
	for n, p := range Presets {
		if n == p.Name {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// presetFor returns the preset a provider uses: the one it names, or the one
// matching its own name when it sets neither type nor preset.
func presetFor(name string, p *Provider) (Preset, bool) {
	if p.Preset != "" {
		pr, ok := Presets[strings.ToLower(p.Preset)]
		return pr, ok
	}
	if p.Type == "" && p.Auth == "" {
		pr, ok := Presets[strings.ToLower(name)]
		return pr, ok
	}
	return Preset{}, false
}

// applyProviderDefaults resolves presets and logins into type, base URL and
// key variables. It runs before validation and before agent defaults, which
// read provider types.
func (c *Config) applyProviderDefaults() {
	for name, p := range c.Providers {
		if p == nil {
			continue
		}
		if pr, ok := presetFor(name, p); ok {
			p.Preset = pr.Name
			if p.Type == "" {
				p.Type = pr.Type
			}
			if p.BaseURL == "" {
				p.BaseURL = pr.BaseURL
			}
			if len(p.KeysEnv) == 0 && pr.KeyEnv != "" && p.Auth == "" {
				p.KeysEnv = []string{pr.KeyEnv}
			}
		}
		if p.Type == "" {
			switch p.Auth {
			case AuthChatGPT:
				p.Type = TypeResponses
			case AuthCopilot:
				p.Type = TypeOpenAI
			}
		}
	}
}

// KeyOptional reports whether a provider may run without any key: local
// servers, Bedrock (AWS credential chain) and logins.
func (p *Provider) KeyOptional() bool {
	if p.Auth != "" || p.Type == TypeBedrock {
		return true
	}
	if pr, ok := Presets[strings.ToLower(p.Preset)]; ok && pr.KeyOptional {
		return true
	}
	return false
}

func (c *Config) validateProviders() []error {
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(c.Providers)) {
		p := c.Providers[name]
		bad := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("provider %s: "+format, append([]any{name}, args...)...))
		}
		if p == nil {
			bad("empty")
			continue
		}
		if p.Preset != "" {
			if _, ok := Presets[strings.ToLower(p.Preset)]; !ok {
				bad("unknown preset %q (known: %s)", p.Preset, strings.Join(PresetNames(), ", "))
			}
		}
		switch p.Type {
		case TypeAnthropic, TypeOpenAI, TypeResponses, TypeGemini, TypeBedrock:
		case "":
			bad("set type (anthropic, openai, responses, gemini or bedrock), a preset, or auth")
		default:
			bad("type %q must be anthropic, openai, responses, gemini or bedrock", p.Type)
		}
		switch p.Auth {
		case "":
		case AuthChatGPT:
			if p.Type != TypeResponses {
				bad("auth chatgpt needs type responses")
			}
		case AuthCopilot:
			if p.Type != TypeOpenAI {
				bad("auth copilot needs type openai")
			}
		case "claude-token", "claude":
			bad("auth %q is not supported: Anthropic's terms do not allow third-party apps to use Claude subscription credentials; use an API key in keys_env", p.Auth)
		default:
			bad("auth %q must be chatgpt or copilot", p.Auth)
		}
		if p.Region != "" && p.Type != TypeBedrock {
			bad("region applies to bedrock only")
		}
		if p.Auth != "" && len(p.KeysEnv) > 0 {
			bad("set either auth or keys_env, not both")
		}
		if len(p.KeysEnv) == 0 && !p.KeyOptional() {
			bad("keys_env is empty")
		}
		if pr, ok := presetFor(name, p); ok && pr.NeedsBaseURL && p.BaseURL == "" {
			bad("base_url is required (%s)", pr.Note)
		}
	}
	for ref, pr := range c.Pricing {
		if pr == nil || pr.Input < 0 || pr.Output < 0 || pr.CacheRead < 0 || pr.CacheWrite < 0 {
			errs = append(errs, fmt.Errorf("pricing %s: prices must be zero or more", ref))
		}
	}
	return errs
}

// Price is a model's list price in US dollars per million tokens.
type Price struct {
	Input      float64 `yaml:"input"`
	Output     float64 `yaml:"output"`
	CacheRead  float64 `yaml:"cache_read"`
	CacheWrite float64 `yaml:"cache_write"`
}

var errNoProvider = errors.New("unknown provider")

// ResolveModel checks a "<provider>/<model>" reference against the
// configured providers.
func (c *Config) ResolveModel(s string) (ModelRef, error) {
	ref, err := ParseModelRef(s)
	if err != nil {
		return ModelRef{}, err
	}
	if _, ok := c.Providers[ref.Provider]; !ok {
		return ModelRef{}, fmt.Errorf("%w %q in %q", errNoProvider, ref.Provider, s)
	}
	return ref, nil
}
