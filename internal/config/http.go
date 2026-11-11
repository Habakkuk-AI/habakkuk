package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"text/template"
)

// DefaultListen keeps the HTTP surface on loopback unless told otherwise.
const DefaultListen = "127.0.0.1:8484"

// WebAccount is the channel account the HTTP chat uses when the config does
// not name one of type web.
const WebAccount = "web"

// HTTP is the optional HTTP surface `hk serve` runs: a chat page, a JSON
// API with Server-Sent Events, and webhooks. Its chat is the channel account
// of type web (one is added as "web" when none is configured).
type HTTP struct {
	Listen   string              `yaml:"listen"`    // host:port; default 127.0.0.1:8484
	TokenEnv string              `yaml:"token_env"` // bearer token for the API; required off loopback
	Webhooks map[string]*Webhook `yaml:"webhooks"`  // POST /hooks/<name>
	// PublicURL is how the internet reaches this server (behind a proxy or
	// tunnel), e.g. https://habakkuk.example.com. Channels that receive
	// webhooks are at <public_url>/channels/<account>.
	PublicURL string `yaml:"public_url"`
	// PushContact is how push services can reach whoever runs this
	// server, sent with every Web Push message: mailto:you@example.com or
	// an https URL. Default public_url when it is https.
	PushContact string `yaml:"push_contact"`
}

// Webhook turns an authenticated POST into a turn for an agent.
type Webhook struct {
	Agent     string `yaml:"agent"`      // default: the default agent
	SecretEnv string `yaml:"secret_env"` // HMAC-SHA256 key, or a bearer secret
	// Prompt is a text/template over the request: {{.Body}} is the raw body
	// and a JSON object's top-level fields are available by name.
	Prompt string `yaml:"prompt"`
	// Deliver is an optional ACCOUNT:CONVERSATION reply address; the reply
	// otherwise lands in the web conversation hook-<name>.
	Deliver string `yaml:"deliver"`
}

var webhookName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// WebAccountName is the channel account of type web, or "" when the HTTP
// surface is off.
func (c *Config) WebAccountName() string {
	for name, ch := range c.Channels {
		if ch != nil && ch.Type == "web" {
			return name
		}
	}
	return ""
}

func (c *Config) applyHTTPDefaults() {
	h := c.HTTP
	if h == nil {
		return
	}
	if h.Listen == "" {
		h.Listen = DefaultListen
	}
	h.PublicURL = strings.TrimRight(h.PublicURL, "/")
	if c.WebAccountName() == "" {
		if c.Channels == nil {
			c.Channels = map[string]*Channel{}
		}
		if _, taken := c.Channels[WebAccount]; !taken {
			c.Channels[WebAccount] = &Channel{Type: "web"}
		}
	}
	for _, ch := range c.Channels {
		// The bearer token is the gate: everyone who holds it may talk.
		if ch != nil && ch.Type == "web" && len(ch.AllowFrom) == 0 {
			ch.AllowFrom = []string{"*"}
		}
	}
	for _, w := range h.Webhooks {
		if w != nil && w.Agent == "" {
			w.Agent = c.defaultAgentName()
		}
	}
}

// defaultAgentName is "habakkuk" if configured, else the only agent, else "".
func (c *Config) defaultAgentName() string {
	if _, ok := c.Agents["habakkuk"]; ok {
		return "habakkuk"
	}
	if len(c.Agents) == 1 {
		return c.AgentNames()[0]
	}
	return ""
}

func (c *Config) validateHTTP() []error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("http: "+format, args...)) }
	webs := 0
	for _, ch := range c.Channels {
		if ch != nil && ch.Type == "web" {
			webs++
		}
	}
	h := c.HTTP
	if h == nil {
		if webs > 0 {
			errs = append(errs, fmt.Errorf("a channel of type web needs the http section"))
		}
		return errs
	}
	if webs > 1 {
		bad("only one channel may have type web")
	}
	if pc := h.PushContact; pc != "" && !strings.HasPrefix(pc, "mailto:") && !strings.HasPrefix(pc, "https://") {
		bad("push_contact %q must start with mailto: or https://", pc)
	}
	if c.WebAccountName() == "" {
		bad("channel %q is taken by another type; add a channel of type web", WebAccount)
	}
	host, _, err := net.SplitHostPort(h.Listen)
	switch {
	case err != nil:
		bad("listen %q: %v", h.Listen, err)
	case h.TokenEnv == "" && !IsLoopback(host):
		bad("listen %s is not a loopback address; set token_env so the API needs a token", h.Listen)
	}
	if h.PublicURL != "" {
		if u, err := url.Parse(h.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" {
			bad("public_url %q must be an http:// or https:// URL without a query", h.PublicURL)
		}
	}
	for name, w := range h.Webhooks {
		hookBad := func(format string, args ...any) { bad("webhook "+name+": "+format, args...) }
		if !webhookName.MatchString(name) {
			hookBad("name must be 1-64 letters, digits, _ or -")
		}
		if w == nil {
			hookBad("set secret_env")
			continue
		}
		if _, ok := c.Agents[w.Agent]; !ok {
			hookBad("agent %q is not configured", w.Agent)
		}
		if w.SecretEnv == "" {
			hookBad("secret_env is required")
		}
		if w.Prompt != "" {
			if _, err := template.New(name).Parse(w.Prompt); err != nil {
				hookBad("prompt: %v", err)
			}
		}
		if w.Deliver != "" {
			acct, conv, ok := strings.Cut(w.Deliver, ":")
			if _, known := c.Channels[acct]; !ok || !known || conv == "" {
				hookBad("deliver %q: want ACCOUNT:CONVERSATION with a configured channel", w.Deliver)
			}
		}
	}
	return errs
}

// IsLoopback reports whether a listen host only accepts local connections.
// An empty host listens on every interface, so it is not loopback.
func IsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
