package channel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// flaky fails its first runs, then connects and beats until canceled.
type flaky struct {
	runs     atomic.Int32
	failures int32
	hang     bool // when true, the healthy run never beats
	stale    time.Duration
}

func (f *flaky) Run(ctx context.Context, in Inbox) error {
	n := f.runs.Add(1)
	if n <= f.failures {
		return errors.New("boom")
	}
	in.Connected("ok")
	if f.hang && n == f.failures+1 {
		<-ctx.Done() // half-open socket: alive but silent
		return nil
	}
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			in.Beat()
			in.Deliver(ctx, Message{ID: "m"})
		}
	}
}
func (f *flaky) Send(context.Context, Address, string, string) error { return nil }
func (f *flaky) Format(s string) string                              { return s }
func (f *flaky) MaxLen() int                                         { return 100 }
func (f *flaky) StaleAfter() time.Duration                           { return f.stale }

func runSupervisor(t *testing.T, a Adapter, d time.Duration) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var mu sync.Mutex
	got, account := 0, ""
	s := &Supervisor{Log: quiet, MinBackoff: 5 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, Check: 5 * time.Millisecond, Grace: time.Second}
	s.Run(ctx, "acct", a, func(_ context.Context, m Message) {
		mu.Lock()
		got++
		account = m.Account
		mu.Unlock()
	})
	mu.Lock()
	defer mu.Unlock()
	return got, account
}

func TestSupervisorRestartsFailedAdapter(t *testing.T) {
	f := &flaky{failures: 3, stale: time.Second}
	got, account := runSupervisor(t, f, 300*time.Millisecond)
	if f.runs.Load() < 4 || got == 0 {
		t.Fatalf("runs=%d delivered=%d", f.runs.Load(), got)
	}
	if account != "acct" {
		t.Fatalf("account = %q", account)
	}
}

func TestSupervisorRestartsSilentAdapter(t *testing.T) {
	f := &flaky{hang: true, stale: 40 * time.Millisecond}
	got, _ := runSupervisor(t, f, 400*time.Millisecond)
	if f.runs.Load() < 2 || got == 0 {
		t.Fatalf("hung adapter was not restarted: runs=%d delivered=%d", f.runs.Load(), got)
	}
}

type fatal struct{ runs atomic.Int32 }

func (f *fatal) Run(context.Context, Inbox) error {
	f.runs.Add(1)
	return Fatal{Err: errors.New("bad token")}
}
func (f *fatal) Send(context.Context, Address, string, string) error { return nil }
func (f *fatal) Format(s string) string                              { return s }
func (f *fatal) MaxLen() int                                         { return 100 }
func (f *fatal) StaleAfter() time.Duration                           { return time.Second }

func TestSupervisorBacksOffOnFatal(t *testing.T) {
	f := &fatal{}
	runSupervisor(t, f, 120*time.Millisecond) // max backoff is 50ms
	if n := f.runs.Load(); n > 4 {
		t.Fatalf("fatal adapter retried %d times; want max backoff between tries", n)
	}
}
