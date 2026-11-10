package state

import (
	"context"
	"testing"
	"time"
)

func TestChatNotesClaimOnce(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Date(2026, 11, 9, 18, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	add := func(conv, delivery, text string) {
		t.Helper()
		if err := s.AddChatNote(ctx, ChatNote{Conversation: conv, Delivery: delivery, Agent: "ops", Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	add("wa|1|", "d1", "first")
	add("wa|1|", "d1", "first again") // a retried delivery
	add("wa|1|", "d2", "second")
	add("wa|2|", "d3", "other chat")

	got, err := s.ClaimChatNotes(ctx, "wa|1|", "turnA", now.Add(-time.Hour))
	if err != nil || len(got) != 2 || got[0].Text != "first" || got[1].Text != "second" || got[0].Agent != "ops" {
		t.Fatalf("claim = %+v, %v", got, err)
	}
	// Another turn finds nothing while turnA holds them.
	if got, _ := s.ClaimChatNotes(ctx, "wa|1|", "turnB", now.Add(-time.Hour)); len(got) != 0 {
		t.Fatalf("a second turn took claimed notes: %+v", got)
	}
	// A rerun of turnA (crash before its message was written) gets the
	// same notes, plus one that arrived meanwhile.
	add("wa|1|", "d4", "third")
	if got, _ := s.ClaimChatNotes(ctx, "wa|1|", "turnA", now.Add(-time.Hour)); len(got) != 3 {
		t.Fatalf("rerun claim = %+v", got)
	}
	if err := s.ChatNotesSeen(ctx, "turnA"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ClaimChatNotes(ctx, "wa|1|", "turnA", now.Add(-time.Hour)); len(got) != 0 {
		t.Fatalf("seen notes came back: %+v", got)
	}
	if got, _ := s.ClaimChatNotes(ctx, "wa|2|", "turnC", now.Add(-time.Hour)); len(got) != 1 || got[0].Text != "other chat" {
		t.Fatalf("other chat = %+v", got)
	}
}

func TestChatNotesStaleClaim(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Date(2026, 11, 9, 18, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.AddChatNote(ctx, ChatNote{Conversation: "c", Delivery: "d1", Text: "x"})
	if got, _ := s.ClaimChatNotes(ctx, "c", "dead", now.Add(-time.Hour)); len(got) != 1 {
		t.Fatal("first claim failed")
	}
	// The claiming turn never wrote its message; after the stale window
	// another turn takes the note.
	now = now.Add(2 * time.Hour)
	if got, _ := s.ClaimChatNotes(ctx, "c", "next", now.Add(-time.Hour)); len(got) != 1 {
		t.Fatalf("stale claim not taken over: %+v", got)
	}
	s.ChatNotesSeen(ctx, "next")
	if got, _ := s.ClaimChatNotes(ctx, "c", "dead", now.Add(-time.Hour)); len(got) != 0 {
		t.Fatalf("the dead turn got the note back: %+v", got)
	}
	if n, err := s.Prune(ctx, now.Add(time.Minute)); err != nil || n == 0 {
		t.Fatalf("prune = %d, %v", n, err)
	}
}
