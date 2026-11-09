package config

import (
	"fmt"
	"slices"
	"sort"
)

// AgentSecurity is an agent's safety settings, written inline in
// agents.<name>.
//
// A turn that comes from a messaging channel (anything but the CLI, the
// TUI and the local dashboard) needs approval for the risky capabilities:
// exec, process, write (outside the workspace), computer and browser (on
// loopback or private addresses), whatever the permission mode. Trust
// "full" or channel_allow lifts that for the owner's own messages. Messages
// from people who are not the owner (let in by allow_from "*") never get
// those capabilities.
type AgentSecurity struct {
	// Trust is "channel" (default: channel turns ask) or "full" (the
	// owner's channel turns follow the permission mode, like local ones).
	Trust string `yaml:"trust"`
	// ChannelAllow lists the capabilities the owner's channel turns may use
	// without approval: exec, process, write, computer, browser.
	ChannelAllow []string `yaml:"channel_allow"`
	// AllowTaintedExec lets a turn that read outside content (web pages,
	// search results, email, webhook payloads) run commands and write
	// outside the workspace as the mode says. Off, such calls need a human
	// even in full mode.
	AllowTaintedExec bool `yaml:"allow_tainted_exec"`
}

// SecurityCapabilities are the names channel_allow takes.
var SecurityCapabilities = []string{"exec", "process", "write", "computer", "browser"}

func (a *Agent) validateSecurity(name string) []error {
	var errs []error
	switch a.Trust {
	case "", "channel", "full":
	default:
		errs = append(errs, fmt.Errorf("agent %s: trust %q must be channel or full", name, a.Trust))
	}
	for _, c := range a.ChannelAllow {
		if !slices.Contains(SecurityCapabilities, c) {
			errs = append(errs, fmt.Errorf("agent %s: channel_allow %q must be one of exec, process, write, computer, browser", name, c))
		}
	}
	return errs
}

// ChannelAllowed is the set of capabilities the owner's channel turns may
// use on this agent without approval.
func (a *Agent) ChannelAllowed() map[string]bool {
	out := map[string]bool{}
	if a.Trust == "full" {
		for _, c := range SecurityCapabilities {
			out[c] = true
		}
		return out
	}
	for _, c := range a.ChannelAllow {
		out[c] = true
	}
	return out
}

// MessagingChannel reports whether an account is a messaging channel:
// every type but the token-gated web chat, which is the owner's own.
func (ch *Channel) MessagingChannel() bool { return ch != nil && ch.Type != "web" }

// OpenToAll reports whether anyone may talk on the account (allow_from
// "*"). Such senders are guests unless their id is listed or paired.
func (ch *Channel) OpenToAll() bool { return ch != nil && slices.Contains(ch.AllowFrom, "*") }

// ChannelsOf lists the messaging channels whose conversations can reach an
// agent: as the account's default agent, through a route, or (on accounts
// that let senders pick, like the web chat) never.
func (c *Config) ChannelsOf(agent string) []string {
	var out []string
	for name, ch := range c.Channels {
		if !ch.MessagingChannel() {
			continue
		}
		reach := ch.Agent == agent
		for _, a := range ch.Routes {
			reach = reach || a == agent
		}
		if reach {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
