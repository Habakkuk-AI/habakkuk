package main

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/habakkuk-ai/habakkuk/internal/config"
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
		if err := yaml.Unmarshal(s.raw, &doc); err != nil {
			return nil, false, fmt.Errorf("config.yaml: %w", err)
		}
		if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
			return nil, false, errors.New("config.yaml is not a mapping")
		}
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
	if !edited {
		return s.raw, false, nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, false, err
	}
	if err := enc.Close(); err != nil {
		return nil, false, err
	}
	out := spaceSections(buf.Bytes())
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
		setKey(a, "permission", str("workspace"), "it may change files only in its workspace")
		ws := s.opts.workspace
		if ws == "" {
			ws = defaultWorkspace
		}
		setKey(a, "workspace", str(ws), "")
		return
	}
	setKey(a, "model", str(c.ref()), "")
}

// applyChannel adds the channel account, or updates the one of that type.
func (s *setup) applyChannel(root *yaml.Node) {
	c := s.channel
	if c.typ == "web" {
		if getKey(root, "http") == nil {
			h := ensureMap(root, "http", "The web chat at http://"+config.DefaultListen+"/ while habakkuk serve runs.\nREADME: Web chat, API and webhooks.")
			setKey(h, "listen", str(config.DefaultListen), "loopback only; another address needs token_env")
		}
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

// spaceSections puts a blank line before each top-level section, which
// the YAML encoder drops.
func spaceSections(data []byte) []byte {
	lines := strings.Split(string(data), "\n")
	var out []string
	for i, l := range lines {
		// A top-level key or comment that follows anything but a blank line
		// or a top-level comment (which belongs to it) starts a section.
		if top := l != "" && l[0] != ' ' && l[0] != '-'; top && i > 0 {
			if prev := lines[i-1]; prev != "" && !strings.HasPrefix(prev, "#") {
				out = append(out, "")
			}
		}
		out = append(out, l)
	}
	return []byte(strings.Join(out, "\n"))
}

func str(v string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
	if needsQuotes(v) {
		n.Style = yaml.DoubleQuotedStyle
	}
	return n
}

// needsQuotes reports whether a plain scalar would read as something else.
func needsQuotes(v string) bool {
	var probe any
	if err := yaml.Unmarshal([]byte(v), &probe); err != nil {
		return true
	}
	s, ok := probe.(string)
	return !ok || s != v
}

func intNode(v int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(v)}
}

func flowList(vals ...string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	for _, v := range vals {
		n.Content = append(n.Content, str(v))
	}
	return n
}

// getKey returns the value under key in a mapping, or nil.
func getKey(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setKey sets key to val in a mapping, replacing the old value (and
// keeping the key's comments) or appending it. comment, when set, becomes
// the value's line comment.
func setKey(m *yaml.Node, key string, val *yaml.Node, comment string) {
	if comment != "" {
		val.LineComment = comment
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			old := m.Content[i+1]
			if comment == "" && old.Kind == yaml.ScalarNode && val.Kind == yaml.ScalarNode {
				val.LineComment = old.LineComment
			}
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
}

// ensureMap returns the mapping under key, adding it (with head, a comment
// above it) when missing. A null value (a bare "channels:") becomes a map.
func ensureMap(root *yaml.Node, key, head string) *yaml.Node {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			v := root.Content[i+1]
			if v.Kind != yaml.MappingNode {
				*v = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", HeadComment: v.HeadComment, LineComment: v.LineComment}
			}
			v.Style = 0
			return v
		}
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key, HeadComment: head}, v)
	return v
}
