package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// Backend runs an agent's shell commands: on this machine, in a container,
// or on a remote host.
type Backend interface {
	// Command prepares a shell (bash; PowerShell on a Windows host) running script in dir, a local path the backend
	// maps to wherever it runs. kill ends the whole process tree there and is
	// safe to call more than once or after the command exited. With stdin the
	// command's standard input stays open for writes; otherwise it is empty.
	// ctx bounds only the preparation (starting a container), never the
	// command.
	Command(ctx context.Context, script, dir string, stdin bool) (cmd *exec.Cmd, kill func(), err error)
	// Remote reports whether commands see other files than the local
	// workspace, so local file writes would not reach them.
	Remote() bool
	// Describe names where commands run, for messages.
	Describe() string
}

// Local runs commands on this machine with bash (PowerShell on Windows, see
// platform.ScriptCommand), each in its own process group so a timeout or
// kill ends everything it started.
type Local struct{}

func (Local) Remote() bool     { return false }
func (Local) Describe() string { return "this machine" }

func (Local) Command(_ context.Context, script, dir string, _ bool) (*exec.Cmd, func(), error) {
	cmd := platform.ScriptCommand(script)
	cmd.Dir = dir
	return cmd, GroupKiller(cmd), nil
}

// GroupKiller puts cmd in its own process group and returns a function that
// kills that group (the process tree on Windows) once cmd has started.
func GroupKiller(cmd *exec.Cmd) func() { return platform.GroupKiller(cmd) }

// localShell names the shell local commands use, for the tool description.
func localShell() string {
	if platform.IsWindows {
		return platform.ScriptShellName() + " (bash in a container or SSH sandbox)"
	}
	return "bash"
}

func backendOf(env *Env) Backend {
	if env.Backend != nil {
		return env.Backend
	}
	return Local{}
}

// maxCommandOutput bounds what a foreground command keeps in memory; the
// registry moves anything over its inline budget to a blob.
const maxCommandOutput = 16 << 20

// RunCommand runs script in dir and waits for it, killing the process tree
// when timeout passes or ctx ends. It returns the combined output and the
// exit code (-1 when killed).
func RunCommand(ctx context.Context, b Backend, script, dir string, timeout time.Duration) (string, int, error) {
	cmd, kill, err := b.Command(ctx, script, dir, false)
	if err != nil {
		return "", -1, err
	}
	out := &capBuffer{max: maxCommandOutput}
	cmd.Stdout, cmd.Stderr = out, out
	// A grandchild holding the pipe open must not hang Wait after the
	// command itself is gone.
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return "", -1, err
	}
	done := make(chan struct{})
	go func() { err = cmd.Wait(); close(done) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-ctx.Done():
		kill()
		<-done
		return out.String(), -1, ctx.Err()
	case <-timer.C:
		kill()
		<-done
		return out.String(), -1, fmt.Errorf("timed out after %s", timeout)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out.String(), exitErr.ExitCode(), nil
	}
	if err != nil {
		return out.String(), -1, err
	}
	return out.String(), 0, nil
}

// capBuffer keeps the first max bytes written and counts the rest.
type capBuffer struct {
	mu      sync.Mutex
	buf     []byte
	max     int
	dropped int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.max - len(c.buf)
	if room >= len(p) {
		c.buf = append(c.buf, p...)
	} else {
		c.buf = append(c.buf, p[:max(room, 0)]...)
		c.dropped += len(p) - max(room, 0)
	}
	return len(p), nil
}

func (c *capBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped > 0 {
		return fmt.Sprintf("%s\n[%d more bytes of output dropped]", c.buf, c.dropped)
	}
	return string(c.buf)
}

// exec

type execTool struct{}

type execInput struct {
	Command        string `json:"command"`
	Cwd            string `json:"cwd"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Background     bool   `json:"background"`
	Notify         bool   `json:"notify"`
}

func (execTool) Risk() Risk { return RiskExec }
func (execTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "exec",
		Description: "Run a shell command with " + localShell() + " in the workspace (or cwd). Returns combined stdout and stderr and the exit code. Default timeout 120s, max 1800s. " +
			"With background: true it returns a process id at once and keeps running (servers, watchers, long builds); read it with process_output, wait with process_wait, stop it with process_kill. " +
			"timeout_seconds then kills it after that long (default never). notify: true starts a new turn here when it exits.",
		Schema: schema(`{"type":"object","properties":{
			"command":{"type":"string"},
			"cwd":{"type":"string"},
			"timeout_seconds":{"type":"integer"},
			"background":{"type":"boolean","description":"run in the background and return a process id"},
			"notify":{"type":"boolean","description":"background only: tell this session when the process exits"}},"required":["command"]}`),
	}
}

// Mutates: a command run in the workspace may change it. Commands on a
// remote host never touch the local workspace.
func (execTool) Mutates(env *Env, input json.RawMessage) bool {
	in, err := decode[execInput](input)
	return err == nil && !env.remote() && env.InWorkspace(env.Resolve(in.Cwd))
}

func (execTool) Run(ctx context.Context, env *Env, input json.RawMessage) (string, error) {
	in, err := decode[execInput](input)
	if err != nil {
		return "", err
	}
	if in.Command == "" {
		return "", errors.New("command is empty")
	}
	dir := env.Resolve(in.Cwd)
	if in.Background {
		if env.Procs == nil {
			return "", errors.New("background processes are not available here")
		}
		var timeout time.Duration
		if in.TimeoutSeconds > 0 {
			timeout = min(time.Duration(in.TimeoutSeconds)*time.Second, 24*time.Hour)
		}
		p, err := env.Procs.Start(ctx, env, backendOf(env), in.Command, dir, timeout, in.Notify)
		if err != nil {
			return "", err
		}
		msg := fmt.Sprintf("started background process %s in %s", p.ID, dir)
		if in.Notify {
			msg += "; this session gets a message when it exits"
		}
		return msg + ". Read its output with process_output, wait with process_wait, stop it with process_kill.", nil
	}
	timeout := 120 * time.Second
	if in.TimeoutSeconds > 0 {
		timeout = min(time.Duration(in.TimeoutSeconds)*time.Second, 30*time.Minute)
	}
	text, code, err := RunCommand(ctx, backendOf(env), in.Command, dir, timeout)
	if err != nil {
		return text, err
	}
	if text == "" && code == 0 {
		text = "(no output)"
	}
	if code != 0 {
		return fmt.Sprintf("%s\n[exit code %d]", text, code), nil
	}
	return text + "\n[exit code 0]", nil
}
