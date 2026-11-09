package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as a fake `habakkuk serve`: with FAKE_HABAKKUK set it
// behaves as the mode says instead of running tests.
func TestMain(m *testing.M) {
	if mode := os.Getenv("FAKE_HABAKKUK"); mode != "" {
		fakeHabakkuk(mode)
		return
	}
	os.Exit(m.Run())
}

func fakeHabakkuk(mode string) {
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM, os.Interrupt)
	if mode == "crash" {
		path := os.Getenv("FAKE_COUNT")
		data, _ := os.ReadFile(path)
		n, _ := strconv.Atoi(string(data))
		n++
		os.WriteFile(path, []byte(strconv.Itoa(n)), 0o600)
		if n < 3 {
			fmt.Println("boom", n)
			os.Exit(1)
		}
	}
	fmt.Fprintf(os.Stderr, "habakkuk test serving; home %s; args %v\n", os.Getenv("HABAKKUK_HOME"), os.Args[1:])
	fmt.Fprintln(os.Stderr, "http on http://127.0.0.1:9999")
	if mode == "stubborn" {
		signal.Ignore(syscall.SIGTERM, os.Interrupt)
		select {}
	}
	<-term
	fmt.Println("got TERM, bye")
	os.Exit(0)
}

func fake(t *testing.T, mode string, extra ...string) *Supervisor {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &Supervisor{
		Binary: self,
		Args:   []string{"serve"},
		Env:    append([]string{"FAKE_HABAKKUK=" + mode, "HABAKKUK_HOME=/fake/home"}, extra...),
		Grace:  2 * time.Second,
		Delay:  func(int) time.Duration { return 10 * time.Millisecond },
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func text(s *Supervisor) string {
	lines, _ := s.Lines(0)
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Text + "\n")
	}
	return b.String()
}

func TestStartLogsAddrStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("graceful stop uses SIGTERM")
	}
	var mu sync.Mutex
	var states []State
	s := fake(t, "serve")
	s.OnChange = func(st Status) { mu.Lock(); states = append(states, st.State); mu.Unlock() }
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil { // already running: a no-op
		t.Fatal(err)
	}
	waitFor(t, "the address", func() bool { return s.Status().Addr == "127.0.0.1:9999" })
	st := s.Status()
	if st.State != Running || st.PID == 0 {
		t.Fatalf("status = %+v", st)
	}
	out := text(s)
	if !strings.Contains(out, "home /fake/home") || !strings.Contains(out, "args [serve]") {
		t.Fatalf("output = %q", out)
	}
	_, next := s.Lines(0)
	if more, _ := s.Lines(next); len(more) != 0 {
		t.Fatalf("Lines(next) = %v", more)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text(s), "got TERM, bye") {
		t.Fatalf("the child was not asked to stop gracefully: %q", text(s))
	}
	if st := s.Status(); st.State != Stopped || st.PID != 0 || st.Addr != "" {
		t.Fatalf("after stop = %+v", st)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(states) < 3 || states[0] != Running || states[len(states)-1] != Stopped {
		t.Fatalf("state changes = %v", states)
	}
	if err := s.Stop(ctx); err != nil { // nothing running: a no-op
		t.Fatal(err)
	}
}

func TestRestartsAfterCrash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("graceful stop uses SIGTERM")
	}
	count := filepath.Join(t.TempDir(), "count")
	s := fake(t, "crash", "FAKE_COUNT="+count)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	// Two crashes, then the third start stays up.
	waitFor(t, "the third start", func() bool { return s.Status().Addr != "" })
	st := s.Status()
	if st.Restarts != 2 || st.State != Running || !strings.Contains(st.LastExit, "exit status 1") {
		t.Fatalf("status = %+v", st)
	}
	out := text(s)
	if !strings.Contains(out, "boom 1") || !strings.Contains(out, "boom 2") {
		t.Fatalf("output = %q", out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestKillsAfterGrace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("graceful stop uses SIGTERM")
	}
	s := fake(t, "stubborn")
	s.Grace = 300 * time.Millisecond
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "start", func() bool { return s.Status().Addr != "" })
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("stopped after %v, before the grace period", d)
	}
	if !strings.Contains(text(s), "killed") {
		t.Fatalf("output = %q", text(s))
	}
	if s.Status().State != Stopped {
		t.Fatalf("state = %v", s.Status().State)
	}
}

func TestStopDuringBackoff(t *testing.T) {
	count := filepath.Join(t.TempDir(), "count")
	s := fake(t, "crash", "FAKE_COUNT="+count)
	s.Delay = func(int) time.Duration { return time.Hour }
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "backoff", func() bool { return s.Status().State == Backoff })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Status().State != Stopped {
		t.Fatalf("state = %v", s.Status().State)
	}
}

func TestMissingBinary(t *testing.T) {
	s := &Supervisor{Binary: filepath.Join(t.TempDir(), "no-such-habakkuk")}
	if err := s.Start(); err == nil {
		t.Fatal("want an error")
	}
	if st := s.Status(); st.State != Stopped {
		t.Fatalf("state = %v", st.State)
	}
	if !strings.Contains(text(s), "could not start") {
		t.Fatalf("output = %q", text(s))
	}
	// The supervisor can be started again (here: fails again, cleanly).
	if err := s.Start(); err == nil {
		t.Fatal("want an error again")
	}
}

func TestLineLimitAndDelay(t *testing.T) {
	s := &Supervisor{MaxLines: 3}
	for i := range 5 {
		s.add(fmt.Sprint("line ", i))
	}
	lines, next := s.Lines(0)
	if len(lines) != 3 || lines[0].Text != "line 2" || next != 5 {
		t.Fatalf("lines = %v, next %d", lines, next)
	}
	if got, _ := s.Lines(4); len(got) != 1 || got[0].Text != "line 4" {
		t.Fatalf("Lines(4) = %v", got)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 30 * time.Second}
	for i, n := range []int{1, 2, 3, 10} {
		if d := s.delay(n); d != want[i] {
			t.Errorf("delay(%d) = %v, want %v", n, d, want[i])
		}
	}
}
