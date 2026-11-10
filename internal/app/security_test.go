package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
	"github.com/habakkuk-ai/habakkuk/internal/work/schedule"
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

// An agent's trusted folders reach its turns, with Habakkuk's own folders
// protected; an agent without the setting gets neither.
func TestTurnPolicyCarriesTrustedFolders(t *testing.T) {
	home := t.TempDir()
	notes := filepath.Join(t.TempDir(), "notes")
	cfg := &config.Config{
		Home: home,
		Agents: map[string]*config.Agent{
			"main":  {AgentSecurity: config.AgentSecurity{TrustedWritePaths: []string{notes, filepath.Join(home, "agents", "main")}}},
			"other": {},
		},
		Hooks: []*config.Hook{{Event: "tool_before", Command: filepath.Join(home, "my-hooks", "check.sh")}},
	}
	p, _ := TurnPolicy(cfg, "main", nil)
	if len(p.TrustedWrite) != 1 || p.TrustedWrite[0] != notes {
		t.Fatalf("trusted folders: %q", p.TrustedWrite)
	}
	for _, want := range []string{filepath.Join(home, "agents"), filepath.Join(home, "skills"), filepath.Join(home, "hooks"),
		filepath.Join(home, "my-hooks"), filepath.Join(home, "agents", "other")} {
		if !slices.Contains(p.Protected, want) {
			t.Errorf("%s is not protected: %q", want, p.Protected)
		}
	}
	// Without the setting there are no trusted folders, but the protected
	// ones still bound approval rules.
	if p, _ := TurnPolicy(cfg, "other", nil); len(p.TrustedWrite) != 0 || !slices.Contains(p.Protected, filepath.Join(home, "agents")) {
		t.Fatalf("an agent without the setting: %+v", p)
	}
}

// The folders scheduled scripts run from are protected too, so a trusted
// folder never covers a script the scheduler will run.
func TestScheduledScriptFoldersAreProtected(t *testing.T) {
	notes := filepath.Join(t.TempDir(), "notes")
	jobs := filepath.Join(notes, "jobs")
	a, _, _ := openTestApp(t, "")
	a.Cfg.Agents["habakkuk"].TrustedWritePaths = []string{notes}
	ctx := context.Background()
	if _, err := a.Schedules.Create(ctx, schedule.Schedule{Agent: "habakkuk", Name: "sync", Spec: "@every 1h",
		Command: "./sync.sh", Workdir: jobs}); err != nil {
		t.Fatal(err)
	}
	env := &tools.Env{Taint: tools.NewTaint(false)}
	a.applyPolicy(ctx, a.Runner.Agents["habakkuk"], nil, env)
	if len(env.Policy.TrustedWrite) != 1 || !slices.Contains(env.Policy.Protected, jobs) {
		t.Fatalf("policy: trusted %q, protected %q; want %s protected", env.Policy.TrustedWrite, env.Policy.Protected, jobs)
	}
}

// Where a turn stands for approval rules: this machine, the owner's chat
// (or a schedule delivering there), or nowhere (guests, webhooks, A2A).
func TestTurnPlaceForRules(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]*config.Agent{"main": {}},
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
	for name, c := range map[string]struct {
		origin json.RawMessage
		want   string
	}{
		"no reply address":          {nil, ""},
		"web chat":                  {origin("web", ""), tools.PlaceLocal},
		"owner on telegram":         {origin("tg", channel.TrustOwner), tools.ChatPlace("tg", "c")},
		"schedule to a closed chat": {origin("tg", ""), tools.ChatPlace("tg", "c")},
		"schedule to an open chat":  {origin("open", ""), ""},
		"guest":                     {origin("open", channel.TrustGuest), ""},
		"webhook":                   {origin("web", channel.TrustExternal), ""},
		"trigger to a channel":      {origin("tg", channel.TrustExternal), ""},
		"garbage":                   {json.RawMessage(`"x"`), ""},
	} {
		if p, _ := TurnPolicy(cfg, "main", c.origin); p.Place != c.want {
			t.Errorf("%s: place %q, want %q", name, p.Place, c.want)
		}
	}
	// With no reply address, only a turn the owner typed here, or a
	// schedule with no chat, is local; the API, MCP and A2A are not.
	a := &App{Cfg: cfg}
	for name, c := range map[string]struct {
		info agent.TurnInfo
		want string
	}{
		"typed here":         {agent.TurnInfo{Local: true}, tools.PlaceLocal},
		"schedule, no chat":  {agent.TurnInfo{Source: agent.SourceSchedule, Background: true}, tools.PlaceLocal},
		"API or MCP request": {agent.TurnInfo{}, ""},
		"A2A task":           {agent.TurnInfo{Source: agent.SourceA2A}, ""},
	} {
		env := &tools.Env{Taint: tools.NewTaint(false)}
		a.applyPolicy(agent.WithTurnInfo(context.Background(), c.info), &agent.Spec{Name: "main"}, nil, env)
		if env.Policy.Place != c.want {
			t.Errorf("%s: place %q, want %q", name, env.Policy.Place, c.want)
		}
	}
}
