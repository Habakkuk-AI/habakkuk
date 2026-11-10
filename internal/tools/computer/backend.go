package computer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
)

// Backend drives one desktop through command-line tools.
type Backend interface {
	Name() string
	// Start prepares the desktop (a virtual one is started here); it is
	// called before every action and must be cheap once started.
	Start(ctx context.Context) error
	// Screenshot captures the whole screen as PNG.
	Screenshot(ctx context.Context) ([]byte, error)
	// InputSize is the size of the space input coordinates are in, or 0, 0
	// when that is the screenshot's pixels.
	InputSize(ctx context.Context) (int, int, error)
	// Do performs one input action; coordinates are input coordinates.
	Do(ctx context.Context, a action) error
	// Cursor is the pointer's position in input coordinates.
	Cursor(ctx context.Context) (int, int, error)
	// Front names the app in front and its window's title.
	Front(ctx context.Context) (app, title string, err error)
	Close()
}

// action is one input action, in input coordinates.
type action struct {
	kind   string // click, move, drag, down, up, type, key, hold, scroll, open
	at     bool   // x, y given (click and scroll at the pointer otherwise)
	x, y   int
	x2, y2 int // drag end
	button string
	count  int
	mods   []string
	text   string // type: the text; open: the app or URL
	keys   combo
	repeat int
	dx, dy int // scroll, in wheel clicks: positive is right and down
	dur    time.Duration
}

// cmd is one command a backend runs.
type cmd struct {
	name   string
	args   []string
	sleep  time.Duration // pause before the next command
	detach bool          // start it and do not wait (an app being opened)
}

// runFunc runs a command with extra environment and returns its output.
type runFunc func(ctx context.Context, env []string, c cmd) ([]byte, error)

// execRun runs c in its own process group, killed whole on cancel. Errors
// carry the command's name and stderr, never its arguments, which may be
// typed text.
func execRun(ctx context.Context, env []string, c cmd) ([]byte, error) {
	if c.detach {
		x := exec.Command(c.name, c.args...)
		x.Env = append(os.Environ(), env...)
		platform.Detach(x)
		if err := x.Start(); err != nil {
			return nil, fmt.Errorf("%s: %w", c.name, err)
		}
		go func() { _ = x.Wait() }()
		return nil, nil
	}
	x := exec.CommandContext(ctx, c.name, c.args...)
	x.Env = append(os.Environ(), env...)
	platform.KillGroupOnCancel(x)
	x.WaitDelay = 2 * time.Second
	var out, stderr bytes.Buffer
	x.Stdout, x.Stderr = &out, &stderr
	if err := x.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("%s is not installed (hk doctor lists what this backend needs)", c.name)
		}
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		if msg != "" {
			return out.Bytes(), fmt.Errorf("%s: %w: %s", c.name, err, msg)
		}
		return out.Bytes(), fmt.Errorf("%s: %w", c.name, err)
	}
	return out.Bytes(), nil
}

// runAll runs commands in order, pausing where asked. For a key hold (a
// press, then a release) the release runs even when the press or the
// pause fails, so no key stays down.
func runAll(ctx context.Context, run runFunc, env []string, cmds []cmd, hold bool) error {
	if hold && len(cmds) == 2 {
		_, err := run(ctx, env, cmds[0])
		if err == nil {
			err = sleep(ctx, cmds[0].sleep)
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, rerr := run(rctx, env, cmds[1]); err == nil {
			err = rerr
		}
		return err
	}
	for _, c := range cmds {
		if _, err := run(ctx, env, c); err != nil {
			return err
		}
		if c.sleep > 0 {
			if err := sleep(ctx, c.sleep); err != nil {
				return err
			}
		}
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readTemp runs a command that writes a file, then returns the file's
// contents and removes it.
func readTemp(ctx context.Context, run runFunc, env []string, path string, c cmd) ([]byte, error) {
	defer os.Remove(path)
	if _, err := run(ctx, env, c); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s wrote no screenshot: %w", c.name, err)
	}
	return data, nil
}

// tempPNG is a fresh path for a screenshot file.
func tempPNG() (string, func(), error) {
	dir, err := os.MkdirTemp("", "habakkuk-shot-")
	if err != nil {
		return "", nil, err
	}
	return dir + "/screen.png", func() { _ = os.RemoveAll(dir) }, nil
}

// isURL reports whether an open target is a URL rather than an app.
func isURL(s string) bool {
	i := strings.Index(s, "://")
	if i <= 0 {
		return strings.HasPrefix(s, "mailto:")
	}
	return strings.Trim(s[:i], "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-.") == ""
}
