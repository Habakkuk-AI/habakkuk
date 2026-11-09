package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/testutil"
)

type notice struct{ session, id, text string }

func procSetup(t *testing.T) (*Registry, *Env, *Procs, chan notice) {
	t.Helper()
	r, env := setup(t, ModeFull, nil)
	ps, err := NewProcs(filepath.Join(t.TempDir(), "procs"), nil)
	if err != nil {
		t.Fatal(err)
	}
	notes := make(chan notice, 10)
	ps.Notify = func(_ context.Context, session, id, text string) error {
		notes <- notice{session, id, text}
		return nil
	}
	t.Cleanup(ps.Close)
	env.Agent, env.Session, env.Procs = "habakkuk", "s1", ps
	return r, env, ps, notes
}

func run(t *testing.T, r *Registry, env *Env, name string, input any) llm.Block {
	t.Helper()
	return r.Execute(context.Background(), env, call(name, input))
}

var procIDRe = regexp.MustCompile(`p_[0-9a-f]{12}`)

func startBG(t *testing.T, r *Registry, env *Env, input map[string]any) string {
	t.Helper()
	input["background"] = true
	res := run(t, r, env, "exec", input)
	if res.IsError {
		t.Fatalf("start: %s", res.Text)
	}
	id := procIDRe.FindString(res.Text)
	if id == "" {
		t.Fatalf("no process id in %q", res.Text)
	}
	return id
}

func TestBackgroundProcessLifecycle(t *testing.T) {
	testutil.UnixOnly(t, "runs an sh script writing to stderr and checks LF output")
	r, env, _, notes := procSetup(t)
	id := startBG(t, r, env, map[string]any{"command": "echo hello; echo oops >&2; exit 3", "notify": true})

	res := run(t, r, env, "process_wait", map[string]any{"id": id, "timeout_seconds": 10})
	if res.IsError || !strings.Contains(res.Text, "exited with code 3") || !strings.Contains(res.Text, "hello") {
		t.Fatalf("wait: %+v", res)
	}
	res = run(t, r, env, "process_output", map[string]any{"id": id})
	if !strings.Contains(res.Text, "hello\noops\n") || !strings.Contains(res.Text, "[next since=11]") {
		t.Fatalf("output: %q", res.Text)
	}
	res = run(t, r, env, "process_output", map[string]any{"id": id, "since": 6})
	if !strings.Contains(res.Text, "oops") || strings.Contains(res.Text, "hello") {
		t.Fatalf("output since: %q", res.Text)
	}
	res = run(t, r, env, "process_list", map[string]any{})
	if !strings.Contains(res.Text, id) || !strings.Contains(res.Text, "exited with code 3") {
		t.Fatalf("list: %q", res.Text)
	}
	select {
	case n := <-notes:
		if n.session != "s1" || n.id != id || !strings.Contains(n.text, "exited with code 3") {
			t.Fatalf("notice: %+v", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no exit notice")
	}
}

func TestBackgroundProcessStdin(t *testing.T) {
	testutil.UnixOnly(t, "runs an sh read loop on stdin")
	r, env, _, _ := procSetup(t)

	id := startBG(t, r, env, map[string]any{"command": "while read l; do echo got:$l; done; echo bye"})
	if res := run(t, r, env, "process_write", map[string]any{"id": id, "stdin": "one\ntwo\n", "eof": true}); res.IsError {
		t.Fatal(res.Text)
	}
	res := run(t, r, env, "process_wait", map[string]any{"id": id, "timeout_seconds": 10})
	if !strings.Contains(res.Text, "exited with code 0") {
		t.Fatalf("wait: %q", res.Text)
	}
	res = run(t, r, env, "process_output", map[string]any{"id": id})
	if !strings.Contains(res.Text, "got:one\ngot:two\nbye") {
		t.Fatalf("output: %q", res.Text)
	}
}

func alive(pid int) bool { return platform.Alive(pid) }

func readPid(t *testing.T, path string) int {
	t.Helper()
	var pid int
	waitUntil(t, "pid file", func() bool {
		b, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil
	})
	return pid
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestKillEndsTheProcessTreeWithoutNotice(t *testing.T) {
	testutil.UnixOnly(t, "starts an sh background child ($!) to check the tree is killed")
	r, env, _, notes := procSetup(t)
	pidFile := filepath.Join(env.Workspace, "child.pid")
	id := startBG(t, r, env, map[string]any{"command": "sleep 60 & echo $! > child.pid; wait", "notify": true})
	child := readPid(t, pidFile)
	res := run(t, r, env, "process_kill", map[string]any{"id": id})
	if res.IsError || !strings.Contains(res.Text, "killed") {
		t.Fatalf("kill: %+v", res)
	}
	waitUntil(t, "child gone", func() bool { return !alive(child) })
	select {
	case n := <-notes:
		t.Fatalf("a killed process notified: %+v", n)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestBackgroundTimeoutNotifies(t *testing.T) {
	r, env, _, notes := procSetup(t)
	startBG(t, r, env, map[string]any{"command": "sleep 60", "timeout_seconds": 1, "notify": true})
	select {
	case n := <-notes:
		if !strings.Contains(n.text, "timed out") {
			t.Fatalf("notice: %q", n.text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no notice after timeout")
	}
}

func TestProcsAreTrackedPerAgent(t *testing.T) {
	r, env, ps, _ := procSetup(t)
	id := startBG(t, r, env, map[string]any{"command": "sleep 60"})
	other := &Env{Agent: "other", Workspace: env.Workspace, Mode: ModeFull, Procs: ps}
	if res := run(t, r, other, "process_output", map[string]any{"id": id}); !res.IsError {
		t.Fatalf("another agent read the process: %q", res.Text)
	}
	if res := run(t, r, other, "process_list", map[string]any{}); res.Text != "no background processes" {
		t.Fatalf("list: %q", res.Text)
	}
}

func TestCloseKillsProcesses(t *testing.T) {
	testutil.UnixOnly(t, "starts an sh background child ($!) to check the tree is killed")
	r, env, ps, notes := procSetup(t)
	pidFile := filepath.Join(env.Workspace, "child.pid")
	startBG(t, r, env, map[string]any{"command": "sleep 60 & echo $! > child.pid; wait", "notify": true})
	child := readPid(t, pidFile)
	ps.Close()
	waitUntil(t, "child gone", func() bool { return !alive(child) })
	if len(notes) != 0 {
		t.Fatal("shutdown sent a notice")
	}
	if res := run(t, r, env, "exec", map[string]any{"command": "true", "background": true}); !res.IsError {
		t.Fatal("started a process after Close")
	}
}

func TestRunningCap(t *testing.T) {
	r, env, ps, _ := procSetup(t)
	ps.MaxRunning = 1
	startBG(t, r, env, map[string]any{"command": "sleep 60"})
	res := run(t, r, env, "exec", map[string]any{"command": "sleep 60", "background": true})
	if !res.IsError || !strings.Contains(res.Text, "already running") {
		t.Fatalf("cap not applied: %+v", res)
	}
}

func TestBackgroundNeedsApprovalInWorkspaceMode(t *testing.T) {
	r, env, _, _ := procSetup(t)
	env.Mode = ModeWorkspace
	res := run(t, r, env, "exec", map[string]any{"command": "true", "background": true})
	if !res.IsError || !strings.Contains(res.Text, "denied") {
		t.Fatalf("background exec ran without approval: %+v", res)
	}
	// process_kill only stops the agent's own processes: no approval.
	env.Mode = ModeFull
	id := startBG(t, r, env, map[string]any{"command": "sleep 60"})
	env.Mode = ModeWorkspace
	if res := run(t, r, env, "process_kill", map[string]any{"id": id}); res.IsError {
		t.Fatalf("kill needed approval: %s", res.Text)
	}
}

func TestOutLogKeepsTheLatestOutput(t *testing.T) {
	l, err := newOutLog(filepath.Join(t.TempDir(), "p.log"), 10)
	if err != nil {
		t.Fatal(err)
	}
	defer l.remove()
	for i := range 4 {
		l.Write([]byte(strings.Repeat(strconv.Itoa(i), 9))) // 36 bytes, segments of 10
	}
	c, err := l.read(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	// Two segments are kept: offsets 20..36.
	if c.Total != 36 || c.Dropped != 20 || c.From != 20 || c.Next != 36 {
		t.Fatalf("chunk: %+v", c)
	}
	if string(c.Data) != strings.Repeat("2", 7)+strings.Repeat("3", 9) {
		t.Fatalf("data %q", c.Data)
	}
	c, _ = l.read(-5, 100)
	if string(c.Data) != "33333" || c.Next != 36 {
		t.Fatalf("tail: %+v", c)
	}
	c, _ = l.read(25, 4)
	if string(c.Data) != "2233" || c.Next != 29 {
		t.Fatalf("window across segments: %q next %d", c.Data, c.Next)
	}
}

func TestOutputIsConcurrencySafe(t *testing.T) {
	l, err := newOutLog(filepath.Join(t.TempDir(), "p.log"), 64)
	if err != nil {
		t.Fatal(err)
	}
	defer l.remove()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				l.Write([]byte("abcdefgh"))
				l.read(-20, 20)
			}
		}()
	}
	wg.Wait()
	if c, _ := l.read(0, 0); c.Total != 3200 {
		t.Fatalf("total %d", c.Total)
	}
}

func TestForegroundTimeoutKillsTree(t *testing.T) {
	testutil.UnixOnly(t, "starts an sh background child ($!) to check the tree is killed")
	r, env, _, _ := procSetup(t)

	start := time.Now()
	res := run(t, r, env, "exec", map[string]any{"command": "sleep 60 & echo $! > child.pid; wait", "timeout_seconds": 1})
	if !res.IsError || !strings.Contains(res.Text, "timed out") || time.Since(start) > 10*time.Second {
		t.Fatalf("timeout: %+v", res)
	}
	child := readPid(t, filepath.Join(env.Workspace, "child.pid"))
	waitUntil(t, "child gone", func() bool { return !alive(child) })
}

func TestExecInputDecodes(t *testing.T) {
	in, err := decode[execInput](json.RawMessage(`{"command":"x","background":true,"notify":true,"timeout_seconds":5}`))
	if err != nil || !in.Background || !in.Notify || in.TimeoutSeconds != 5 {
		t.Fatalf("%+v %v", in, err)
	}
}
