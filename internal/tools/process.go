package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Procs tracks background processes per agent. Output goes to files under
// Dir, capped: each process keeps at most two segments of MaxOutput bytes,
// so a chatty server keeps its latest output and never fills the disk.
// Processes live as long as this Habakkuk process; Close kills them all.
type Procs struct {
	Dir        string
	MaxOutput  int64 // bytes per output segment; default 4 MiB
	MaxRunning int   // running processes per agent; default 16
	MaxKept    int   // finished processes remembered per agent; default 50
	// Notify starts a turn in session saying the process exited; nil
	// disables notify.
	Notify func(ctx context.Context, session, procID, text string) error
	Log    *slog.Logger

	mu     sync.Mutex
	procs  map[string]*Proc
	closed bool
}

// Proc is one background process.
type Proc struct {
	ID      string
	Agent   string
	Session string
	Command string
	Dir     string
	Started time.Time

	notify bool
	cmd    *exec.Cmd
	kill   func()
	stdin  io.WriteCloser
	out    *outLog
	done   chan struct{}

	mu     sync.Mutex
	ended  time.Time
	code   int
	reason string // "killed", "timed out", or a wait error
}

// NewProcs prepares the output directory, removing logs a previous run
// left behind more than a day ago.
func NewProcs(dir string, log *slog.Logger) (*Procs, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Procs{Dir: dir, Log: log}, nil
}

func (ps *Procs) limits() (seg int64, running, kept int) {
	seg, running, kept = ps.MaxOutput, ps.MaxRunning, ps.MaxKept
	if seg <= 0 {
		seg = 4 << 20
	}
	if running <= 0 {
		running = 16
	}
	if kept <= 0 {
		kept = 50
	}
	return
}

func newProcID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "p_" + hex.EncodeToString(b)
}

// Start runs command in the background for env's agent.
func (ps *Procs) Start(ctx context.Context, env *Env, b Backend, command, dir string, timeout time.Duration, notify bool) (*Proc, error) {
	seg, maxRunning, _ := ps.limits()
	ps.mu.Lock()
	if ps.closed {
		ps.mu.Unlock()
		return nil, errors.New("shutting down")
	}
	if n := ps.running(env.Agent); n >= maxRunning {
		ps.mu.Unlock()
		return nil, fmt.Errorf("%d background processes are already running; stop one with process_kill first", n)
	}
	ps.mu.Unlock()

	cmd, kill, err := b.Command(ctx, command, dir, true)
	if err != nil {
		return nil, err
	}
	p := &Proc{ID: newProcID(), Agent: env.Agent, Session: env.Session, Command: command, Dir: dir,
		notify: notify, cmd: cmd, kill: kill, done: make(chan struct{})}
	if p.out, err = newOutLog(filepath.Join(ps.Dir, p.ID+".log"), seg); err != nil {
		return nil, err
	}
	if p.stdin, err = cmd.StdinPipe(); err != nil {
		p.out.remove()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = p.out, p.out
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		p.out.remove()
		return nil, err
	}
	p.Started = time.Now()
	ps.mu.Lock()
	if ps.procs == nil {
		ps.procs = map[string]*Proc{}
	}
	ps.procs[p.ID] = p
	ps.prune(env.Agent)
	ps.mu.Unlock()

	var timer *time.Timer
	if timeout > 0 {
		timer = time.AfterFunc(timeout, func() { p.stop("timed out after " + timeout.String()) })
	}
	go func() {
		err := cmd.Wait()
		if timer != nil {
			timer.Stop()
		}
		p.mu.Lock()
		p.ended = time.Now()
		p.code = 0
		var exitErr *exec.ExitError
		switch {
		case errors.As(err, &exitErr):
			p.code = exitErr.ExitCode()
		case err != nil:
			p.code = -1
			if p.reason == "" {
				p.reason = err.Error()
			}
		}
		killed := p.reason == "killed"
		p.mu.Unlock()
		p.out.close()
		close(p.done)
		ps.exited(p, killed)
	}()
	return p, nil
}

// exited tells the session, when asked to, that the process ended. A process
// the agent killed itself, or one ended by shutdown, says nothing.
func (ps *Procs) exited(p *Proc, killed bool) {
	ps.mu.Lock()
	closed := ps.closed
	ps.mu.Unlock()
	if !p.notify || killed || closed || ps.Notify == nil || p.Session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ps.Notify(ctx, p.Session, p.ID, p.exitNote()); err != nil {
		ps.Log.Warn("process exit notice failed", "process", p.ID, "session", p.Session, "err", err)
	}
}

func (p *Proc) exitNote() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	how := fmt.Sprintf("exited with code %d", p.code)
	if p.reason != "" {
		how = fmt.Sprintf("was stopped (%s, code %d)", p.reason, p.code)
	}
	return fmt.Sprintf("[Background process %s (%s) %s after %s. Read its output with process_output.]",
		p.ID, oneLine(p.Command, 80), how, p.ended.Sub(p.Started).Round(time.Second))
}

// stop kills the process tree and records why.
func (p *Proc) stop(reason string) {
	p.mu.Lock()
	if p.ended.IsZero() && p.reason == "" {
		p.reason = reason
	}
	p.mu.Unlock()
	p.kill()
}

// Running reports whether the process has not exited yet.
func (p *Proc) Running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Status is one line: running, or how it ended.
func (p *Proc) Status() string {
	if p.Running() {
		return fmt.Sprintf("running for %s", time.Since(p.Started).Round(time.Second))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := fmt.Sprintf("exited with code %d after %s", p.code, p.ended.Sub(p.Started).Round(time.Second))
	if p.reason != "" {
		s += " (" + p.reason + ")"
	}
	return s
}

func (ps *Procs) running(agent string) int {
	n := 0
	for _, p := range ps.procs {
		if p.Agent == agent && p.Running() {
			n++
		}
	}
	return n
}

// prune forgets the oldest finished processes of agent past the limit and
// removes their output. ps.mu is held.
func (ps *Procs) prune(agent string) {
	_, _, kept := ps.limits()
	var done []*Proc
	for _, p := range ps.procs {
		if p.Agent == agent && !p.Running() {
			done = append(done, p)
		}
	}
	if len(done) <= kept {
		return
	}
	sort.Slice(done, func(i, j int) bool { return done[i].Started.Before(done[j].Started) })
	for _, p := range done[:len(done)-kept] {
		delete(ps.procs, p.ID)
		p.out.remove()
	}
}

// Get finds one of agent's processes.
func (ps *Procs) Get(agent, id string) (*Proc, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p, ok := ps.procs[strings.TrimSpace(id)]
	if !ok || p.Agent != agent {
		return nil, fmt.Errorf("no background process %q (process_list shows them)", id)
	}
	return p, nil
}

// List returns agent's processes, oldest first.
func (ps *Procs) List(agent string) []*Proc {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	var out []*Proc
	for _, p := range ps.procs {
		if p.Agent == agent {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// Kill stops a process and waits briefly for it to go.
func (p *Proc) Kill(wait time.Duration) bool {
	p.stop("killed")
	select {
	case <-p.done:
		return true
	case <-time.After(wait):
		return false
	}
}

// Wait waits for the process to exit, up to d or until ctx ends.
func (p *Proc) Wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-p.done:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// Write sends text to the process's standard input; eof closes it after.
func (p *Proc) Write(text string, eof bool) error {
	if !p.Running() {
		return errors.New("the process has exited")
	}
	if text != "" {
		if _, err := io.WriteString(p.stdin, text); err != nil {
			return err
		}
	}
	if eof {
		return p.stdin.Close()
	}
	return nil
}

// Output reads the process's output from byte offset since; a negative since
// reads the last -since bytes.
func (p *Proc) Output(since int64, limit int) (Chunk, error) {
	return p.out.read(since, limit)
}

// Close kills every process and removes their output. Nothing is notified.
func (ps *Procs) Close() {
	ps.mu.Lock()
	ps.closed = true
	all := make([]*Proc, 0, len(ps.procs))
	for _, p := range ps.procs {
		all = append(all, p)
	}
	ps.procs = nil
	ps.mu.Unlock()
	for _, p := range all {
		if p.Running() {
			p.stop("shutdown")
		}
	}
	for _, p := range all {
		p.Wait(context.Background(), 3*time.Second)
		p.out.remove()
	}
}

// outLog is a capped output file in two segments: when the current one is
// full it becomes the previous one and a new one starts. Offsets are counted
// over everything ever written, so readers can resume where they left off.
type outLog struct {
	mu        sync.Mutex
	path      string // current segment; the previous one is path + ".1"
	f         *os.File
	seg       int64
	total     int64 // bytes ever written
	curStart  int64 // offset where the current segment begins
	prevStart int64 // offset where the previous segment begins; -1 if none
}

func newOutLog(path string, seg int64) (*outLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &outLog{path: path, f: f, seg: seg, prevStart: -1}, nil
}

func (l *outLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		if l.f == nil {
			return n, nil // closed: drop
		}
		room := l.seg - (l.total - l.curStart)
		if room <= 0 {
			if err := l.rotate(); err != nil {
				return n, err
			}
			continue
		}
		k := int(min(room, int64(len(p))))
		if _, err := l.f.Write(p[:k]); err != nil {
			return n, err
		}
		l.total += int64(k)
		p = p[k:]
	}
	return n, nil
}

func (l *outLog) rotate() error {
	l.f.Close()
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		l.f = nil
		return err
	}
	l.f, l.prevStart, l.curStart = f, l.curStart, l.total
	return nil
}

func (l *outLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}

func (l *outLog) remove() {
	l.close()
	os.Remove(l.path)
	os.Remove(l.path + ".1")
}

// Chunk is a slice of a process's output.
type Chunk struct {
	Data    []byte
	From    int64 // offset of Data[0]
	Next    int64 // offset to read from next
	Total   int64 // bytes written so far
	Dropped int64 // bytes asked for that were already discarded by the cap
}

func (l *outLog) read(since int64, limit int) (Chunk, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if since < 0 {
		since = max(l.total+since, 0)
	}
	earliest := l.curStart
	if l.prevStart >= 0 {
		earliest = l.prevStart
	}
	c := Chunk{Total: l.total}
	if since < earliest {
		c.Dropped = earliest - since
		since = earliest
	}
	since = min(since, l.total)
	c.From = since
	end := min(since+int64(limit), l.total)
	for pos := since; pos < end; {
		path, base, segEnd := l.path, l.curStart, l.total
		if pos < l.curStart {
			path, base, segEnd = l.path+".1", l.prevStart, l.curStart
		}
		n := min(end, segEnd) - pos
		buf := make([]byte, n)
		f, err := os.Open(path)
		if err != nil {
			return c, err
		}
		k, err := f.ReadAt(buf, pos-base)
		f.Close()
		c.Data = append(c.Data, buf[:k]...)
		if err != nil && !errors.Is(err, io.EOF) {
			return c, err
		}
		if int64(k) < n {
			break
		}
		pos += n
	}
	c.Next = c.From + int64(len(c.Data))
	return c, nil
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "..."
	}
	return s
}
