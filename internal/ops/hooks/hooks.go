// Package hooks runs user-configured commands on Habakkuk's events: turns
// starting and ending, tool calls, messages in and out, new sessions and
// scheduled runs. A hook gets the event as JSON on stdin and HABAKKUK_EVENT in
// its environment. Only tool_before is waited for: a non-zero exit blocks
// the call, and its stdout is the reason the model sees. Every other event
// runs its hooks in the background, so a slow hook never delays a turn.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
)

// Veto is the one event whose hooks are waited for and can block.
const Veto = "tool_before"

// DefaultTimeout bounds a hook that sets none.
const DefaultTimeout = 10 * time.Second

// maxOutput caps what is kept of a hook's stdout and stderr.
const maxOutput = 16 << 10

type hook struct {
	event, command string
	timeout        time.Duration
	match          *regexp.Regexp
}

// Runner fires hooks. Its hook list can be swapped while it runs (config
// hot reload).
type Runner struct {
	Home string // working directory and HABAKKUK_HOME for hook commands
	Log  *slog.Logger

	hooks atomic.Pointer[[]hook]
	wg    sync.WaitGroup
}

// New returns a runner for the configured hooks.
func New(hs []*config.Hook, home string, log *slog.Logger) *Runner {
	r := &Runner{Home: home, Log: log}
	r.Set(hs)
	return r
}

// Set replaces the hook list. Entries are assumed validated (config.Load).
func (r *Runner) Set(hs []*config.Hook) {
	list := make([]hook, 0, len(hs))
	for _, h := range hs {
		if h == nil {
			continue
		}
		m, err := regexp.Compile(h.Match)
		if err != nil {
			continue
		}
		t := h.Timeout
		if t <= 0 {
			t = DefaultTimeout
		}
		list = append(list, hook{event: h.Event, command: h.Command, timeout: t, match: m})
	}
	r.hooks.Store(&list)
}

// Count is how many hooks are configured.
func (r *Runner) Count() int {
	if p := r.hooks.Load(); p != nil {
		return len(*p)
	}
	return 0
}

// Fire runs the hooks for event whose match accepts subject. For
// tool_before it runs them in order, waits, and returns the first veto's
// reason; a hook that cannot run or times out also vetoes, so a guard never
// fails open. For other events it starts them in the background and
// returns "".
func (r *Runner) Fire(ctx context.Context, event, subject string, payload any) string {
	if r == nil {
		return ""
	}
	p := r.hooks.Load()
	if p == nil {
		return ""
	}
	var due []hook
	for _, h := range *p {
		if h.event == event && h.match.MatchString(subject) {
			due = append(due, h)
		}
	}
	if len(due) == 0 {
		return ""
	}
	input, err := json.Marshal(payload)
	if err != nil {
		input = []byte(`{"event":"` + event + `"}`)
	}
	if event != Veto {
		bg := context.WithoutCancel(ctx) // a hook outlives the turn that fired it, up to its timeout
		for _, h := range due {
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				if _, err := r.run(bg, h, input); err != nil {
					r.log().Warn("hook failed", "event", event, "command", h.command, "err", err)
				}
			}()
		}
		return ""
	}
	for _, h := range due {
		out, err := r.run(ctx, h, input)
		if err == nil {
			continue
		}
		reason := strings.TrimSpace(out)
		if reason == "" {
			reason = err.Error()
		}
		r.log().Info("hook vetoed a tool call", "tool", subject, "command", h.command, "reason", reason)
		return reason
	}
	return ""
}

// Wait blocks until background hooks finish (they are bounded by their
// timeouts).
func (r *Runner) Wait() {
	if r != nil {
		r.wg.Wait()
	}
}

func (r *Runner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

// run executes one hook and returns its stdout. A non-zero exit, a timeout
// or a failure to start is an error.
func (r *Runner) run(ctx context.Context, h hook, input []byte) (string, error) {
	tctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	cmd := platform.ShellCommandContext(tctx, h.command)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Env = append(os.Environ(), "HABAKKUK_EVENT="+h.event)
	if r.Home != "" {
		cmd.Dir = r.Home
		cmd.Env = append(cmd.Env, "HABAKKUK_HOME="+r.Home)
	}
	platform.KillGroupOnCancel(cmd)
	cmd.WaitDelay = time.Second
	stdout, stderr := &capped{}, &capped{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	if err != nil && errors.Is(tctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("hook timed out after %s", h.timeout)
	} else if err != nil && stderr.Len() > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), err
}

// capped keeps the first maxOutput bytes and never fails a write.
type capped struct{ bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := maxOutput - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
