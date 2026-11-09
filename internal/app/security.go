package app

import (
	"context"
	"encoding/json"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// TurnPolicy is the tool policy of a turn of an agent that came from
// origin (a channel.Address; empty for a turn started on this machine). It
// also reports whether the origin is outside content (a webhook or
// trigger payload), which taints the turn.
func TurnPolicy(cfg *config.Config, agentName string, origin json.RawMessage) (p *tools.Policy, external bool) {
	p = &tools.Policy{}
	ac := cfg.Agents[agentName]
	if ac != nil {
		p.AllowTaintedExec = ac.AllowTaintedExec
	}
	if len(origin) == 0 {
		return p, false
	}
	var addr channel.Address
	if json.Unmarshal(origin, &addr) != nil {
		// Not an address we know: treat it as the strictest case.
		p.Channel, p.Guest = "unknown", true
		return p, false
	}
	external = addr.Trust == channel.TrustExternal
	ch := cfg.Channels[addr.Account]
	if !ch.MessagingChannel() {
		return p, external // the local web chat and dashboard
	}
	p.Channel = addr.Account
	switch addr.Trust {
	case channel.TrustGuest:
		p.Guest = true
	case "":
		// A turn with no sender behind it (a schedule, /retry): on an
		// account anyone can talk to, assume a guest asked for it.
		p.Guest = ch.OpenToAll()
	}
	if ac != nil {
		p.Allowed = map[tools.Capability]bool{}
		for c := range ac.ChannelAllowed() {
			p.Allowed[tools.Capability(c)] = true
		}
	}
	return p, external
}

// applyPolicy is the Runner's PolicyFunc.
func (a *App) applyPolicy(_ context.Context, spec *agent.Spec, origin json.RawMessage, env *tools.Env) {
	p, external := TurnPolicy(a.Cfg, spec.Name, origin)
	env.Policy = p
	if external {
		env.Taint.Mark()
	}
}
