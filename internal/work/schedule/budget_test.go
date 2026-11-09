package schedule

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// spent stands in for the spending limits: every agent is out of budget.
type spent struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (s *spent) Blocked(agent string) *llm.BudgetError {
	return &llm.BudgetError{Key: "agent:" + agent + ":daily:2026-11-09", Message: "Paused: the daily budget of $1.00 for agent " + agent + " is used up."}
}

func (s *spent) Notice(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[key] {
		return false
	}
	s.seen[key] = true
	return true
}

// Runs whose agent is out of budget are skipped without a session or a
// turn, and the chat is told once, not once per run.
func TestRunsSkippedByBudgetNoticeOnce(t *testing.T) {
	r := newRig(t)
	r.sch.Runner.Budget = &spent{seen: map[string]bool{}}
	ctx := context.Background()
	to, _ := json.Marshal(map[string]string{"account": "tg", "conversation": "7"})
	sc, err := r.st.Create(ctx, Schedule{Agent: "habakkuk", Name: "digest", Spec: "@every 1h", Prompt: "summarize", DeliverTo: to})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		id, err := r.sch.RunNow(ctx, sc.ID)
		if err != nil {
			t.Fatal(err)
		}
		j, _ := r.q.Claim(ctx, "w", time.Minute, KindScheduled)
		if j == nil || j.ID != id {
			t.Fatalf("run %d not claimable", i)
		}
		res, err := r.sch.Handle(ctx, j, "w")
		if err != nil || !strings.HasPrefix(res, "skipped: Paused:") {
			t.Fatalf("run %d: %q %v", i, res, err)
		}
		r.q.Complete(ctx, j.ID, "w", res)
		if turn, _ := r.q.Get(ctx, "turn_"+id); turn != nil {
			t.Fatal("a turn was queued for a skipped run")
		}
		r.now = r.now.Add(time.Second)
	}
	got := r.deliveries()
	if len(got) != 1 || !strings.Contains(got[0].text, `Scheduled task "digest" skipped. Paused: the daily budget`) {
		t.Fatalf("deliveries %+v", got)
	}
}
