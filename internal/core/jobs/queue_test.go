package jobs

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

func newQueue(t *testing.T) *Queue {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "control.db"), Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewQueue(db)
}

func TestClaimCompleteLifecycle(t *testing.T) {
	ctx := context.Background()
	q := newQueue(t)
	id, err := q.Enqueue(ctx, NewJob{Kind: "k", Agent: "a", Payload: map[string]int{"n": 1}})
	if err != nil {
		t.Fatal(err)
	}
	j, err := q.Claim(ctx, "w1", time.Minute, "k")
	if err != nil || j == nil || j.ID != id || j.Attempts != 1 {
		t.Fatalf("claim = %+v, %v", j, err)
	}
	if again, _ := q.Claim(ctx, "w2", time.Minute, "k"); again != nil {
		t.Fatal("job claimed twice while leased")
	}
	if err := q.Complete(ctx, id, "w2", ""); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("non-owner complete: %v", err)
	}
	if err := q.Complete(ctx, id, "w1", "ok"); err != nil {
		t.Fatal(err)
	}
	got, _ := q.Get(ctx, id)
	if got.Status != Done || got.Result != "ok" {
		t.Fatalf("status %s result %q", got.Status, got.Result)
	}
}

func TestExpiredLeaseIsReclaimed(t *testing.T) {
	ctx := context.Background()
	q := newQueue(t)
	now := time.Now()
	q.now = func() time.Time { return now }
	id, _ := q.Enqueue(ctx, NewJob{Kind: "k", Agent: "a"})
	if j, _ := q.Claim(ctx, "dead", time.Second, "k"); j == nil {
		t.Fatal("no claim")
	}
	now = now.Add(2 * time.Second)
	j, err := q.Claim(ctx, "alive", time.Minute, "k")
	if err != nil || j == nil || j.ID != id || j.Attempts != 2 {
		t.Fatalf("reclaim = %+v, %v", j, err)
	}
	if err := q.Owns(ctx, id, "dead"); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("dead worker still owns the job")
	}
}

func TestSerialKeyPreventsConcurrentRuns(t *testing.T) {
	ctx := context.Background()
	q := newQueue(t)
	a, _ := q.Enqueue(ctx, NewJob{Kind: "k", Agent: "a", SerialKey: "s1"})
	b, _ := q.Enqueue(ctx, NewJob{Kind: "k", Agent: "a", SerialKey: "s1"})
	c, _ := q.Enqueue(ctx, NewJob{Kind: "k", Agent: "a", SerialKey: "s2"})
	j1, _ := q.Claim(ctx, "w1", time.Minute, "k")
	j2, _ := q.Claim(ctx, "w2", time.Minute, "k")
	if j1 == nil || j2 == nil || j1.ID != a || j2.ID != c {
		t.Fatalf("got %v %v, want %s then %s", j1, j2, a, c)
	}
	if j3, _ := q.Claim(ctx, "w3", time.Minute, "k"); j3 != nil {
		t.Fatalf("claimed %s while s1 busy", j3.ID)
	}
	q.Complete(ctx, a, "w1", "")
	if j3, _ := q.Claim(ctx, "w3", time.Minute, "k"); j3 == nil || j3.ID != b {
		t.Fatal("second s1 job not runnable after first finished")
	}
}

func TestConcurrentClaimsNeverDuplicate(t *testing.T) {
	ctx := context.Background()
	q := newQueue(t)
	const n = 50
	for i := 0; i < n; i++ {
		q.Enqueue(ctx, NewJob{Kind: "k", Agent: "a"})
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 10; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner := fmt.Sprintf("w%d", w)
			for {
				j, err := q.Claim(ctx, owner, time.Minute, "k")
				if err != nil {
					t.Error(err)
					return
				}
				if j == nil {
					return
				}
				mu.Lock()
				seen[j.ID]++
				mu.Unlock()
				q.Complete(ctx, j.ID, owner, "")
			}
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("claimed %d distinct jobs, want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("job %s claimed %d times", id, c)
		}
	}
}

func TestRetryThenFailAfterMaxAttempts(t *testing.T) {
	ctx := context.Background()
	q := newQueue(t)
	id, _ := q.Enqueue(ctx, NewJob{Kind: "k", Agent: "a", MaxAttempts: 2})
	for i := 0; i < 2; i++ {
		j, _ := q.Claim(ctx, "w", time.Minute, "k")
		if j == nil {
			t.Fatalf("attempt %d not claimable", i+1)
		}
		q.Fail(ctx, id, "w", errors.New("boom"), true, 0)
	}
	got, _ := q.Get(ctx, id)
	if got.Status != Failed || got.LastError != "boom" {
		t.Fatalf("status %s err %q", got.Status, got.LastError)
	}
}

func TestExhaustedExpiredJobFailsAndFreesSerialKey(t *testing.T) {
	ctx := context.Background()
	q := newQueue(t)
	now := time.Now()
	q.now = func() time.Time { return now }
	stuck, _ := q.Enqueue(ctx, NewJob{Kind: "k", Agent: "a", SerialKey: "s", MaxAttempts: 1})
	next, _ := q.Enqueue(ctx, NewJob{Kind: "k", Agent: "a", SerialKey: "s"})
	q.Claim(ctx, "dead", time.Second, "k")
	now = now.Add(time.Hour)
	j, _ := q.Claim(ctx, "w", time.Minute, "k")
	if j == nil || j.ID != next {
		t.Fatalf("got %v, want %s", j, next)
	}
	if got, _ := q.Get(ctx, stuck); got.Status != Failed {
		t.Fatalf("stuck job status %s", got.Status)
	}
}

func TestReleaseDeadOwner(t *testing.T) {
	ctx := context.Background()
	q := newQueue(t)
	id, _ := q.Enqueue(ctx, NewJob{Kind: "k", Agent: "a"})
	// A pid that cannot be alive on this host.
	host, _, _ := parseOwner(OwnerID())
	dead := fmt.Sprintf("worker:%s:%d:x", host, 1<<30)
	if j, _ := q.Claim(ctx, dead, time.Hour, "k"); j == nil {
		t.Fatal("no claim")
	}
	n, err := q.ReleaseDead(ctx)
	if err != nil || n != 1 {
		t.Fatalf("released %d, %v", n, err)
	}
	if j, _ := q.Claim(ctx, OwnerID(), time.Minute, "k"); j == nil || j.ID != id {
		t.Fatal("dead owner's job not reclaimable immediately")
	}
}

func TestPoolRunsAndRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := newQueue(t)
	var mu sync.Mutex
	calls := 0
	done := make(chan struct{})
	pool := &Pool{Queue: q, Concurrency: 2, Poll: 10 * time.Millisecond, Handlers: map[string]Handler{
		"k": func(ctx context.Context, j *Job, owner string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls == 1 {
				return "", errors.New("transient")
			}
			close(done)
			return "ok", nil
		},
	}}
	id, _ := q.Enqueue(ctx, NewJob{Kind: "k", Agent: "a"})
	q.db.Exec(`UPDATE jobs SET run_at = 0 WHERE id = ?`, id)
	go pool.Run(ctx)
	// The retry backoff is 2s after the first attempt; pull it forward.
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
				q.db.Exec(`UPDATE jobs SET run_at = 0 WHERE id = ? AND status = 'pending'`, id)
				pool.Wake()
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("job never succeeded")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if j, _ := q.Get(ctx, id); j.Status == Done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job not marked done")
}
