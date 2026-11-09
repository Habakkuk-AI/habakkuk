package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/anthropic"
	"github.com/habakkuk-ai/habakkuk/internal/llm/auth"
	"github.com/habakkuk-ai/habakkuk/internal/llm/gemini"
	"github.com/habakkuk-ai/habakkuk/internal/llm/openai"
)

// keyedPresets are the hosted providers whose keys setup looks for, best
// first: the first one found is the one it recommends.
var keyedPresets = []string{"anthropic", "openai", "gemini", "openrouter", "deepseek", "xai", "mistral",
	"groq", "cerebras", "together", "fireworks", "deepinfra", "moonshot", "zai", "minimax", "qwen",
	"perplexity", "nvidia", "huggingface", "vercel"}

// localPresets are the local servers setup probes, in order.
var localPresets = []string{"ollama", "lmstudio", "vllm", "llamacpp", "litellm"}

// keyAliases are other variables people keep a provider's key in.
var keyAliases = map[string][]string{"gemini": {"GOOGLE_API_KEY"}}

// recommendedModels is the model setup picks for a provider; providers
// missing here get the first model their API lists (or the user's pick).
var recommendedModels = map[string]string{
	"anthropic":  "claude-opus-5-5",
	"openai":     "gpt-5.6-sol",
	"gemini":     "gemini-4-argon",
	"openrouter": "anthropic/claude-opus-5-5",
	"deepseek":   "deepseek-v4-pro",
	"xai":        "grok-4.7",
	"mistral":    "mistral-large-4",
	"chatgpt":    "gpt-5.6-sol",
}

// fastModels is the cheaper model setup pairs with a provider's
// recommended one (agents.X.fast_model): quick messages and chores go to it,
// real work to the recommended model. Same provider, so the same key.
var fastModels = map[string]string{
	"anthropic": "claude-haiku-5-5",
	"openai":    "gpt-5.6-luna",
	"gemini":    "gemini-3.8-flash",
	"deepseek":  "deepseek-flash",
}

// fastRef is the fast model to pair with the choice, or "" when the user
// picked a model other than the recommended one or the provider has none.
func (c *modelChoice) fastRef() string {
	f, ok := fastModels[c.provider]
	if !ok || c.auth != "" || c.model != recommendedModels[c.provider] {
		return ""
	}
	return c.provider + "/" + f
}

var providerLabels = map[string]string{
	"anthropic": "Anthropic", "openai": "OpenAI", "gemini": "Google Gemini", "openrouter": "OpenRouter",
	"deepseek": "DeepSeek", "xai": "xAI", "mistral": "Mistral", "groq": "Groq", "cerebras": "Cerebras",
	"together": "Together AI", "fireworks": "Fireworks", "deepinfra": "DeepInfra", "moonshot": "Moonshot (Kimi)",
	"zai": "Z.ai (GLM)", "minimax": "MiniMax", "qwen": "Qwen (DashScope)", "perplexity": "Perplexity",
	"nvidia": "NVIDIA NIM", "huggingface": "Hugging Face", "vercel": "Vercel AI Gateway",
	"ollama": "Ollama", "lmstudio": "LM Studio", "vllm": "vLLM", "llamacpp": "llama.cpp", "litellm": "LiteLLM",
	"chatgpt": "ChatGPT plan", "copilot": "GitHub Copilot plan", "bedrock": "AWS Bedrock",
	"cloudflare": "Cloudflare AI Gateway", "custom": "your API",
}

func providerLabel(name string) string {
	if l, ok := providerLabels[name]; ok {
		return l
	}
	return name
}

// modelChoice is the provider and model the agent will use.
type modelChoice struct {
	provider string // its name under providers:
	preset   string // the preset it uses; "" for a custom API or a login
	typ      string
	baseURL  string // written when set: a custom API, or an override
	keyEnv   string // "" for local servers and logins
	key      string
	source   string // where the key came from: environment, .env, typed
	auth     string // chatgpt or copilot
	model    string
	local    bool
	note     string // shown next to it in the menu
}

func (c *modelChoice) ref() string { return c.provider + "/" + c.model }

// apiBase is the URL calls go to.
func (c *modelChoice) apiBase() string {
	if c.baseURL != "" {
		return c.baseURL
	}
	return config.Presets[c.preset].BaseURL
}

// needsKey reports whether the provider is unusable without a key.
func (c *modelChoice) needsKey() bool {
	return c.auth == "" && !c.local && c.typ != config.TypeBedrock && !config.Presets[c.preset].KeyOptional
}

func (c *modelChoice) describe() string {
	s := providerLabel(c.provider)
	if c.model != "" {
		s += ", " + c.model
	}
	switch {
	case c.note != "":
		s += " (" + c.note + ")"
	case c.source != "":
		s += " (" + c.keyEnv + " from " + c.source + ")"
	}
	return s
}

// currentModel is the model the agent uses now, "" when there is none.
func (s *setup) currentModel() string {
	if s.cfg == nil {
		return ""
	}
	if a, ok := s.cfg.Agents[s.agent]; ok {
		return a.Model
	}
	return ""
}

func (s *setup) modelStep() error {
	s.heading(1, "Pick a model")
	current := s.currentModel()
	if s.opts.provider != "" {
		c, err := s.choiceFromFlags()
		if err != nil {
			return err
		}
		return s.settle(c)
	}
	if !s.interactive && current != "" && s.opts.model == "" {
		s.say("  Keeping %s.", current)
		return nil
	}
	found := s.detectModels()
	if !s.interactive {
		if current != "" && s.opts.model != "" { // a new model on the same provider
			ref, err := config.ParseModelRef(current)
			if err != nil {
				return err
			}
			c := s.choiceFromConfig(ref.Provider)
			c.model = s.opts.model
			return s.settle(c)
		}
		if len(found) == 0 {
			return errors.New("no model provider found: put an API key in the environment (ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY, ...), start Ollama, or pass -provider")
		}
		c := found[0]
		if s.opts.model != "" {
			c.model = s.opts.model
		}
		s.say("  Using %s.", c.describe())
		return s.settle(c)
	}

	var labels []string
	var picks []func() (*modelChoice, error)
	if current != "" {
		labels = append(labels, "Keep "+current)
		picks = append(picks, func() (*modelChoice, error) { return nil, nil })
	}
	if len(found) == 0 {
		s.say("  No API keys in the environment and no model server running on this computer.")
	} else {
		var names []string
		for _, c := range found {
			names = append(names, providerLabel(c.provider))
		}
		s.say("  Found on this computer: %s.", strings.Join(names, ", "))
	}
	for i, c := range found {
		label := c.describe()
		if i == 0 && current == "" {
			label += "  <- recommended"
		}
		labels = append(labels, label)
		picks = append(picks, func() (*modelChoice, error) { return c, nil })
	}
	labels = append(labels, "Another provider: paste an API key", "Sign in with a ChatGPT or GitHub Copilot plan")
	picks = append(picks, s.askProvider, s.askLogin)
	i, err := s.menu("Choose", labels, 0)
	if err != nil {
		return err
	}
	c, err := picks[i]()
	if err != nil || c == nil {
		return err
	}
	return s.settle(c)
}

// choiceFromConfig describes a provider that is already configured.
func (s *setup) choiceFromConfig(name string) *modelChoice {
	p := s.cfg.Providers[name]
	c := &modelChoice{provider: name, preset: p.Preset, typ: p.Type, baseURL: p.BaseURL, auth: p.Auth}
	if len(p.KeysEnv) > 0 {
		c.keyEnv = p.KeysEnv[0]
		c.key, c.source = s.lookup(c.keyEnv)
	}
	c.local = p.KeyOptional() && p.Auth == "" && p.Type != config.TypeBedrock
	return c
}

// choiceFromFlags builds the choice -provider (and friends) describe.
func (s *setup) choiceFromFlags() (*modelChoice, error) {
	o := s.opts
	name := strings.ToLower(o.provider)
	var c *modelChoice
	switch name {
	case auth.ChatGPT, auth.Copilot:
		c = s.loginChoice(name)
		if err := s.ensureLogin(name); err != nil {
			return nil, err
		}
	case "custom":
		if !isHTTP(o.baseURL) {
			return nil, errors.New("-provider custom needs -base-url, an http:// or https:// URL of an OpenAI-compatible API")
		}
		c = &modelChoice{provider: "custom", typ: config.TypeOpenAI, baseURL: o.baseURL, keyEnv: "LLM_API_KEY"}
	default:
		p, ok := config.Presets[name]
		if !ok {
			return nil, fmt.Errorf("unknown provider %q; known: %s, chatgpt, copilot, custom", o.provider, strings.Join(config.PresetNames(), ", "))
		}
		if p.NeedsBaseURL && o.baseURL == "" {
			return nil, fmt.Errorf("provider %s needs -base-url (%s)", p.Name, p.Note)
		}
		c = &modelChoice{provider: p.Name, preset: p.Name, typ: p.Type, keyEnv: p.KeyEnv, model: recommendedModels[p.Name],
			local: p.KeyOptional && p.Type != config.TypeBedrock, baseURL: o.baseURL}
	}
	if o.keyEnv != "" {
		c.keyEnv = o.keyEnv
	}
	if o.model != "" {
		c.model = o.model
	}
	if c.keyEnv != "" {
		c.key, c.source = s.lookup(c.keyEnv)
		if c.key == "" {
			for _, alt := range keyAliases[c.preset] {
				if v, src := s.lookup(alt); v != "" && o.keyEnv == "" {
					c.keyEnv, c.key, c.source = alt, v, src
					break
				}
			}
		}
	}
	if c.key == "" && c.needsKey() {
		if !s.interactive {
			return nil, fmt.Errorf("no key for %s: set %s in the environment or in %s/.env (keys are never passed as flags)", providerLabel(c.provider), c.keyEnv, s.home)
		}
		if err := s.askKey(c); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (s *setup) loginChoice(kind string) *modelChoice {
	c := &modelChoice{provider: kind, auth: kind, typ: config.TypeOpenAI, model: recommendedModels[kind], note: "signed in"}
	if kind == auth.ChatGPT {
		c.typ = config.TypeResponses
	}
	return c
}

// detectModels lists what this computer can use without asking: keys in
// the environment or .env, subscription logins, and local servers.
func (s *setup) detectModels() []*modelChoice {
	var out []*modelChoice
	for _, name := range keyedPresets {
		p := config.Presets[name]
		for _, env := range append([]string{p.KeyEnv}, keyAliases[name]...) {
			if v, src := s.lookup(env); v != "" {
				out = append(out, &modelChoice{provider: name, preset: name, typ: p.Type, keyEnv: env, key: v,
					source: src, model: recommendedModels[name]})
				break
			}
		}
	}
	if names, err := auth.NewStore(s.home).Names(); err == nil {
		for _, kind := range auth.Kinds {
			if slices.Contains(names, kind) {
				out = append(out, s.loginChoice(kind))
			}
		}
	}
	return append(out, s.probeLocal()...)
}

// probeLocal asks each local server preset for its models, all at once
// and briefly, so a missing server costs well under a second.
func (s *setup) probeLocal() []*modelChoice {
	found := make([]*modelChoice, len(localPresets))
	var wg sync.WaitGroup
	for i, name := range localPresets {
		base := s.ep.local[name]
		if base == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(s.ctx, 700*time.Millisecond)
			defer cancel()
			models, err := listOpenAIModels(ctx, base, "")
			if err != nil {
				return
			}
			p := config.Presets[name]
			c := &modelChoice{provider: name, preset: name, typ: p.Type, local: true, note: "running on this computer"}
			if base != p.BaseURL {
				c.baseURL = base
			}
			c.model = pickLocalModel(models)
			if c.model == "" {
				c.note = "running, but it has no models yet"
				if name == "ollama" {
					c.note += ": ollama pull a model first"
				}
			}
			found[i] = c
		}()
	}
	wg.Wait()
	var out []*modelChoice
	for _, c := range found {
		if c != nil && c.model != "" {
			out = append(out, c)
		}
	}
	return out
}

// pickLocalModel prefers a chat model over embedding models.
func pickLocalModel(models []string) string {
	for _, m := range models {
		if !strings.Contains(strings.ToLower(m), "embed") {
			return m
		}
	}
	return ""
}

// listOpenAIModels is GET {base}/models on an OpenAI-compatible API.
func listOpenAIModels(ctx context.Context, base, key string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /models: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		out = append(out, m.ID)
	}
	return out, nil
}

// askProvider asks for any preset (or an API URL) and its key.
func (s *setup) askProvider() (*modelChoice, error) {
	s.say("  Known providers: %s.", strings.Join(append(slices.Clone(keyedPresets), localPresets...), ", "))
	for {
		v, err := s.line("  Provider name, or the URL of an OpenAI-compatible API", "anthropic")
		if err != nil {
			return nil, err
		}
		v = strings.ToLower(strings.TrimSpace(v))
		var c *modelChoice
		if isHTTP(v) {
			c = &modelChoice{provider: "custom", typ: config.TypeOpenAI, baseURL: v, keyEnv: "LLM_API_KEY"}
		} else if p, ok := config.Presets[v]; ok && !p.NeedsBaseURL {
			c = &modelChoice{provider: p.Name, preset: p.Name, typ: p.Type, keyEnv: p.KeyEnv, model: recommendedModels[p.Name],
				local: p.KeyOptional && p.Type != config.TypeBedrock}
		} else {
			s.say("  %q is not a provider setup knows; pick one from the list or give a URL.", v)
			continue
		}
		if c.keyEnv != "" {
			c.key, c.source = s.lookup(c.keyEnv)
		}
		if c.needsKey() || c.provider == "custom" {
			if err := s.askKey(c); err != nil {
				return nil, err
			}
		}
		if c.model == "" || c.provider == "custom" {
			if err := s.pickModel(c); err != nil {
				return nil, err
			}
		}
		return c, nil
	}
}

// askKey asks for the provider's key, hidden.
func (s *setup) askKey(c *modelChoice) error {
	prompt := "  Paste your " + providerLabel(c.provider) + " API key (hidden)"
	if c.provider == "custom" {
		s.say("  (A local server that takes no key? Use its preset instead: ollama, lmstudio, vllm, llamacpp or litellm.)")
		prompt = "  Paste the API's key (hidden)"
	}
	if c.key != "" {
		prompt += "; Enter keeps the one in " + c.source
	}
	for {
		v, err := s.hidden(prompt)
		if err != nil {
			return err
		}
		switch {
		case v != "":
			c.key, c.source = v, "typed"
			return nil
		case c.key != "" || !c.needsKey():
			return nil
		}
		s.say("  A key is needed; paste it, or press Ctrl-C to stop.")
	}
}

// askLogin signs in with a subscription, or uses an existing sign-in.
func (s *setup) askLogin() (*modelChoice, error) {
	i, err := s.menu("Which plan", []string{"ChatGPT (Plus, Pro, Business)", "GitHub Copilot"}, 0)
	if err != nil {
		return nil, err
	}
	kind := auth.Kinds[i]
	if err := s.ensureLogin(kind); err != nil {
		return nil, err
	}
	c := s.loginChoice(kind)
	if c.model == "" {
		if err := s.pickModel(c); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// ensureLogin runs the device sign-in unless the login already exists.
func (s *setup) ensureLogin(kind string) error {
	st := auth.NewStore(s.home)
	if names, err := st.Names(); err == nil && slices.Contains(names, kind) {
		return nil
	}
	if !s.interactive {
		return fmt.Errorf("not signed in to %s: run habakkuk auth login %s first", kind, kind)
	}
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Minute)
	defer cancel()
	_, err := st.Login(ctx, kind, kind, func(d auth.DeviceCode) {
		s.say("  Open %s and enter the code %s (it expires in %s). Waiting...", d.URL, d.Code, d.ExpiresIn.Round(time.Minute))
	})
	if err != nil {
		return fmt.Errorf("sign-in to %s failed: %w", kind, err)
	}
	s.say("  Signed in.")
	return nil
}

// pickModel fills in the model: the flag, the API's list, or the user.
func (s *setup) pickModel(c *modelChoice) error {
	if s.opts.model != "" {
		c.model = s.opts.model
		return nil
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.opts.timeout)
	defer cancel()
	models, _ := listModels(ctx, s.home, c)
	def := c.model
	if def == "" && len(models) > 0 {
		def = models[0]
		if c.local {
			def = pickLocalModel(models)
		}
	}
	if !s.interactive {
		if def == "" {
			return fmt.Errorf("pass -model: setup has no default model for %s", providerLabel(c.provider))
		}
		c.model = def
		return nil
	}
	if len(models) > 0 {
		shown := models[:min(len(models), 12)]
		s.say("  Models it serves: %s%s", strings.Join(shown, ", "), map[bool]string{true: ", ..."}[len(models) > len(shown)])
	}
	for {
		v, err := s.line("  Model", def)
		if err != nil {
			return err
		}
		if v != "" {
			c.model = v
			return nil
		}
		s.say("  Type a model id.")
	}
}

// listModels asks the provider which models it serves.
func listModels(ctx context.Context, home string, c *modelChoice) ([]string, error) {
	if c.typ == config.TypeOpenAI && c.auth == "" {
		return listOpenAIModels(ctx, c.apiBase(), c.key) // plain GET: local servers need no key
	}
	p, key, err := modelProvider(ctx, home, c)
	if err != nil || p == nil {
		return nil, err
	}
	if l, ok := p.(interface {
		ListModels(context.Context, string) ([]string, error)
	}); ok {
		return l.ListModels(ctx, key)
	}
	return nil, nil
}

// settle checks the choice with one tiny call (asking again for a typed
// key that fails) and keeps it.
func (s *setup) settle(c *modelChoice) error {
	if c.model == "" {
		if err := s.pickModel(c); err != nil {
			return err
		}
	}
	for attempt := 1; ; attempt++ {
		err := s.verifyModel(c)
		if err == nil {
			break
		}
		if !s.interactive {
			return fmt.Errorf("model check failed: %w", err)
		}
		s.say("  %v", err)
		if c.source == "typed" && attempt < 3 {
			if err := s.askKey(c); err != nil {
				return err
			}
			continue
		}
		ok, err := s.yes("  Save it anyway and fix it later?", false)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("setup stopped: the model check failed; nothing was written")
		}
		break
	}
	if c.keyEnv != "" && c.key != "" {
		s.keep(c.keyEnv, c.key)
	}
	s.model = c
	return nil
}

// verifyModel makes one tiny call and says how it went.
func (s *setup) verifyModel(c *modelChoice) error {
	if s.opts.skipVerify {
		s.say("  Not checked (-skip-verify): %s.", c.ref())
		return nil
	}
	fmt.Fprintf(s.out, "  Checking %s ... ", c.ref())
	start := s.now()
	err := checkModel(s.ctx, s.home, c, s.opts.timeout)
	if err == nil {
		fmt.Fprintf(s.out, "ok, it answered in %s\n", short(s.now().Sub(start)))
		return nil
	}
	if llm.Classify(err).Kind == llm.ErrRateLimit {
		fmt.Fprintln(s.out, "ok (the key works, but it is rate limited right now)")
		return nil
	}
	fmt.Fprintln(s.out, "failed")
	return s.redact(explainModelErr(err, c, s.opts.timeout), c.key)
}

// modelProvider builds the client for a choice and the credential it
// sends, as the app does for a configured provider. A nil provider means
// the choice cannot be checked here (Bedrock's AWS credential chain).
func modelProvider(ctx context.Context, home string, c *modelChoice) (llm.Provider, string, error) {
	switch c.auth {
	case auth.ChatGPT:
		e, err := auth.NewStore(home).Get(ctx, auth.ChatGPT)
		if err != nil {
			return nil, "", fmt.Errorf("the ChatGPT sign-in does not work: %w (run habakkuk auth login chatgpt)", err)
		}
		return &openai.Responses{ChatGPT: true, UserAgent: "habakkuk", AccountID: func() string { return e.AccountID }}, e.AccessToken, nil
	case auth.Copilot:
		e, err := auth.NewStore(home).Get(ctx, auth.Copilot)
		if err != nil {
			return nil, "", fmt.Errorf("the GitHub Copilot sign-in does not work: %w (run habakkuk auth login copilot)", err)
		}
		base := e.BaseURL
		if base == "" {
			base = auth.CopilotDefaultBaseURL
		}
		return &openai.Provider{BaseURL: base, Headers: auth.CopilotHeaders}, e.AccessToken, nil
	}
	switch c.typ {
	case config.TypeAnthropic:
		return &anthropic.Provider{BaseURL: c.apiBase()}, c.key, nil
	case config.TypeOpenAI:
		return &openai.Provider{BaseURL: c.apiBase()}, c.key, nil
	case config.TypeResponses:
		return &openai.Responses{BaseURL: c.apiBase(), UserAgent: "habakkuk"}, c.key, nil
	case config.TypeGemini:
		return &gemini.Provider{BaseURL: c.apiBase()}, c.key, nil
	}
	return nil, "", nil
}

// checkModel sends "reply with ok" with a small token cap.
func checkModel(ctx context.Context, home string, c *modelChoice, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	p, key, err := modelProvider(ctx, home, c)
	if err != nil || p == nil {
		return err
	}
	_, err = p.Stream(ctx, key, llm.Request{Model: c.model, MaxTokens: 32,
		Messages: []llm.Message{llm.UserText("Reply with one word: ok")}}, func(llm.Event) {})
	return err
}

// explainModelErr turns a failed check into what to do about it.
func explainModelErr(err error, c *modelChoice, timeout time.Duration) error {
	label := providerLabel(c.provider)
	detail := firstLine(err.Error())
	switch llm.Classify(err).Kind {
	case llm.ErrAuth:
		if c.auth != "" {
			return fmt.Errorf("%s refused the sign-in: run habakkuk auth login %s again (%s)", label, c.auth, detail)
		}
		return fmt.Errorf("%s refused the key: check that you copied all of it and that it is a %s key (%s)", label, label, detail)
	case llm.ErrBilling:
		return fmt.Errorf("the key works, but the %s account has no credit or billing set up (%s)", label, detail)
	case llm.ErrNotFound:
		return fmt.Errorf("%s does not serve the model %q: pick another with -model, or see habakkuk models (%s)", label, c.model, detail)
	case llm.ErrNetwork:
		if c.local {
			return fmt.Errorf("no answer from %s at %s within %s: is it running? (%s)", label, c.apiBase(), timeout, detail)
		}
		return fmt.Errorf("no answer from %s within %s: check your connection, or raise -timeout (%s)", label, timeout, detail)
	}
	return fmt.Errorf("the check call to %s failed: %s", c.ref(), detail)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

func isHTTP(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}
