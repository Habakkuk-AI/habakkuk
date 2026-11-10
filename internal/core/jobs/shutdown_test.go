package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"
)

// runUntilStarted starts a pool on q whose handler blocks until its context
// ends, waits for the job to be running, and returns a func that shuts the
// pool down and waits for it, as SIGTERM does to serve.
func runUntilStarted(t *testing.T, q *Queue) (shutdown func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 1)
	pool := &Pool{Queue: q, Concurrency: 1, Poll: 5 * time.Millisecond, Lease: time.Minute,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Handlers: map[string]Handler{"k": func(ctx context.Context, _ *Job, _ string) (string, error) {
			started <- struct{}{}
			<-ctx.Done()
			return "", ctx.Err()
		}}}
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the job never started")
	}
	return func() { cancel(); <-done }
}

// A planned stop hands the job back at once and does not count the run that
// was cut short, however many deploys land during one long task.
func TestGracefulShutdownDoesNotUseAnAttempt(t *testing.T) {
	ctx := context.Background()
	q := newQueue(t)
	id, _ := q.Enqueue(ctx, NewJob{Kind: "k", Agent: "a", MaxAttempts: 3})
	for deploy := 1; deploy <= 5; deploy++ {
		runUntilStarted(t, q)()
		j, err := q.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status != Pending || j.Attempts != 0 || j.LeaseOwner != "" {
			t.Fatalf("after shutdown %d: status %s, attempts %d, owner %q; want pending, 0 and no owner",
				deploy, j.Status, j.Attempts, j.LeaseOwner)
		}
	}
	// The next start picks it up without waiting for a lease to expire.
	j, err := q.Claim(ctx, "next", time.Minute, "k")
	if err != nil || j == nil || j.ID != id || j.Attempts != 1 {
		t.Fatalf("claim after shutdown = %+v, %v; want the job on attempt 1", j, err)
	}
}

// A released job keeps its place: a message queued behind it in the same
// conversation must not run first after the restart.
func TestReleasedJobKeepsItsPlaceInLine(t *testing.T) {
	ctx := context.Background()
	q := newQueue(t)
	now := time.Now()
	q.now = func() time.Time { return now }
	first, _ := q.Enqueue(ctx, NewJob{Kind: "k", Agent: "a", SerialKey: "s"})
	if j, _ := q.Claim(ctx, "w", time.Minute, "k"); j == nil || j.ID != first {
		t.Fatal("no claim")
	}
	now = now.Add(time.Second)
	if _, err := q.Enqueue(ctx, NewJob{Kind: "k", Agent: "a", SerialKey: "s"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := q.Release(ctx, first, "w"); err != nil {
		t.Fatal(err)
	}
	if j, _ := q.Claim(ctx, "w2", time.Minute, "k"); j == nil || j.ID != first {
		t.Fatalf("claimed %+v after the release, want the released job first", j)
	}
}

// Only the worker that holds the job can release it, and a job that was
// stopped (/stop) stays stopped.
func TestReleaseNeedsTheLease(t *testing.T) {
	ctx := context.Background()
	q := newQueue(t)
	id, _ := q.Enqueue(ctx, NewJob{Kind: "k", Agent: "a"})
	if j, _ := q.Claim(ctx, "w1", time.Minute, "k"); j == nil {
		t.Fatal("no claim")
	}
	if err := q.Release(ctx, id, "w2"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("release by another worker: %v", err)
	}
	if err := q.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := q.Release(ctx, id, "w1"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("release of a canceled job: %v", err)
	}
	if j, _ := q.Get(ctx, id); j.Status != Canceled {
		t.Fatalf("status %s, want canceled", j.Status)
	}
}

// A crash (or kill -9) releases nothing: the lease of the dead process is
// expired on the next start and that run still counts, so a job that keeps
// killing its worker fails after max_attempts.
func TestCrashStillCountsAsAnAttempt(t *testing.T) {
	ctx := context.Background()
	q := newQueue(t)
	id, _ := q.Enqueue(ctx, NewJob{Kind: "k", Agent: "a", MaxAttempts: 3})
	host, _, _ := parseOwner(OwnerID())
	for crash := 1; crash <= 3; crash++ {
		dead := fmt.Sprintf("worker:%s:%d:x%d", host, 1<<30, crash) // a pid that cannot be alive
		j, err := q.Claim(ctx, dead, time.Hour, "k")
		if err != nil || j == nil || j.Attempts != crash {
			t.Fatalf("crash %d: claim = %+v, %v; want attempt %d", crash, j, err, crash)
		}
		if n, err := q.ReleaseDead(ctx); err != nil || n != 1 {
			t.Fatalf("crash %d: released %d dead leases, %v", crash, n, err)
		}
	}
	if j, _ := q.Claim(ctx, "alive", time.Minute, "k"); j != nil {
		t.Fatalf("claimed %+v after three crashes, want none", j)
	}
	if j, _ := q.Get(ctx, id); j.Status != Failed {
		t.Fatalf("status %s after three crashes, want failed", j.Status)
	}
}
