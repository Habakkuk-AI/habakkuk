package channel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// withGuestAsk gives the agent a home (the owner's chat, tg:100) when home
// is set, and the app's turn policy in small: a guest's turn asks the
// owner when the agent has a home (guest_approvals: home).
func withGuestAsk(home string) func(*rig) {
	return func(r *rig) {
		r.g.Runner.Agents["habakkuk"].Home = home
		r.g.Runner.Policy = func(_ context.Context, spec *agent.Spec, origin json.RawMessage, env *tools.Env) {
			var to Address
			if json.Unmarshal(origin, &to) != nil || to.Account == "" {
				return
			}
			guest := to.Trust == TrustGuest
			env.Policy = &tools.Policy{Channel: to.Account, Guest: guest, GuestAsk: guest && spec.Home != ""}
		}
	}
}

// guestDM is a message from someone let in by allow_from "*", in a chat
// of their own.
func guestDM(id, text string) Message {
	m := dm(id, text)
	m.Conversation, m.Sender, m.SenderName = "200", "9", "Kemi"
	return m
}

// sentTo lists what went to one conversation.
func sentTo(r *rig, conv string) []string {
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	var out []string
	for _, s := range r.a.sent {
		if s.To.Conversation == conv {
			out = append(out, s.Text)
		}
	}
	return out
}

func convHas(r *rig, conv, sub string) func() bool {
	return func() bool {
		for _, s := range sentTo(r, conv) {
			if strings.Contains(s, sub) {
				return true
			}
		}
		return false
	}
}

func TestGuestRequestAsksOwnerInHome(t *testing.T) {
	r := openStateRig(t, t.TempDir(), &config.Channel{AllowFrom: []string{"7", "*"}}, withGuestAsk("tg:100"))
	r.g.Handle(context.Background(), guestDM("g1", "please run it"))

	waitFor(t, "prompt in the owner's chat", convHas(r, "100", "I'd like to run a command on your computer."))
	waitFor(t, "why", convHas(r, "100", "someone else asked me for this, and only you can OK it."))
	waitFor(t, "what they asked", convHas(r, "100", `They asked in tg: "please run it"`))
	waitFor(t, "guest told it waits", convHas(r, "200", "I've asked the owner to OK this."))
	for _, s := range sentTo(r, "200") {
		if strings.Contains(s, "I'd like to run a command") {
			t.Fatalf("the prompt went to the guest's chat: %q", s)
		}
	}
	if convHas(r, "100", "always")() {
		t.Fatal("a guest's request offered Always allow")
	}

	// The guest can't answer it from their own chat.
	r.g.Handle(context.Background(), guestDM("g2", "yes"))
	send(r, "m1", "yes") // the owner, in the home chat
	waitFor(t, "tool output", convHas(r, "200", "approved-run"))
}

func TestGuestRequestOwnerDeclines(t *testing.T) {
	r := openStateRig(t, t.TempDir(), &config.Channel{AllowFrom: []string{"7", "*"}}, withGuestAsk("tg:100"))
	r.g.Handle(context.Background(), guestDM("g1", "please run it"))
	waitFor(t, "prompt in the owner's chat", convHas(r, "100", "I'd like to run a command on your computer."))
	send(r, "m1", "no")
	waitFor(t, "denial", convHas(r, "200", "the owner declined exec"))
	if convHas(r, "200", "approved-run\n")() {
		t.Fatal("ran after the owner said no")
	}
}

// A guest's answer in the owner's home never counts: the home is the
// owner's chat, and only the owner answers approvals.
func TestGuestCannotAnswerInHome(t *testing.T) {
	r := openStateRig(t, t.TempDir(), &config.Channel{AllowFrom: []string{"7", "*"}}, withGuestAsk("tg:100"))
	r.g.Handle(context.Background(), guestDM("g1", "please run it"))
	waitFor(t, "prompt in the owner's chat", convHas(r, "100", "I'd like to run a command on your computer."))
	yes := dm("m1", "yes")
	yes.Sender = "9"
	r.g.Handle(context.Background(), yes)
	send(r, "m2", "no")
	waitFor(t, "denial", convHas(r, "200", "the owner declined exec"))
}

// Without the setting (no GuestAsk), a guest's request is refused as before
// and nobody is asked.
func TestGuestRequestRefusedWithoutSetting(t *testing.T) {
	r := openStateRig(t, t.TempDir(), &config.Channel{AllowFrom: []string{"7", "*"}}, withGuestAsk(""))
	r.g.Handle(context.Background(), guestDM("g1", "please run it"))
	waitFor(t, "refusal", convHas(r, "200", "not available to messages from people other than the owner"))
	if len(sentTo(r, "100")) != 0 {
		t.Fatalf("the owner's chat got %q", sentTo(r, "100"))
	}
}
