package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/anthropic"
	"github.com/habakkuk-ai/habakkuk/internal/llm/auth"
	"github.com/habakkuk-ai/habakkuk/internal/llm/bedrock"
	"github.com/habakkuk-ai/habakkuk/internal/llm/gemini"
	"github.com/habakkuk-ai/habakkuk/internal/llm/openai"
	"github.com/habakkuk-ai/habakkuk/internal/llm/usage"
)

// openModels builds the router's endpoints from the configured providers,
// opens usage.db and hooks usage recording into the router.
func (a *App) openModels(ctx context.Context, home string) error {
	a.Auth = auth.NewStore(home)
	a.Router = &llm.Router{Endpoints: map[string]*llm.Endpoint{}, Log: a.Log}
	for name, p := range a.Cfg.Providers {
		a.Router.Endpoints[name] = a.endpoint(name, p)
	}
	var err error
	if a.Usage, err = usage.Open(ctx, filepath.Join(home, "usage.db")); err != nil {
		return err
	}
	a.closers = append(a.closers, func() { _ = a.Usage.Close() })
	a.Pricer = usage.NewPricer(a.Cfg)
	a.Router.OnUsage = func(ctx context.Context, t llm.Target, u llm.Usage) {
		c := llm.CallerFrom(ctx)
		// Recording must not fail a turn whose context is ending.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := a.Usage.Record(rctx, usage.Call{Provider: t.Endpoint, Model: t.Model, Agent: c.Agent, Session: c.Session, Usage: u}); err != nil {
			a.Log.Warn("record usage failed", "err", err)
		}
	}
	return a.openBudget(ctx)
}

// endpoint builds one provider's adapter and credentials.
func (a *App) endpoint(name string, p *config.Provider) *llm.Endpoint {
	ep := &llm.Endpoint{Name: name}
	// A login's latest entry, for values that ride beside the token.
	var mu sync.Mutex
	var login auth.Entry
	if p.Auth != "" {
		kind := p.Auth
		ep.Credentials = []llm.Credential{{Name: "login:" + kind, Token: func(ctx context.Context) (string, error) {
			e, err := a.Auth.Get(ctx, kind)
			if err == nil {
				mu.Lock()
				login = e
				mu.Unlock()
			}
			return e.AccessToken, err
		}}}
		if list, err := a.Auth.List(); err == nil {
			login = list[kind]
		}
	} else {
		for _, env := range p.KeysEnv {
			ep.Credentials = append(ep.Credentials, llm.Credential{Name: env, Key: a.Secret(env)})
		}
		if p.KeyOptional() && !hasKey(ep.Credentials) {
			// Local servers and the AWS credential chain need no key.
			ep.Credentials = append(ep.Credentials, llm.Credential{Name: "none",
				Token: func(context.Context) (string, error) { return "", nil }})
		}
	}
	switch p.Type {
	case config.TypeAnthropic:
		ep.Provider = &anthropic.Provider{BaseURL: p.BaseURL}
	case config.TypeOpenAI:
		op := &openai.Provider{BaseURL: p.BaseURL}
		if p.Auth == config.AuthCopilot {
			op.Headers = auth.CopilotHeaders
			if op.BaseURL == "" {
				op.BaseURL = login.BaseURL
			}
			if op.BaseURL == "" {
				op.BaseURL = auth.CopilotDefaultBaseURL
			}
		}
		ep.Provider = op
	case config.TypeResponses:
		rp := &openai.Responses{BaseURL: p.BaseURL, ChatGPT: p.Auth == config.AuthChatGPT, UserAgent: "habakkuk"}
		if rp.ChatGPT {
			rp.AccountID = func() string {
				mu.Lock()
				defer mu.Unlock()
				return login.AccountID
			}
		}
		ep.Provider = rp
	case config.TypeGemini:
		ep.Provider = &gemini.Provider{BaseURL: p.BaseURL}
	case config.TypeBedrock:
		ep.Provider = &bedrock.Provider{Region: p.Region, BaseURL: p.BaseURL}
	}
	return ep
}

func hasKey(creds []llm.Credential) bool {
	for _, c := range creds {
		if c.Key != "" {
			return true
		}
	}
	return false
}

// wireModelCommands connects /model and /usage to the runner and channels.
func (a *App) wireModelCommands() {
	a.Runner.Choices = a.Usage
	a.Runner.Budget = a.Budget
	a.Runner.CheckModel = func(ref string) error {
		_, err := a.Cfg.ResolveModel(ref)
		return err
	}
	if a.Gateway != nil {
		a.Gateway.Commands = a.ChatCommands()
	}
}

// ChatCommands are the slash commands chats get beyond the built-in ones.
func (a *App) ChatCommands() map[string]channel.AppCommand {
	cmds := map[string]channel.AppCommand{
		"/usage": {Help: "/usage: tokens and cost of this conversation", Run: func(ctx context.Context, session, _ string) string {
			return a.SessionUsage(ctx, session)
		}},
		"/model": {Help: "/model [provider/model | default]: show or switch this conversation's model", Run: a.Runner.ModelCommand},
		"/budget": {Help: "/budget: spending today and this month against the limits", Run: func(ctx context.Context, session, _ string) string {
			return a.sessionBudget(ctx, session)
		}},
		"/retro": {Help: "/retro [apply N... | reject N]: look back over my last week now, or answer the latest retro's proposals",
			Run: a.retroCommand, Async: true, OwnerOnly: true},
	}
	a.addHubCommands(cmds) // /skills, /skill, /learn, plugin commands
	return cmds
}

// SessionUsage summarizes one session's model calls for a chat.
func (a *App) SessionUsage(ctx context.Context, session string) string {
	calls, err := a.Usage.Calls(ctx, usage.Filter{Session: session})
	if err != nil {
		return "Couldn't read usage: " + err.Error()
	}
	return usage.Summarize(calls, a.Pricer).Brief()
}

// ProviderModels is one provider in `hk models`.
type ProviderModels struct {
	Name, Type, BaseURL, Auth string
	Credentials               string // e.g. "2/2 keys", "logged in", "no key needed"
	Models                    []string
	Err                       error
	Unsupported               bool // the API has no model listing
}

// ListModels asks every provider (in parallel, 15 seconds at most) which
// models it serves.
func (a *App) ListModels(ctx context.Context) []ProviderModels {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	names := make([]string, 0, len(a.Cfg.Providers))
	for n := range a.Cfg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]ProviderModels, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		p, ep := a.Cfg.Providers[name], a.Router.Endpoints[name]
		pm := ProviderModels{Name: name, Type: p.Type, BaseURL: p.BaseURL, Auth: p.Auth}
		pm.Credentials = a.credentialStatus(p, ep)
		lister, ok := ep.Provider.(llm.ModelLister)
		if !ok {
			pm.Unsupported = true
			out[i] = pm
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := firstKey(ctx, ep)
			if err == nil {
				pm.Models, err = lister.ListModels(ctx, key)
			}
			pm.Err = err
			sort.Strings(pm.Models)
			out[i] = pm
		}()
	}
	wg.Wait()
	return out
}

func (a *App) credentialStatus(p *config.Provider, ep *llm.Endpoint) string {
	if p.Auth != "" {
		if list, err := a.Auth.List(); err == nil {
			if _, ok := list[p.Auth]; ok {
				return "logged in (" + p.Auth + ")"
			}
		}
		return "not logged in: run hk auth login " + p.Auth
	}
	have := 0
	for _, c := range ep.Credentials {
		if c.Key != "" {
			have++
		}
	}
	if have == 0 && p.KeyOptional() {
		return "no key needed"
	}
	return fmt.Sprintf("%d/%d keys", have, len(p.KeysEnv))
}

func firstKey(ctx context.Context, ep *llm.Endpoint) (string, error) {
	for _, c := range ep.Credentials {
		if c.Key != "" {
			return c.Key, nil
		}
		if c.Token != nil {
			return c.Token(ctx)
		}
	}
	return "", errors.New("no credentials set")
}
