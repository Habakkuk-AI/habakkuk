package schedule

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
)

// A one-off's run is done once its turn is: the list must not keep saying
// "started".
func TestOneOffStatusSettlesWhenTheTurnEnds(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	ok, _ := r.st.Create(ctx, Schedule{Agent: "habakkuk", Name: "remind", Spec: "at 2026-11-09 09:30", Prompt: "call mum"})
	bad, _ := r.st.Create(ctx, Schedule{Agent: "habakkuk", Name: "other", Spec: "at 2026-11-09 09:30", Prompt: "fail"})
	r.now = r.now.Add(32 * time.Minute)
	if n, _ := r.sch.Tick(ctx); n != 2 {
		t.Fatal("one-offs did not start")
	}
	status := func(id string) string {
		sc, err := r.st.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return sc.LastStatus
	}
	// Run both scheduled jobs: each queues a turn.
	for i := 0; i < 2; i++ {
		j, err := r.q.Claim(ctx, "w", time.Minute, KindScheduled)
		if err != nil || j == nil {
			t.Fatal("scheduled job not claimable")
		}
		turnID, err := r.sch.Handle(ctx, j, "w")
		if err != nil {
			t.Fatal(err)
		}
		r.q.Complete(ctx, j.ID, "w", turnID)
	}
	if err := r.sch.Settle(ctx); err != nil {
		t.Fatal(err)
	}
	if s := status(ok.ID); s != "started" {
		t.Fatalf("status while the turn waits: %q", s)
	}
	// The turns end: one replies, one fails for good.
	for i := 0; i < 2; i++ {
		turn, err := r.q.Claim(ctx, "w", time.Minute, agent.KindTurn)
		if err != nil || turn == nil {
			t.Fatal("turn not claimable")
		}
		var p agent.TurnPayload
		turn.Decode(&p)
		if strings.Contains(p.Text, "fail") {
			r.q.Fail(ctx, turn.ID, "w", errors.New("model refused"), false, 0)
		} else {
			r.q.Complete(ctx, turn.ID, "w", "Reminder sent.")
		}
	}
	if _, err := r.sch.Tick(ctx); err != nil { // serve settles on every tick
		t.Fatal(err)
	}
	if s := status(ok.ID); s != "done" {
		t.Fatalf("status after the turn: %q, want done", s)
	}
	if s := status(bad.ID); !strings.HasPrefix(s, "failed: ") || !strings.Contains(s, "model refused") {
		t.Fatalf("status after a failed turn: %q", s)
	}
	list, _ := r.st.List(ctx, "")
	if out := Format(list, r.st.loc); !strings.Contains(out, "ran once") || strings.Contains(out, "paused") {
		t.Fatalf("list:\n%s", out)
	}
}

func TestFirstRunNamesTheTimezone(t *testing.T) {
	toronto, _ := time.LoadLocation("America/Toronto")
	at := time.Date(2026, 11, 9, 22, 57, 0, 0, time.UTC)
	if got := When(at, toronto); got != "Mon 2026-11-09 17:57 America/Toronto (22:57 UTC)" {
		t.Fatalf("When = %q", got)
	}
	if got := When(at, time.UTC); got != "Mon 2026-11-09 22:57 UTC" {
		t.Fatalf("When in UTC = %q", got)
	}
	if note := ZoneNote("at 2026-11-09 17:57", toronto); !strings.Contains(note, "read in America/Toronto") {
		t.Fatalf("note = %q", note)
	}
	for _, spec := range []string{"at 2026-11-09T22:57:00Z", "0 9 * * *", "@every 2h"} {
		if note := ZoneNote(spec, toronto); note != "" {
			t.Fatalf("%q: unexpected note %q", spec, note)
		}
	}
}
