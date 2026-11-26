package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/config/yamledit"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"gopkg.in/yaml.v3"
)

// Edits the dashboard makes to agents and channel routing. Like
// SetChannelDisplay, each one edits config.yaml through its YAML node tree,
// so comments, order and every other setting stay as they were. The edited
// file must load before it is written; the previous file is kept as
// config.yaml.bak. These settings take effect when hk serve restarts.

// AgentChange is what the dashboard can change about an agent. A nil field
// is left as it is. An empty string or list removes the setting, so the
// agent falls back to the default.
type AgentChange struct {
	Description *string   `json:"description"`
	Model       *string   `json:"model"`
	Fallbacks   *[]string `json:"fallbacks"`
	Toolsets    *[]string `json:"toolsets"`
	Home        *string   `json:"home"`
	ReportHome  *string   `json:"report_home"`
	// Daily and Monthly are spending limits in US dollars; 0 removes one.
	Daily   *float64 `json:"daily"`
	Monthly *float64 `json:"monthly"`
}

// SetAgent applies a change to one agent and returns the agent as the
// edited file defines it.
func SetAgent(home, name string, ch AgentChange) (*Agent, error) {
	cfg, err := editConfig(home, func(root *yaml.Node) error {
		a := yamledit.Get(yamledit.Get(root, "agents"), name)
		if a == nil || a.Kind != yaml.MappingNode {
			return fmt.Errorf("config.yaml has no agent %q", name)
		}
		setStr(a, "description", ch.Description)
		setStr(a, "model", ch.Model)
		setList(a, "fallbacks", ch.Fallbacks)
		setList(a, "toolsets", ch.Toolsets)
		setStr(a, "home", ch.Home)
		setStr(a, "report_home", ch.ReportHome)
		if ch.Daily != nil || ch.Monthly != nil {
			b := yamledit.Get(a, "budget")
			if b == nil || b.Kind != yaml.MappingNode {
				b = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				yamledit.Set(a, "budget", b, "")
			}
			b.Style = 0
			setMoney(b, "daily", ch.Daily)
			setMoney(b, "monthly", ch.Monthly)
			if len(b.Content) == 0 {
				deleteKey(a, "budget")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cfg.Agents[name], nil
}

// Route is one conversation sent to an agent other than the channel's
// default. Label is a name for people (a channel's name, say); it is kept as
// the route's comment in config.yaml.
type Route struct {
	Conversation string `json:"conversation"`
	Agent        string `json:"agent"`
	Label        string `json:"label,omitempty"`
}

// Routes lists a channel's routes with their labels, sorted by label and
// then conversation. It reads config.yaml itself, because labels are
// comments that the parsed config does not keep.
func Routes(home, name string) ([]Route, error) {
	data, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if err != nil {
		return nil, err
	}
	doc, err := yamledit.Parse(data)
	if err != nil {
		return nil, err
	}
	ch := yamledit.Get(yamledit.Get(doc.Content[0], "channels"), name)
	if ch == nil {
		return nil, fmt.Errorf("config.yaml has no channel %q", name)
	}
	var out []Route
	if r := yamledit.Get(ch, "routes"); r != nil && r.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(r.Content); i += 2 {
			out = append(out, Route{Conversation: r.Content[i].Value, Agent: r.Content[i+1].Value,
				Label: label(r.Content[i+1].LineComment, r.Content[i].LineComment)})
		}
	}
	slices.SortFunc(out, func(a, b Route) int {
		if c := strings.Compare(strings.ToLower(a.Label), strings.ToLower(b.Label)); c != 0 {
			return c
		}
		return strings.Compare(a.Conversation, b.Conversation)
	})
	return out, nil
}

// label is a route comment as people should read it: the first comment
// that has text, without its # and surrounding space.
func label(comments ...string) string {
	for _, c := range comments {
		if c = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(c), "#")); c != "" {
			return c
		}
	}
	return ""
}

// SetChannelRouting replaces a channel's routes, and its default agent when
// agent is not empty, and returns the channel as the edited file defines it.
func SetChannelRouting(home, name, agent string, routes []Route) (*Channel, error) {
	cfg, err := editConfig(home, func(root *yaml.Node) error {
		ch := yamledit.Get(yamledit.Get(root, "channels"), name)
		if ch == nil || ch.Kind != yaml.MappingNode {
			return fmt.Errorf("config.yaml has no channel %q", name)
		}
		if agent != "" {
			yamledit.Set(ch, "agent", yamledit.Str(agent), "")
		}
		seen := map[string]bool{}
		m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, r := range routes {
			conv := strings.TrimSpace(r.Conversation)
			if conv == "" || strings.TrimSpace(r.Agent) == "" {
				return fmt.Errorf("each route needs a conversation and an agent")
			}
			if seen[conv] {
				return fmt.Errorf("conversation %s is routed twice", conv)
			}
			seen[conv] = true
			v := yamledit.Str(strings.TrimSpace(r.Agent))
			if l := oneLine(r.Label); l != "" {
				v.LineComment = "# " + l
			}
			m.Content = append(m.Content, yamledit.Str(conv), v)
		}
		if len(m.Content) == 0 {
			deleteKey(ch, "routes")
		} else {
			old := yamledit.Get(ch, "routes")
			if old != nil {
				m.HeadComment, m.LineComment = old.HeadComment, old.LineComment
			}
			yamledit.Set(ch, "routes", m, "")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cfg.Channels[name], nil
}

// editConfig runs edit on config.yaml's node tree, checks that the result
// loads, and writes it (keeping the previous file as config.yaml.bak) when
// anything changed.
func editConfig(home string, edit func(root *yaml.Node) error) (*Config, error) {
	path := filepath.Join(home, "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	doc, err := yamledit.Parse(data)
	if err != nil {
		return nil, err
	}
	if err := edit(doc.Content[0]); err != nil {
		return nil, err
	}
	out, err := yamledit.Encode(doc)
	if err != nil {
		return nil, err
	}
	cfg, err := ParseHome(out, home)
	if err != nil {
		return nil, fmt.Errorf("config.yaml would not load with the change: %w", err)
	}
	if !bytes.Equal(out, data) {
		if err := store.WriteFileAtomic(path+".bak", data, 0o600); err != nil {
			return nil, err
		}
		if err := store.WriteFileAtomic(path, out, 0o600); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

func setStr(m *yaml.Node, key string, v *string) {
	if v == nil {
		return
	}
	if s := strings.TrimSpace(*v); s != "" {
		yamledit.Set(m, key, yamledit.Str(oneLine(s)), "")
	} else {
		deleteKey(m, key)
	}
}

func setList(m *yaml.Node, key string, v *[]string) {
	if v == nil {
		return
	}
	var vals []string
	for _, s := range *v {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(vals, s) {
			vals = append(vals, s)
		}
	}
	if len(vals) == 0 {
		deleteKey(m, key)
		return
	}
	yamledit.Set(m, key, yamledit.FlowList(vals...), "")
}

func setMoney(m *yaml.Node, key string, v *float64) {
	if v == nil {
		return
	}
	if *v <= 0 {
		deleteKey(m, key)
		return
	}
	val := strconv.FormatFloat(*v, 'f', -1, 64)
	tag := "!!int"
	if strings.Contains(val, ".") {
		tag = "!!float"
	}
	yamledit.Set(m, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: val}, "")
}

func deleteKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// oneLine keeps a value on one line, so it cannot break the YAML around it.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
