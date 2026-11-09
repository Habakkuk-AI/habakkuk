package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// limitErr says retrying cannot help, the way a refused spending limit does.
type limitErr struct{}

func (limitErr) Error() string   { return "limit reached" }
func (limitErr) Permanent() bool { return true }

func TestPermanentErrorsAreNotRetried(t *testing.T) {
	if !IsPermanent(Permanent{Err: errors.New("x")}) || !IsPermanent(fmt.Errorf("wrapped: %w", limitErr{})) ||
		IsPermanent(errors.New("transient")) {
		t.Fatal("IsPermanent")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := newQueue(t)
	var mu sync.Mutex
	calls := 0
	pool := &Pool{Queue: q, Concurrency: 1, Poll: 10 * time.Millisecond, Handlers: map[string]Handler{
		"k": func(ctx context.Context, j *Job, owner string) (string, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return "", fmt.Errorf("turn: %w", limitErr{})
		},
	}}
	id, _ := q.Enqueue(ctx, NewJob{Kind: "k", Agent: "a"})
	q.db.Exec(`UPDATE jobs SET run_at = 0 WHERE id = ?`, id)
	go pool.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j, _ := q.Get(ctx, id); j.Status == Failed {
			q.db.Exec(`UPDATE jobs SET run_at = 0 WHERE id = ?`, id) // a retry would now be due
			pool.Wake()
			time.Sleep(100 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			if j, _ := q.Get(ctx, id); calls != 1 || j.Attempts != 1 || j.Status != Failed {
				t.Fatalf("ran %d times, %d attempts, %s", calls, j.Attempts, j.Status)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job not failed permanently")
}
