package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// originTrust reads the trust a message's turn was queued with.
func originTrust(t *testing.T, r *rig, m Message) string {
	t.Helper()
	var trust string
	waitFor(t, "turn queued", func() bool {
		j, err := r.q.Get(context.Background(), TurnID(m))
		if err != nil || j == nil {
			return false
		}
		var p agent.TurnPayload
		if j.Decode(&p) != nil {
			return false
		}
		var a Address
		if json.Unmarshal(p.Origin, &a) != nil {
			return false
		}
		trust = a.Trust
		return true
	})
	return trust
}

func TestTurnOriginCarriesSenderTrust(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7", "*"}})
	owner := dm("m1", "hello")
	r.g.Handle(context.Background(), owner)
	if got := originTrust(t, r, owner); got != TrustOwner {
		t.Fatalf("listed sender: trust %q, want owner", got)
	}
	guest := dm("m2", "hello")
	guest.Sender = "9"
	r.g.Handle(context.Background(), guest)
	if got := originTrust(t, r, guest); got != TrustGuest {
		t.Fatalf("sender let in by *: trust %q, want guest", got)
	}
}

// A command that starts a turn (a skill run as /<skill-name>, /skill, a
// plugin's prompt command, /retry) carries who sent it, like an ordinary
// message: the owner's runs as the owner's and a guest's as a guest's.
func TestCommandTurnsCarrySenderTrust(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7", "*"}})
	ctx := context.Background()
	r.g.AgentCommand = func(_, name string) (AppCommand, bool) {
		if name != "/weekly-report" {
			return AppCommand{}, false
		}
		return AppCommand{Prompt: func(_ context.Context, _, arg string) (string, error) {
			return "[Skill weekly-report] " + arg, nil
		}}, true
	}
	r.g.Commands = map[string]AppCommand{"/skill": {Prompt: func(_ context.Context, _, arg string) (string, error) {
		return "[Skill " + arg + "]", nil
	}}}
	trustOf := func(prefix string, m Message) string {
		t.Helper()
		id := prefix + strings.TrimPrefix(TurnID(m), "turn_")
		var trust string
		waitFor(t, "the turn of "+m.Text+" from "+m.Sender, func() bool {
			j, err := r.q.Get(ctx, id)
			if err != nil || j == nil {
				return false
			}
			var p agent.TurnPayload
			var a Address
			if j.Decode(&p) != nil || json.Unmarshal(p.Origin, &a) != nil {
				return false
			}
			trust = a.Trust
			return true
		})
		return trust
	}
	for i, text := range []string{"/weekly-report for last week", "/skill report", "/retry"} {
		prefix := "cmd_"
		if text == "/retry" {
			prefix = "retry_"
			// /retry needs a message that has been answered.
			waitFor(t, "an earlier turn's reply", hasText(r.a, "[Skill report]"))
		}
		owner := dm(fmt.Sprintf("o%d", i), text)
		r.g.Handle(ctx, owner)
		if got := trustOf(prefix, owner); got != TrustOwner {
			t.Errorf("%s from the owner: trust %q, want owner", text, got)
		}
		guest := dm(fmt.Sprintf("g%d", i), text)
		guest.Sender = "9"
		r.g.Handle(ctx, guest)
		if got := trustOf(prefix, guest); got != TrustGuest {
			t.Errorf("%s from a guest: trust %q, want guest", text, got)
		}
	}
}

func TestPairedSenderIsOwner(t *testing.T) {
	r := newRig(t, &config.Channel{})
	ctx := context.Background()
	code, _, err := r.g.Store.PairCode(ctx, "tg", "7", "Ay")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.g.Store.Pair(ctx, code); err != nil {
		t.Fatal(err)
	}
	m := dm("m1", "hello")
	r.g.Handle(ctx, m)
	if got := originTrust(t, r, m); got != TrustOwner {
		t.Fatalf("paired sender: trust %q, want owner", got)
	}
}

func TestGuestCannotAnswerOwnersApproval(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7", "*"}})
	ctx := context.Background()
	r.g.Handle(ctx, dm("m1", "please run it"))
	waitFor(t, "approval prompt", hasText(r.a, "I'd like to run a command on your computer."))
	yes := dm("m2", "yes")
	yes.Sender = "9" // someone else in the same chat
	r.g.Handle(ctx, yes)
	r.g.Handle(ctx, dm("m3", "no"))
	waitFor(t, "denial result", hasText(r.a, "tool said:"))
	if hasText(r.a, "approved-run\n")() {
		t.Fatal("a guest approved the owner's command")
	}
}

func TestGuestCannotWriteMemory(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"*"}})
	m := dm("m1", "/remember the owner's password is hunter2")
	m.Sender = "9"
	r.g.Handle(context.Background(), m)
	waitFor(t, "refusal", hasText(r.a, "only the owner"))
}

// A guest can neither list memory changes nor undo one.
func TestGuestCannotReadOrUndoMemoryHistory(t *testing.T) {
	for i, cmd := range []string{"/memory", "/undo-memory abc123"} {
		r := newRig(t, &config.Channel{AllowFrom: []string{"*"}})
		m := dm(fmt.Sprint("g", i), cmd)
		m.Sender = "9"
		r.g.Handle(context.Background(), m)
		waitFor(t, cmd+" refusal", hasText(r.a, "only the owner"))
	}
}

// A WhatsApp agent hears only from its creator, so allow_from "*" there
// (what setup writes before it has seen a message) still means the owner.
func TestWhatsAppAgentSenderIsOwner(t *testing.T) {
	r := newRig(t, &config.Channel{Type: "whatsappagent", AllowFrom: []string{"*"}})
	m := dm("m1", "hello")
	m.Sender = "50972923564215"
	r.g.Handle(context.Background(), m)
	if got := originTrust(t, r, m); got != TrustOwner {
		t.Fatalf("whatsappagent sender: trust %q, want owner", got)
	}
}
