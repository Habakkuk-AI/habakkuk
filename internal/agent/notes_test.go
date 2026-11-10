package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// fakeNotes hands out one note and counts the calls.
type fakeNotes struct {
	mu          sync.Mutex
	taken, seen int
}

func (f *fakeNotes) TakeNotes(_ context.Context, _, _ string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taken++
	return `[Earlier in this chat, at 18:44, your scheduled task "inbox" sent: "3 new mails"]`
}

func (f *fakeNotes) NotesTainted(context.Context, string) bool { return false }

func (f *fakeNotes) NotesSeen(context.Context, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen++
}

// failOnce fails its first call, as a turn that dies after writing the
// user message would.
type failOnce struct {
	fakeModel
	failed bool
}

func (f *failOnce) Send(ctx context.Context, chain []llm.Target, req llm.Request, on func(llm.Event)) (*llm.Response, error) {
	if !f.failed {
		f.failed = true
		return nil, errors.New("connection reset")
	}
	return f.fakeModel.Send(ctx, chain, req, on)
}

// Notes join the user message once: a rerun of the turn finds the message
// written and only confirms the notes as seen.
func TestChatNotesJoinTheUserMessageOnce(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &failOnce{})
	notes := &fakeNotes{}
	h.runner.Notes = notes
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	h.runner.Submit(ctx, s.ID, "how did you do that?")
	j, _ := h.queue.Claim(ctx, "w1", time.Minute, KindTurn)
	if _, err := h.runner.HandleTurn(ctx, j, "w1"); err == nil {
		t.Fatal("first attempt should fail")
	}
	if _, err := h.runner.HandleTurn(ctx, j, "w1"); err != nil {
		t.Fatal(err)
	}
	if notes.taken != 1 || notes.seen != 2 {
		t.Fatalf("taken %d, seen %d", notes.taken, notes.seen)
	}
	msgs := h.transcript(t, s.ID)
	if msgs[0].Role != llm.User || strings.Count(msgs[0].Text(), "Earlier in this chat") != 1 {
		t.Fatalf("user message: %q", msgs[0].Text())
	}
	note, words := strings.Index(msgs[0].Text(), "Earlier in this chat"), strings.Index(msgs[0].Text(), "how did you do that?")
	if note > words {
		t.Fatalf("notes should come before the user's words: %q", msgs[0].Text())
	}
	for _, m := range msgs[1:] {
		if m.Role == llm.User && strings.Contains(m.Text(), "Earlier in this chat") {
			t.Fatal("notes written twice")
		}
	}
}
