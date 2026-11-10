// Package supervisor runs `hk serve` as a child process: it starts it,
// keeps its output for the logs view, restarts it with a backoff when it
// exits on its own, and stops it gracefully (SIGTERM, then a kill after a
// grace period) when asked.
package supervisor

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// State is what the child is doing.
type State string

const (
	Stopped  State = "stopped"
	Running  State = "running"
	Backoff  State = "backoff" // exited on its own; waiting to start again
	Stopping State = "stopping"
)

// Line is one line of the child's output.
type Line struct {
	N    int64     `json:"n"`
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// Status is a snapshot for the UI.
type Status struct {
	State    State  `json:"state"`
	PID      int    `json:"pid,omitempty"`
	Addr     string `json:"addr,omitempty"` // the address serve printed ("http on http://ADDR")
	Restarts int    `json:"restarts"`
	LastExit string `json:"last_exit,omitempty"`
}

// Supervisor runs one child. The zero value is not usable; set Binary.
type Supervisor struct {
	Binary string
	Args   []string // default: serve
	Env    []string // added to the app's environment
	Dir    string

	// Grace is how long Stop waits after SIGTERM before killing (default 10s).
	Grace time.Duration
	// MaxLines is how many output lines are kept (default 2000).
	MaxLines int
	// Delay returns the wait before restart n (1, 2, ...). Default: 1s
	// doubling to 30s.
	Delay func(n int) time.Duration
	// Healthy is how long a run must last for the restart count to reset
	// (default 30s).
	Healthy time.Duration
	// OnChange is called (on its own goroutine's time, not under a lock)
	// whenever the status changes.
	OnChange func(Status)
	Log      *slog.Logger

	mu       sync.Mutex
	st       Status
	cmd      *exec.Cmd
	lines    []Line
	next     int64
	stop     chan struct{} // closed by Stop
	done     chan struct{} // closed when the run loop ends
	changeMu sync.Mutex
}

// Start launches the child unless it is already supervised. It returns the
// error of the first start (a missing binary, say); later restarts are
// reported through the status.
func (s *Supervisor) Start() error {
	s.mu.Lock()
	if s.done != nil {
		s.mu.Unlock()
		return nil
	}
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	s.st.Restarts = 0
	first := make(chan error, 1)
	stop, done := s.stop, s.done
	s.mu.Unlock()
	go s.loop(stop, done, first)
	if err := <-first; err != nil {
		<-done
		return err
	}
	return nil
}

// Stop ends the child and waits for it. It is a no-op when nothing runs.
func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.done == nil {
		s.mu.Unlock()
		return nil
	}
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	done := s.done
	s.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Status returns a snapshot.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	if st.State == "" {
		st.State = Stopped
	}
	return st
}

// Lines returns the kept output after line n (0 for all) and the number to
// pass next time.
func (s *Supervisor) Lines(after int64) ([]Line, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Line
	for _, l := range s.lines {
		if l.N > after {
			out = append(out, l)
		}
	}
	return out, s.next
}

func (s *Supervisor) loop(stop, done chan struct{}, first chan<- error) {
	defer func() {
		s.mu.Lock()
		s.done, s.cmd = nil, nil
		s.st.PID, s.st.Addr = 0, ""
		s.st.State = Stopped
		s.mu.Unlock()
		close(done)
		s.changed()
	}()
	restarts := 0
	for {
		started := time.Now()
		cmd, wait, err := s.spawn()
		if first != nil {
			first <- err
			first = nil
		}
		if err != nil {
			s.note("could not start " + s.Binary + ": " + err.Error())
			return
		}
		var exitErr error
		select {
		case exitErr = <-wait:
		case <-stop:
			s.terminate(cmd, wait)
			return
		}
		msg := "exited"
		if exitErr != nil {
			msg = exitErr.Error()
		}
		s.note("hk serve " + msg)
		if time.Since(started) >= s.healthy() {
			restarts = 0
		}
		restarts++
		s.mu.Lock()
		s.cmd = nil
		s.st.PID, s.st.Addr = 0, ""
		s.st.State, s.st.Restarts, s.st.LastExit = Backoff, s.st.Restarts+1, msg
		s.mu.Unlock()
		s.changed()
		select {
		case <-time.After(s.delay(restarts)):
		case <-stop:
			return
		}
	}
}

func (s *Supervisor) spawn() (*exec.Cmd, <-chan error, error) {
	args := s.Args
	if args == nil {
		args = []string{"serve"}
	}
	cmd := exec.Command(s.Binary, args...)
	cmd.Dir = s.Dir
	cmd.Env = append(os.Environ(), s.Env...)
	cmd.SysProcAttr = sysProcAttr()
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return nil, nil, err
	}
	s.mu.Lock()
	s.cmd = cmd
	s.st.State, s.st.PID, s.st.Addr = Running, cmd.Process.Pid, ""
	s.mu.Unlock()
	s.changed()
	read := make(chan struct{})
	go func() {
		defer close(read)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			s.add(sc.Text())
		}
		_, _ = io.Copy(io.Discard, pr)
	}()
	wait := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		_ = pw.Close()
		<-read
		wait <- err
	}()
	return cmd, wait, nil
}

// terminate asks the child to stop, kills it after the grace period, and
// waits for it to be gone.
func (s *Supervisor) terminate(cmd *exec.Cmd, wait <-chan error) {
	s.mu.Lock()
	s.st.State = Stopping
	s.mu.Unlock()
	s.changed()
	if err := interrupt(cmd); err != nil {
		kill(cmd)
	}
	grace := s.Grace
	if grace <= 0 {
		grace = 10 * time.Second
	}
	select {
	case err := <-wait:
		s.note(exitNote(err))
	case <-time.After(grace):
		s.note("hk serve did not stop in time; killed")
		kill(cmd)
		if err := <-wait; err != nil {
			s.note(exitNote(err))
		}
	}
}

func (s *Supervisor) add(text string) {
	s.mu.Lock()
	s.next++
	s.lines = append(s.lines, Line{N: s.next, At: time.Now(), Text: text})
	max := s.MaxLines
	if max <= 0 {
		max = 2000
	}
	if len(s.lines) > max {
		s.lines = append(s.lines[:0:0], s.lines[len(s.lines)-max:]...)
	}
	changed := false
	if addr, ok := strings.CutPrefix(text, "http on http://"); ok && s.st.Addr == "" {
		s.st.Addr = strings.TrimSpace(addr)
		changed = true
	}
	s.mu.Unlock()
	if changed {
		s.changed()
	}
}

// note adds a line of the supervisor's own to the log.
func (s *Supervisor) note(text string) {
	if s.Log != nil {
		s.Log.Info(text)
	}
	s.add("[desktop] " + text)
}

func (s *Supervisor) changed() {
	if s.OnChange == nil {
		return
	}
	s.changeMu.Lock() // in order, one at a time
	defer s.changeMu.Unlock()
	s.OnChange(s.Status())
}

func (s *Supervisor) delay(n int) time.Duration {
	if s.Delay != nil {
		return s.Delay(n)
	}
	d := time.Second
	for i := 1; i < n && d < 30*time.Second; i++ {
		d *= 2
	}
	return min(d, 30*time.Second)
}

func (s *Supervisor) healthy() time.Duration {
	if s.Healthy > 0 {
		return s.Healthy
	}
	return 30 * time.Second
}

func exitNote(err error) string {
	if err == nil {
		return "hk serve stopped"
	}
	return "hk serve stopped: " + err.Error()
}
