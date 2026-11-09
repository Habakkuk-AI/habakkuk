package config

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"
)

// A2A configures the Agent2Agent protocol: Habakkuk's agents served to other
// agents at /a2a (with an Agent Card at /.well-known/agent-card.json), and
// the peers the a2a toolset talks to.
type A2A struct {
	// Serve exposes the agents on the HTTP surface (needs the http section).
	Serve bool `yaml:"serve"`
	// TokenEnv names a bearer token for A2A clients only; default the http
	// section's token.
	TokenEnv    string `yaml:"token_env"`
	Name        string `yaml:"name"`        // the Agent Card's name (default "Habakkuk")
	Description string `yaml:"description"` // the Agent Card's description
	// Agents limits which agents are served, each with the description
	// other agents see as its skill; default every agent.
	Agents map[string]string `yaml:"agents"`
	// Peers are agents the a2a toolset may call by name.
	Peers map[string]*A2APeer `yaml:"peers"`
}

// A2APeer is another agent reachable over A2A.
type A2APeer struct {
	// URL is the agent's base URL (its card is at
	// /.well-known/agent-card.json below it) or the card's own URL.
	URL string `yaml:"url"`
	// Headers are sent with every request, as for MCP servers: a variable
	// holding the whole value, or a template like "Bearer ${VAR}".
	Headers map[string]string `yaml:"headers"`
	Timeout time.Duration     `yaml:"timeout"` // how long a2a_send waits for an answer (default 5m)
}

// HomeAssistant configures the homeassistant toolset and its event triggers.
type HomeAssistant struct {
	URL      string `yaml:"url"`       // e.g. http://homeassistant.local:8123
	TokenEnv string `yaml:"token_env"` // a long-lived access token
	// AllowDomains are the service domains ha_call_service may call
	// without asking (light, switch, climate, ...). Empty allows none.
	AllowDomains []string `yaml:"allow_domains"`
	// Confirm are domains whose services may be called only after a human
	// approves each call (lock, alarm_control_panel, cover, ...).
	Confirm []string `yaml:"confirm"`
	// Triggers start a turn when a matching event arrives.
	Triggers []*HATrigger `yaml:"triggers"`
}

// HATrigger turns matching Home Assistant events into a turn whose reply is
// delivered to a chat.
type HATrigger struct {
	Name      string `yaml:"name"`
	EventType string `yaml:"event_type"` // default state_changed
	// EntityID matches the event's data.entity_id; * matches any run of
	// characters (binary_sensor.*_doorbell).
	EntityID string `yaml:"entity_id"`
	// To and From match a state_changed event's new and old state.
	To   string `yaml:"to"`
	From string `yaml:"from"`
	// Data matches other top-level fields of the event's data, compared as
	// text (for example device_id or command of a button event).
	Data  map[string]string `yaml:"data"`
	Agent string            `yaml:"agent"` // default: the default agent
	// Prompt is a text/template over the event: .EventType, .EntityID,
	// .From, .To, .Name (friendly name), .Time and .Data.
	Prompt  string `yaml:"prompt"`
	Deliver string `yaml:"deliver"` // ACCOUNT:CONVERSATION
	// Cooldown ignores further matches for this long after one fires
	// (default 30s).
	Cooldown time.Duration `yaml:"cooldown"`
}

var (
	haDomain    = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	triggerName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
)

func (c *Config) applyIntegrationDefaults() {
	if a := c.A2A; a != nil {
		if a.Name == "" {
			a.Name = "Habakkuk"
		}
		for _, p := range a.Peers {
			if p != nil && p.Timeout == 0 {
				p.Timeout = 5 * time.Minute
			}
		}
	}
	if h := c.HomeAssistant; h != nil {
		h.URL = strings.TrimRight(h.URL, "/")
		for _, t := range h.Triggers {
			if t == nil {
				continue
			}
			if t.EventType == "" {
				t.EventType = "state_changed"
			}
			if t.Agent == "" {
				t.Agent = c.defaultAgentName()
			}
			if t.Cooldown == 0 {
				t.Cooldown = 30 * time.Second
			}
		}
	}
}

func (c *Config) validateIntegrations() []error {
	var errs []error
	for name, m := range c.MCP {
		errs = append(errs, validateMCPAuth(name, m)...)
	}
	errs = append(errs, c.validateA2A()...)
	errs = append(errs, c.validateHomeAssistant()...)
	return errs
}

func validateMCPAuth(name string, m *MCPServer) []error {
	if m == nil {
		return nil
	}
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("mcp server %s: "+format, append([]any{name}, args...)...))
	}
	switch m.Auth {
	case "":
		if m.ClientID != "" || m.ClientSecretEnv != "" || len(m.Scopes) > 0 || m.CallbackPort != 0 {
			bad("client_id, client_secret_env, scopes and callback_port need auth: oauth")
		}
	case "oauth":
		if m.URL == "" {
			bad("auth: oauth applies to url servers only")
		}
		if _, ok := m.Headers["Authorization"]; ok {
			bad("auth: oauth sets the Authorization header itself; remove it from headers")
		}
		if m.ClientSecretEnv != "" && m.ClientID == "" {
			bad("client_secret_env needs client_id")
		}
	default:
		bad("auth %q must be oauth or empty", m.Auth)
	}
	if m.CallbackPort < 0 || m.CallbackPort > 65535 {
		bad("callback_port must be a port number")
	}
	return errs
}

func validHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func (c *Config) validateA2A() []error {
	a := c.A2A
	if a == nil {
		return nil
	}
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("a2a: "+format, args...)) }
	if a.Serve && c.HTTP == nil {
		bad("serve needs the http section")
	}
	if a.TokenEnv != "" && !a.Serve {
		bad("token_env applies to serve")
	}
	for name := range a.Agents {
		if _, ok := c.Agents[name]; !ok {
			bad("agents: %q is not a configured agent", name)
		}
	}
	for name, p := range a.Peers {
		if !mcpServerName.MatchString(name) {
			bad("peer %q: name must be 1-32 letters, digits, _ or -", name)
		}
		if p == nil || !validHTTPURL(p.URL) {
			bad("peer %s: url must be an http:// or https:// URL", name)
			continue
		}
		for k, v := range p.Headers {
			if k == "" || v == "" {
				bad("peer %s: header %q: name the header and the variable holding its value", name, k)
			}
		}
		if p.Timeout < 0 {
			bad("peer %s: timeout must not be negative", name)
		}
	}
	return errs
}

func (c *Config) validateHomeAssistant() []error {
	h := c.HomeAssistant
	if h == nil {
		return nil
	}
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("homeassistant: "+format, args...)) }
	if !validHTTPURL(h.URL) {
		bad("url must be an http:// or https:// URL, e.g. http://homeassistant.local:8123")
	}
	if h.TokenEnv == "" {
		bad("token_env is required (a long-lived access token from your Home Assistant profile)")
	}
	for _, d := range slices.Concat(h.AllowDomains, h.Confirm) {
		if !haDomain.MatchString(d) {
			bad("domain %q must be lowercase letters, digits and _", d)
		}
	}
	seen := map[string]bool{}
	for i, t := range h.Triggers {
		if t == nil {
			bad("trigger %d is empty", i+1)
			continue
		}
		tb := func(format string, args ...any) { bad("trigger "+t.Name+": "+format, args...) }
		if !triggerName.MatchString(t.Name) {
			bad("trigger %d: name must be 1-64 letters, digits, _ or -", i+1)
		}
		if seen[t.Name] {
			tb("named twice")
		}
		seen[t.Name] = true
		if _, ok := c.Agents[t.Agent]; !ok {
			tb("agent %q is not configured", t.Agent)
		}
		acct, conv, ok := strings.Cut(t.Deliver, ":")
		if _, known := c.Channels[acct]; !ok || !known || conv == "" {
			tb("deliver %q: want ACCOUNT:CONVERSATION with a configured channel", t.Deliver)
		}
		if t.Prompt != "" {
			if _, err := template.New(t.Name).Parse(t.Prompt); err != nil {
				tb("prompt: %v", err)
			}
		}
		if (t.To != "" || t.From != "") && t.EventType != "state_changed" {
			tb("to and from apply to state_changed events; match other events with data")
		}
		if t.Cooldown < 0 {
			tb("cooldown must not be negative")
		}
	}
	return errs
}
