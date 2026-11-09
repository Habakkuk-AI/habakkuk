package schedule

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type rig struct {
	st   *Store
	q    *jobs.Queue
	sch  *Scheduler
	now  time.Time
	dir  string
	mu   sync.Mutex
	sent []sent
}

type sent struct {
	id, text string
	to       json.RawMessage
}

func (r *rig) deliveries() []sent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.sent)
}

// controlSchema is this package's control.db steps after the jobs table,
// with no-ops standing in for other packages' steps.
func controlSchema(upTo int) []store.Migration {
	out := slices.Clone(jobs.Migrations)
	mine := map[int]store.Migration{}
	for _, m := range Migrations {
		mine[m.Version] = m
	}
	for v := 2; v <= upTo; v++ {
		m, ok := mine[v]
		if !ok {
			m = store.Migration{Version: v, SQL: "SELECT 1"}
		}
		out = append(out, m)
	}
	return out
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	control, err := store.Open(ctx, filepath.Join(dir, "control.db"), controlSchema(Migrations[len(Migrations)-1].Version))
	if err != nil {
		t.Fatal(err)
	}
	transcripts, err := store.Open(ctx, filepath.Join(dir, "transcripts.db"), session.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close(); transcripts.Close() })
	loc, _ := time.LoadLocation("America/Toronto")
	r := &rig{st: NewStore(control, loc), q: jobs.NewQueue(control), dir: dir}
	r.now = time.Date(2026, 11, 9, 8, 59, 0, 0, loc) // a Monday
	r.st.now = func() time.Time { return r.now }
	runner := &agent.Runner{Agents: map[string]*agent.Spec{"habakkuk": {Name: "habakkuk", Dir: dir, Workspace: dir}},
		Sessions: session.NewStore(transcripts), Queue: r.q, Log: quiet}
	r.sch = &Scheduler{Store: r.st, Queue: r.q, Runner: runner, Log: quiet,
		Deliver: func(_ context.Context, id string, to json.RawMessage, text string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.sent = append(r.sent, sent{id: id, text: text, to: to})
			return nil
		}}
	return r
}

func TestNextUsesTimezoneAndFormats(t *testing.T) {
	r := newRig(t)
	next, err := r.st.Next("0 9 * * 1-5", r.now)
	if err != nil || next.Hour() != 9 || next.Day() != 9 || next.Location().String() != "America/Toronto" {
		t.Fatalf("next = %v, %v", next, err)
	}
	if n, _ := r.st.Next("@every 2h", r.now); n.Sub(r.now) != 2*time.Hour {
		t.Fatalf("@every: %v", n)
	}
	if n, _ := r.st.Next("at 2026-11-09 10:30", r.now); n.Hour() != 10 || n.Minute() != 30 {
		t.Fatalf("at: %v", n)
	}
	if n, _ := r.st.Next("at 2026-11-01 10:30", r.now); !n.IsZero() {
		t.Fatalf("past one-off should have no next run: %v", n)
	}
	if _, err := r.st.Next("every tuesday", r.now); err == nil {
		t.Fatal("nonsense spec accepted")
	}
}

func TestSlotRunsOnceAcrossSchedulers(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	sc, err := r.st.Create(ctx, Schedule{Agent: "habakkuk", Name: "brief", Spec: "0 9 * * *", Prompt: "morning brief"})
	if err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(90 * time.Second) // 09:00:30
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ { // eight processes polling at once
		wg.Add(1)
		go func() { defer wg.Done(); r.sch.Tick(ctx) }()
	}
	wg.Wait()
	r.sch.Tick(ctx)
	var n int
	r.st.db.QueryRow(`SELECT count(*) FROM jobs WHERE kind = ?`, KindScheduled).Scan(&n)
	if n != 1 {
		t.Fatalf("%d jobs for one slot", n)
	}
	got, _ := r.st.Get(ctx, sc.ID)
	if got.NextRun.Day() != 10 || got.LastStatus != "started" {
		t.Fatalf("not advanced: %+v", got)
	}
}

func TestLateSlotIsSkipped(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	sc, _ := r.st.Create(ctx, Schedule{Agent: "habakkuk", Name: "brief", Spec: "0 9 * * *", Prompt: "p"})
	r.now = r.now.Add(3*24*time.Hour + 5*time.Hour) // down for three days
	if n, err := r.sch.Tick(ctx); err != nil || n != 0 {
		t.Fatalf("started %d, %v", n, err)
	}
	got, _ := r.st.Get(ctx, sc.ID)
	if !got.NextRun.After(r.now) || got.LastStatus == "" {
		t.Fatalf("late slot not skipped forward: next %v status %q", got.NextRun, got.LastStatus)
	}
}

func TestOneOffRunsThenDisables(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	to, _ := json.Marshal(map[string]string{"account": "tg", "conversation": "7"})
	sc, _ := r.st.Create(ctx, Schedule{Agent: "habakkuk", Name: "remind", Spec: "at 2026-11-09 09:30", Prompt: "call mum", DeliverTo: to})
	r.now = r.now.Add(32 * time.Minute)
	if n, _ := r.sch.Tick(ctx); n != 1 {
		t.Fatal("one-off did not start")
	}
	got, _ := r.st.Get(ctx, sc.ID)
	if got.Enabled {
		t.Fatal("one-off still enabled after running")
	}
	j, err := r.q.Claim(ctx, "w", time.Minute, KindScheduled)
	if err != nil || j == nil {
		t.Fatal("scheduled job not claimable")
	}
	turnID, err := r.sch.Handle(ctx, j, "w")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := r.sch.Handle(ctx, j, "w"); again != turnID {
		t.Fatal("rerun queued a second turn")
	}
	turn, _ := r.q.Get(ctx, turnID)
	var p agent.TurnPayload
	turn.Decode(&p)
	if string(p.Origin) != string(to) || p.Text == "" {
		t.Fatalf("turn payload: %+v", p)
	}
}
