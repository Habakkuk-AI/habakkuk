package migrate

import (
	"net/url"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// known maps the provider names both sources use to a Habakkuk preset or
// login, with the variables that may hold the key in the source's
// environment, most likely first.
type known struct {
	preset string   // Habakkuk preset; "" for a login
	auth   string   // chatgpt or copilot: a subscription sign-in
	keys   []string // source variables holding the key
}

var knownProviders = map[string]known{
	"anthropic":             {preset: "anthropic", keys: []string{"ANTHROPIC_API_KEY"}},
	"openai":                {preset: "openai", keys: []string{"OPENAI_API_KEY"}},
	"openai-api":            {preset: "openai", keys: []string{"OPENAI_API_KEY"}},
	"openai-codex":          {auth: config.AuthChatGPT},
	"chatgpt":               {auth: config.AuthChatGPT},
	"copilot":               {auth: config.AuthCopilot},
	"github-copilot":        {auth: config.AuthCopilot},
	"gemini":                {preset: "gemini", keys: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}},
	"google":                {preset: "gemini", keys: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}},
	"google-gemini":         {preset: "gemini", keys: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}},
	"google-ai-studio":      {preset: "gemini", keys: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}},
	"openrouter":            {preset: "openrouter", keys: []string{"OPENROUTER_API_KEY"}},
	"groq":                  {preset: "groq", keys: []string{"GROQ_API_KEY"}},
	"together":              {preset: "together", keys: []string{"TOGETHER_API_KEY"}},
	"fireworks":             {preset: "fireworks", keys: []string{"FIREWORKS_API_KEY"}},
	"deepseek":              {preset: "deepseek", keys: []string{"DEEPSEEK_API_KEY"}},
	"xai":                   {preset: "xai", keys: []string{"XAI_API_KEY"}},
	"mistral":               {preset: "mistral", keys: []string{"MISTRAL_API_KEY"}},
	"cerebras":              {preset: "cerebras", keys: []string{"CEREBRAS_API_KEY"}},
	"deepinfra":             {preset: "deepinfra", keys: []string{"DEEPINFRA_API_KEY"}},
	"nvidia":                {preset: "nvidia", keys: []string{"NVIDIA_API_KEY"}},
	"huggingface":           {preset: "huggingface", keys: []string{"HF_TOKEN", "HUGGINGFACE_HUB_TOKEN"}},
	"perplexity":            {preset: "perplexity", keys: []string{"PERPLEXITY_API_KEY"}},
	"moonshot":              {preset: "moonshot", keys: []string{"MOONSHOT_API_KEY", "KIMI_API_KEY"}},
	"kimi":                  {preset: "moonshot", keys: []string{"MOONSHOT_API_KEY", "KIMI_API_KEY"}},
	"kimi-coding":           {preset: "moonshot", keys: []string{"KIMI_API_KEY", "MOONSHOT_API_KEY"}},
	"zai":                   {preset: "zai", keys: []string{"ZAI_API_KEY", "GLM_API_KEY", "Z_AI_API_KEY"}},
	"glm":                   {preset: "zai", keys: []string{"ZAI_API_KEY", "GLM_API_KEY"}},
	"minimax":               {preset: "minimax", keys: []string{"MINIMAX_API_KEY"}},
	"qwen":                  {preset: "qwen", keys: []string{"DASHSCOPE_API_KEY"}},
	"dashscope":             {preset: "qwen", keys: []string{"DASHSCOPE_API_KEY"}},
	"ollama":                {preset: "ollama", keys: []string{"OLLAMA_API_KEY"}},
	"lmstudio":              {preset: "lmstudio", keys: []string{"LM_API_KEY", "LM_API_TOKEN"}},
	"vllm":                  {preset: "vllm", keys: []string{"VLLM_API_KEY"}},
	"llamacpp":              {preset: "llamacpp", keys: []string{"LLAMA_API_KEY"}},
	"litellm":               {preset: "litellm", keys: []string{"LITELLM_API_KEY"}},
	"ai-gateway":            {preset: "vercel", keys: []string{"AI_GATEWAY_API_KEY"}},
	"vercel-ai-gateway":     {preset: "vercel", keys: []string{"AI_GATEWAY_API_KEY"}},
	"cloudflare-ai-gateway": {preset: "cloudflare", keys: []string{"CLOUDFLARE_AI_GATEWAY_KEY"}},
	"bedrock":               {preset: "bedrock"},
	"amazon-bedrock":        {preset: "bedrock"},
}

// directPrefix are model-id prefixes an aggregator uses that a direct
// provider's own API does not want ("anthropic/claude-..." on Anthropic).
var directPrefix = map[string][]string{
	"anthropic": {"anthropic/"},
	"openai":    {"openai/"},
	"gemini":    {"google/", "gemini/"},
	"deepseek":  {"deepseek/"},
	"xai":       {"x-ai/", "xai/"},
	"mistral":   {"mistralai/", "mistral/"},
}

// knownProvider adds the provider a known source name stands for, with its
// key copied from env, and returns the Habakkuk provider name; ok is false
// for a name it does not know.
func (f *Found) knownProvider(name, baseURL string, env map[string]string, keyValue string) (string, bool) {
	k, ok := knownProviders[strings.ToLower(name)]
	if !ok {
		return "", false
	}
	if k.auth != "" {
		f.provider(&Provider{Name: k.auth, Auth: k.auth})
		f.leftover("the "+k.auth+" subscription sign-in", "hk auth login "+k.auth)
		return k.auth, true
	}
	pr := config.Presets[k.preset]
	p := &Provider{Name: k.preset, Preset: k.preset}
	if baseURL != "" && strings.TrimRight(baseURL, "/") != strings.TrimRight(pr.BaseURL, "/") && k.preset != "bedrock" {
		p.BaseURL = baseURL
	}
	f.provider(p)
	if keyValue == "" {
		for _, v := range k.keys {
			if keyValue = env[v]; keyValue != "" {
				break
			}
		}
	}
	f.secret(pr.KeyEnv, keyValue)
	return p.Name, true
}

// customProvider adds an endpoint the source configured by URL. api is
// the wire it speaks (anthropic, responses, gemini; anything else is
// OpenAI-compatible). keyVar names the source variable holding its key,
// keyValue is a literal key.
func (f *Found) customProvider(name, baseURL, api, keyVar, keyValue string, env map[string]string) string {
	if p, ok := localPreset(baseURL); ok {
		if keyValue == "" && keyVar != "" {
			keyValue = env[keyVar]
		}
		b := strings.TrimRight(baseURL, "/")
		if !strings.HasSuffix(b, "/v1") {
			b += "/v1" // the OpenAI-compatible path, not the server's native API
		}
		if b == config.Presets[p].BaseURL {
			b = ""
		}
		f.provider(&Provider{Name: p, Preset: p, BaseURL: b})
		f.secret(config.Presets[p].KeyEnv, keyValue)
		return p
	}
	n := cleanName(name, 32)
	if n == "" {
		n = "custom"
	}
	typ := config.TypeOpenAI
	switch strings.ToLower(api) {
	case "anthropic", "anthropic-messages", "anthropic_messages":
		typ = config.TypeAnthropic
	case "responses", "openai-responses", "codex_responses":
		typ = config.TypeResponses
	case "gemini", "google-generative-ai":
		typ = config.TypeGemini
	}
	v := envSuffix(n) + "_API_KEY"
	if keyVar != "" {
		v = keyVar
		if keyValue == "" {
			keyValue = env[keyVar]
		}
	}
	f.provider(&Provider{Name: n, Type: typ, BaseURL: baseURL, KeysEnv: []string{v}})
	f.secret(v, keyValue)
	return n
}

// localPreset matches a base URL on this machine to a local-server preset
// by port.
func localPreset(baseURL string) (string, bool) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
	default:
		return "", false
	}
	for _, name := range []string{"ollama", "lmstudio", "vllm", "llamacpp", "litellm"} {
		pu, err := url.Parse(config.Presets[name].BaseURL)
		if err == nil && pu.Port() == u.Port() {
			return name, true
		}
	}
	return "", false
}

// modelRef joins a provider and a model id, dropping an aggregator-style
// prefix the provider's own API does not use.
func modelRef(provider, model string) string {
	for _, p := range directPrefix[provider] {
		model = strings.TrimPrefix(model, p)
	}
	return provider + "/" + model
}
