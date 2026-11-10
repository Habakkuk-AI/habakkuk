package main

import (
	"bytes"
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/config/yamledit"
)

// Setup edits config.yaml as a YAML node tree, so a re-run changes only
// what was asked and keeps the rest, comments included. A new file starts
// from a short header and gets only what was chosen.

const setupHeader = `Habakkuk settings, written by habakkuk setup. Run it again to change them,
or edit this file: every option is explained in the README, under Configuration
(https://github.com/habakkuk-ai/habakkuk#configuration).
Keys and tokens are kept in .env next to this file, never here.`

const defaultWorkspace = "~/habakkuk-workspace"

// render returns the new config.yaml and whether it differs from the old.
func (s *setup) render() ([]byte, bool, error) {
	var doc yaml.Node
	if s.raw != nil {
		d, err := yamledit.Parse(s.raw)
		if err != nil {
			return nil, false, err
		}
		doc = *d
	} else {
		root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setKey(root, "version", intNode(config.CurrentVersion), "")
		doc = yaml.Node{Kind: yaml.DocumentNode, HeadComment: setupHeader, Content: []*yaml.Node{root}}
	}
	root := doc.Content[0]
	edited := s.raw == nil
	if s.model != nil {
		s.applyModel(root)
		edited = true
	}
	if s.opts.workspace != "" {
		agent := ensureMap(root, "agents", "")
		if a := getKey(agent, s.agent); a != nil && a.Kind == yaml.MappingNode {
			setKey(a, "workspace", str(s.opts.workspace), "")
			edited = true
		}
	}
	if s.channel != nil {
		s.applyChannel(root)
		edited = true
	}
	if s.raw == nil {
		s.applyStarterDefaults(root)
	}
	if !edited {
		return s.raw, false, nil
	}
	out, err := yamledit.Encode(&doc)
	if err != nil {
		return nil, false, err
	}
	if _, err := config.ParseHome(out, s.home); err != nil {
		return nil, false, fmt.Errorf("the config setup built does not load, so nothing was written: %w", err)
	}
	return out, !bytes.Equal(out, s.raw), nil
}

// applyModel adds the provider (or completes an existing one) and points
// the agent at the model, creating the agent when it is new.
func (s *setup) applyModel(root *yaml.Node) {
	c := s.model
	providers := ensureMap(root, "providers", "Where the model comes from. Known services need only a preset;\nREADME: Models and providers. habakkuk models lists what each one serves.")
	var existing *config.Provider
	if s.cfg != nil {
		existing = s.cfg.Providers[c.provider]
	}
	p := getKey(providers, c.provider)
	if p == nil || p.Kind != yaml.MappingNode {
		p = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setKey(providers, c.provider, p, "")
		existing = nil
	}
	switch {
	case c.auth != "":
		if existing == nil {
			setKey(p, "auth", str(c.auth), "a subscription sign-in (habakkuk auth login "+c.auth+")")
		}
	default:
		if existing == nil && c.preset != "" {
			note := "fills in the API type and URL"
			if c.local {
				note = "a server on this computer; no key needed"
			}
			setKey(p, "preset", str(c.preset), note)
		}
		if existing == nil && c.preset == "" {
			setKey(p, "type", str(c.typ), "")
		}
		if c.baseURL != "" && (existing == nil || existing.BaseURL != c.baseURL) {
			setKey(p, "base_url", str(c.baseURL), "")
		}
		if c.keyEnv != "" && !c.local && (existing == nil || !slices.Contains(existing.KeysEnv, c.keyEnv)) {
			keys := []string{c.keyEnv}
			if existing != nil {
				keys = append(keys, existing.KeysEnv...)
			}
			setKey(p, "keys_env", flowList(keys...), "the variable in .env that holds the key")
		}
	}
	agents := ensureMap(root, "agents", "Your agent. permission: read-only | ask | workspace | full (README: Approvals).")
	a := getKey(agents, s.agent)
	if a == nil || a.Kind != yaml.MappingNode {
		a = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setKey(agents, s.agent, a, "")
		setKey(a, "model", str(c.ref()), "")
		if f := c.fastRef(); f != "" {
			setKey(a, "fast_model", str(f), "quick messages and chores; real work goes to model (README: Routing)")
		}
		setKey(a, "permission", str("workspace"), "it may change files only in its workspace")
		ws := s.opts.workspace
		if ws == "" {
			ws = defaultWorkspace
		}
		setKey(a, "workspace", str(ws), "")
		return
	}
	setKey(a, "model", str(c.ref()), "")
	if f := c.fastRef(); f != "" {
		setKey(a, "fast_model", str(f), "quick messages and chores; real work goes to model (README: Routing)")
	}
}

// applyChannel adds the channel account, or updates the one of that type.
func (s *setup) applyChannel(root *yaml.Node) {
	c := s.channel
	if c.typ == "web" {
		s.applyHTTP(root)
		return
	}
	chans := ensureMap(root, "channels", "Where you talk to your agent; habakkuk serve connects them.\nREADME: Channels. Add another with habakkuk setup channel.")
	ch := getKey(chans, c.name)
	if ch == nil || ch.Kind != yaml.MappingNode {
		ch = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setKey(chans, c.name, ch, "")
		note := ""
		switch c.typ {
		case "whatsappagent":
			note = "an agent made in WhatsApp (Settings > Agents)"
		case "whatsapp":
			note = "a linked device; link with habakkuk whatsapp link"
		}
		setKey(ch, "type", str(c.typ), note)
	}
	switch c.typ {
	case "whatsappagent", "telegram", "discord":
		setKey(ch, "token_env", str(c.tokenEnv), "")
	case "slack":
		setKey(ch, "app_token_env", str(c.appEnv), "")
		if getKey(ch, "bot_tokens_env") == nil {
			setKey(ch, "bot_tokens_env", flowList(c.botEnv), "")
		}
	}
	if c.apiURL != "" {
		setKey(ch, "api_url", str(c.apiURL), "")
	}
	if !c.exists || len(c.allow) > 0 {
		setKey(ch, "allow_from", flowList(c.allow...), c.allowNote)
	}
}

// applyHTTP adds the HTTP listener on loopback: the dashboard at /app/
// and the web chat at /, while habakkuk serve runs.
func (s *setup) applyHTTP(root *yaml.Node) {
	if getKey(root, "http") != nil {
		return
	}
	l := s.webListen()
	h := ensureMap(root, "http", "The dashboard at http://"+l+"/app/ and the web chat at http://"+l+"/,\nwhile habakkuk serve runs. README: Dashboard; Web chat, API and webhooks.")
	setKey(h, "listen", str(l), "loopback only; another address needs token_env")
}

// applyStarterDefaults gives a new home what a first-timer would otherwise
// have to find in the docs: the dashboard, and memory that keeps itself
// up to date. A re-run never adds them back once removed.
func (s *setup) applyStarterDefaults(root *yaml.Node) {
	s.applyHTTP(root)
	if getKey(root, "memory") == nil {
		m := ensureMap(root, "memory", "Memory review: when a chat goes quiet, the agent saves what is worth keeping\n(the fast model does it when there is one). README: Memory.")
		setKey(m, "review", yamledit.Bool(true), "")
	}
}

// The node helpers are shared with habakkuk migrate.
var (
	spaceSections = yamledit.SpaceSections
	str           = yamledit.Str
	intNode       = yamledit.Int
	flowList      = yamledit.FlowList
	getKey        = yamledit.Get
	setKey        = yamledit.Set
	ensureMap     = yamledit.EnsureMap
)
