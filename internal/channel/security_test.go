package channel

import (
	"context"
	"encoding/json"
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
	waitFor(t, "approval prompt", hasText(r.a, "Approval needed"))
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
