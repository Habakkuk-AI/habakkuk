package security

import (
	"fmt"
	"slices"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// AgentPolicy is the effective tool policy of one agent: what its turns may
// do depending on where they come from.
type AgentPolicy struct {
	Agent      string
	Permission string
	Approvals  string
	// Channels are the messaging channels that reach the agent; Open are
	// those that let anyone talk (allow_from "*"), whose unknown senders
	// are guests.
	Channels, Open []string
	// GuestAsk are the open channels whose guests' risky requests ask the
	// owner in the agent's home chat (guest_approvals: home).
	GuestAsk []string
	Home     string // the agent's home chat
	Webhooks       []string
	// Allowed are the capabilities the owner's channel turns use without
	// approval; Gated those that need it.
	Allowed, Gated   []string
	AllowTaintedExec bool
	// TrustedWrite are the folders outside the workspace where the file
	// tools may write notes without approval (trusted_write_paths);
	// TrustedIgnored the entries that are not honoured because they reach
	// into Habakkuk's own folders.
	TrustedWrite, TrustedIgnored []string
}

// Policies lists every agent's effective policy, sorted by agent.
func Policies(c *config.Config) []AgentPolicy {
	var out []AgentPolicy
	for _, name := range c.AgentNames() {
		a := c.Agents[name]
		p := AgentPolicy{Agent: name, Permission: a.Permission, Approvals: a.Approvals, AllowTaintedExec: a.AllowTaintedExec, Home: a.Home}
		p.TrustedWrite, p.TrustedIgnored = c.TrustedWriteDirs(name)
		for _, ch := range c.ChannelsOf(name) {
			p.Channels = append(p.Channels, ch)
			if c.Channels[ch].OpenToAll() {
				p.Open = append(p.Open, ch)
				if c.Channels[ch].GuestsAskHome() && a.Home != "" {
					p.GuestAsk = append(p.GuestAsk, ch)
				}
			}
		}
		if c.HTTP != nil {
			for _, hn := range sortedKeys(c.HTTP.Webhooks) {
				if w := c.HTTP.Webhooks[hn]; w != nil && w.Agent == name {
					p.Webhooks = append(p.Webhooks, hn)
				}
			}
		}
		allowed := a.ChannelAllowed()
		for _, cp := range config.SecurityCapabilities {
			if allowed[cp] {
				p.Allowed = append(p.Allowed, cp)
			} else {
				p.Gated = append(p.Gated, cp)
			}
		}
		out = append(out, p)
	}
	return out
}

// Lines renders the policy for hk status and hk security.
func (p AgentPolicy) Lines() []string {
	out := []string{fmt.Sprintf("agent %s: permission %s, approvals %s", p.Agent, p.Permission, p.Approvals)}
	tainted := "after reading outside content (web, email, webhooks), exec, process and writes outside the workspace need a human, even in full mode"
	if p.AllowTaintedExec {
		tainted = "allow_tainted_exec is set: outside content does not change what the mode allows"
	}
	out = append(out, "  local (CLI, TUI, dashboard): the permission mode decides; "+tainted)
	if len(p.Channels) == 0 {
		out = append(out, "  messaging channels: none reach it")
	} else {
		via := strings.Join(p.Channels, ", ")
		switch {
		case len(p.Gated) == 0:
			out = append(out, fmt.Sprintf("  owner via %s: trust full, the permission mode decides", via))
		case len(p.Allowed) == 0:
			out = append(out, fmt.Sprintf("  owner via %s: %s need approval", via, strings.Join(p.Gated, ", ")))
		default:
			out = append(out, fmt.Sprintf("  owner via %s: %s allowed as the mode says; %s need approval", via,
				strings.Join(p.Allowed, ", "), strings.Join(p.Gated, ", ")))
		}
		guests := "no account lets strangers in"
		if len(p.Open) > 0 {
			guests = "senders let in by allow_from \"*\" on " + strings.Join(p.Open, ", ")
		}
		out = append(out, fmt.Sprintf("  guests (%s): %s denied", guests, strings.Join(config.SecurityCapabilities, ", ")))
		if len(p.GuestAsk) > 0 {
			out = append(out, fmt.Sprintf("    except on %s (guest_approvals: home): each such call asks the owner in %s", strings.Join(p.GuestAsk, ", "), p.Home))
		}
	}
	if len(p.Webhooks) > 0 {
		out = append(out, "  webhooks "+strings.Join(p.Webhooks, ", ")+": payloads are outside content (tainted turn)")
	}
	if len(p.TrustedWrite) > 0 {
		out = append(out, "  trusted_write_paths: "+strings.Join(p.TrustedWrite, ", ")+
			": file tools write notes there without approval, from a chat and after reading outside content (never commands, scripts, programs or hidden files)")
	}
	if len(p.TrustedIgnored) > 0 {
		out = append(out, "  trusted_write_paths ignored: "+strings.Join(p.TrustedIgnored, ", ")+
			" (inside Habakkuk's own folders, or a folder that contains them)")
	}
	return out
}

// checkPolicies flags opt-outs of the safe defaults on agents outsiders
// can reach.
func checkPolicies(c *config.Config) []Finding {
	var out []Finding
	for _, p := range Policies(c) {
		reached := len(p.Channels) > 0 || len(p.Webhooks) > 0
		if p.AllowTaintedExec {
			sev := Medium
			if reached {
				sev = High
			}
			out = append(out, Finding{Severity: sev, Check: "agents",
				Message: fmt.Sprintf("agent %s sets allow_tainted_exec: a web page, email or webhook payload it reads can steer its commands without a human", p.Agent),
				Fix:     fmt.Sprintf("remove agents.%s.allow_tainted_exec unless the agent runs in a sandbox", p.Agent)})
		}
		if len(p.TrustedWrite) > 0 && reached {
			out = append(out, Finding{Severity: Low, Check: "agents",
				Message: fmt.Sprintf("agent %s may write notes to %s without approval after reading outside content (trusted_write_paths): a page or email it reads can put text there, though not scripts", p.Agent, strings.Join(p.TrustedWrite, ", ")),
				Fix:     fmt.Sprintf("keep agents.%s.trusted_write_paths to folders that hold only notes, and nothing another program runs or loads", p.Agent)})
		}
		if len(p.TrustedIgnored) > 0 {
			out = append(out, Finding{Severity: Low, Check: "agents",
				Message: fmt.Sprintf("agent %s lists trusted_write_paths that are ignored (%s): they are inside Habakkuk's own folders, or contain them", p.Agent, strings.Join(p.TrustedIgnored, ", ")),
				Fix:     fmt.Sprintf("remove them from agents.%s.trusted_write_paths, or name a folder for notes elsewhere", p.Agent)})
		}
		if len(p.Channels) > 0 && slices.Contains(p.Allowed, "exec") {
			out = append(out, Finding{Severity: Medium, Check: "agents",
				Message: fmt.Sprintf("agent %s lets channel messages (%s) run commands without approval (trust or channel_allow); anyone who takes over that chat account gets your shell", p.Agent, strings.Join(p.Channels, ", ")),
				Fix:     fmt.Sprintf("drop exec from agents.%s.channel_allow (or trust: full), or run it with sandbox: docker", p.Agent)})
		}
	}
	return out
}
