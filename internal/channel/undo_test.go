package channel

import (
	"context"
	"sync"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

func TestUndoCommand(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ctx := context.Background()
	var mu sync.Mutex
	var undone []string
	r.g.Undo = func(_ context.Context, session string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		undone = append(undone, session)
		return "Undid the file changes from my turn", nil
	}
	r.g.Handle(ctx, dm("m0", "/undo"))
	waitFor(t, "nothing to undo", hasText(r.a, "no changes to undo"))

	r.g.Handle(ctx, dm("m1", "make a file"))
	waitFor(t, "reply", hasText(r.a, "echo: make a file"))
	waitFor(t, "turn finished", func() bool {
		conv, _ := r.g.Store.Conversation(ctx, Address{Account: "tg", Conversation: "100"})
		busy, _ := r.q.Busy(ctx, conv.SessionID, "")
		return !busy
	})
	r.g.Handle(ctx, dm("m2", "/undo"))
	waitFor(t, "undo reply", hasText(r.a, "Undid the file changes"))
	conv, _ := r.g.Store.Conversation(ctx, Address{Account: "tg", Conversation: "100"})
	mu.Lock()
	defer mu.Unlock()
	if len(undone) != 1 || undone[0] != conv.SessionID {
		t.Fatalf("undo called with %v, want [%s]", undone, conv.SessionID)
	}
}
