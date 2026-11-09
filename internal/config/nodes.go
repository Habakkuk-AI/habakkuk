package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/ops/i18n"
)

// Nodes lets other machines (a laptop, a Raspberry Pi, a phone running the
// dashboard) lend Habakkuk capabilities: notifications, screenshots, the
// camera, location, the clipboard, files and, when a node allows it, a
// shell. Devices pair with a one-time code and connect to the HTTP
// listener at /nodes, so nodes need an http section.
type Nodes struct {
	// URL is how devices reach this server, printed by `habakkuk node pair`
	// (default http://<http.listen>).
	URL string `yaml:"url"`
	// Allow lists, per node name ("*" for every node), the commands that
	// run without asking ("*" for every command). Anything else needs
	// approval each time, and unattended runs cannot use it.
	Allow map[string][]string `yaml:"allow"`
	// Timeout is the longest one invocation waits for its node (default 60s).
	Timeout time.Duration `yaml:"timeout"`
	// PairTTL is how long a pairing code works (default 10m).
	PairTTL time.Duration `yaml:"pair_ttl"`
}

// InvokeTimeout is Timeout with its default.
func (n *Nodes) InvokeTimeout() time.Duration {
	if n == nil || n.Timeout <= 0 {
		return 60 * time.Second
	}
	return n.Timeout
}

// PairCodeTTL is PairTTL with its default.
func (n *Nodes) PairCodeTTL() time.Duration {
	if n == nil || n.PairTTL <= 0 {
		return 10 * time.Minute
	}
	return n.PairTTL
}

// Allowed reports whether nodes.allow lets command run on node without
// asking.
func (n *Nodes) Allowed(node, command string) bool {
	if n == nil {
		return false
	}
	for _, key := range []string{node, "*"} {
		if cmds := n.Allow[key]; slices.Contains(cmds, command) || slices.Contains(cmds, "*") {
			return true
		}
	}
	return false
}

// NodesURL is the address devices use to reach this server.
func (c *Config) NodesURL() string {
	if c.Nodes != nil && c.Nodes.URL != "" {
		return strings.TrimRight(c.Nodes.URL, "/")
	}
	listen := "127.0.0.1:8484"
	if c.HTTP != nil && c.HTTP.Listen != "" {
		listen = c.HTTP.Listen
	}
	if strings.HasPrefix(listen, ":") {
		listen = "127.0.0.1" + listen
	}
	return "http://" + listen
}

// validateNodes checks the nodes section and the language settings.
func (c *Config) validateNodes() []error {
	var errs []error
	if !i18n.Known(c.Language) {
		errs = append(errs, fmt.Errorf("language %q is not one Habakkuk has text for (%s)", c.Language, strings.Join(i18n.Supported(), ", ")))
	}
	for name, ch := range c.Channels {
		if ch != nil && !i18n.Known(ch.Language) {
			errs = append(errs, fmt.Errorf("channel %s: language %q is not one Habakkuk has text for (%s)", name, ch.Language, strings.Join(i18n.Supported(), ", ")))
		}
	}
	n := c.Nodes
	if n == nil {
		return errs
	}
	if c.HTTP == nil {
		errs = append(errs, errors.New("nodes: devices connect to the HTTP listener, so nodes need an http section"))
	}
	if n.URL != "" {
		u, err := url.Parse(n.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("nodes: url %q must be an http or https URL", n.URL))
		}
	}
	if n.Timeout < 0 || n.PairTTL < 0 {
		errs = append(errs, errors.New("nodes: timeout and pair_ttl must not be negative"))
	}
	for node, cmds := range n.Allow {
		if node == "" {
			errs = append(errs, errors.New("nodes: allow has an empty node name"))
		}
		for _, cmd := range cmds {
			if strings.TrimSpace(cmd) == "" || strings.ContainsAny(cmd, " \t") {
				errs = append(errs, fmt.Errorf("nodes: allow.%s: %q is not a command name", node, cmd))
			}
		}
	}
	return errs
}
