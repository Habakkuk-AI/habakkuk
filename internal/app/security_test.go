package app

import (
	"encoding/json"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

func TestTurnPolicy(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]*config.Agent{
			"main":    {},
			"trusted": {AgentSecurity: config.AgentSecurity{Trust: "full", AllowTaintedExec: true}},
			"some":    {AgentSecurity: config.AgentSecurity{ChannelAllow: []string{"write"}}},
		},
		Channels: map[string]*config.Channel{
			"tg":   {Type: "telegram", AllowFrom: []string{"42"}},
			"open": {Type: "whatsapp", AllowFrom: []string{"*"}},
			"web":  {Type: "web", AllowFrom: []string{"*"}},
		},
	}
	origin := func(acct, trust string) json.RawMessage {
		b, _ := json.Marshal(channel.Address{Account: acct, Conversation: "c", Trust: trust})
		return b
	}
	cases := []struct {
		name     string
		agent    string
		origin   json.RawMessage
		channel  string
		guest    bool
		external bool
		allowed  []tools.Capability
		tainted  bool
	}{
		{name: "local", agent: "main"},
		{name: "web chat is local", agent: "main", origin: origin("web", "")},
		{name: "owner on telegram", agent: "main", origin: origin("tg", channel.TrustOwner), channel: "tg"},
		{name: "unknown sender on a closed account", agent: "main", origin: origin("tg", ""), channel: "tg"},
		{name: "guest", agent: "main", origin: origin("open", channel.TrustGuest), channel: "open", guest: true},
		{name: "unknown sender on an open account", agent: "main", origin: origin("open", ""), channel: "open", guest: true},
		{name: "owner on an open account", agent: "main", origin: origin("open", channel.TrustOwner), channel: "open"},
		{name: "trust full", agent: "trusted", origin: origin("tg", channel.TrustOwner), channel: "tg",
			allowed: []tools.Capability{tools.CapExec, tools.CapProcess, tools.CapWrite, tools.CapComputer, tools.CapBrowser}, tainted: true},
		{name: "channel_allow", agent: "some", origin: origin("tg", channel.TrustOwner), channel: "tg", allowed: []tools.Capability{tools.CapWrite}},
		{name: "webhook", agent: "main", origin: origin("web", channel.TrustExternal), external: true},
		{name: "trigger to a channel", agent: "main", origin: origin("tg", channel.TrustExternal), channel: "tg", external: true},
		{name: "garbage origin", agent: "main", origin: json.RawMessage(`"x"`), channel: "unknown", guest: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, external := TurnPolicy(cfg, c.agent, c.origin)
			if p.Channel != c.channel || p.Guest != c.guest || external != c.external || p.AllowTaintedExec != c.tainted {
				t.Fatalf("got channel=%q guest=%v external=%v allowTainted=%v", p.Channel, p.Guest, external, p.AllowTaintedExec)
			}
			if len(p.Allowed) != len(c.allowed) {
				t.Fatalf("allowed %v, want %v", p.Allowed, c.allowed)
			}
			for _, cp := range c.allowed {
				if !p.Allowed[cp] {
					t.Fatalf("allowed %v, want %v", p.Allowed, c.allowed)
				}
			}
		})
	}
}
