//go:build windows

package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
)

// controlFlag: Windows cannot hand the browser extra file descriptors, so
// --remote-debugging-pipe is out. The browser listens on a random loopback
// port instead and names it in <profile>\DevToolsActivePort.
const controlFlag = "--remote-debugging-port=0"

func init() {
	binaryNames = append(binaryNames, "msedge")
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"} {
		base := os.Getenv(env)
		if base == "" {
			continue
		}
		binaryPaths = append(binaryPaths,
			filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(base, "Chromium", "Application", "chrome.exe"),
			filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"))
	}
}

// launch starts the browser listening on a loopback port and connects to it
// over its DevTools WebSocket.
func launch(bin, profile string, headless bool, extra []string) (*process, transport, error) {
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, nil, err
	}
	pidFile := filepath.Join(profile, "habakkuk.pid")
	killStale(profile, pidFile)
	portFile := filepath.Join(profile, "DevToolsActivePort")
	_ = os.Remove(portFile)

	args := append([]string{"--remote-debugging-address=127.0.0.1"}, launchArgs(profile, headless, extra)...)
	cmd := exec.Command(bin, args...)
	platform.SetProcessGroup(cmd)
	st := &tail{max: 8 << 10}
	cmd.Stderr = st
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("starting %s: %w", bin, err)
	}
	p := &process{cmd: cmd, exited: make(chan struct{}), pidFile: pidFile, stderr: st}
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	u, err := waitDevToolsPort(ctx, portFile, p.exited)
	if err == nil {
		var t transport
		if t, err = dialWS(ctx, u); err == nil {
			return p, t, nil
		}
	}
	p.stop(0)
	return nil, nil, fmt.Errorf("starting %s: %w", bin, err)
}

// waitDevToolsPort waits for the browser to write its port and returns the
// browser's WebSocket URL.
func waitDevToolsPort(ctx context.Context, portFile string, exited <-chan struct{}) (string, error) {
	for {
		if data, err := os.ReadFile(portFile); err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) >= 2 {
				port, path := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
				if _, err := strconv.Atoi(port); err == nil && strings.HasPrefix(path, "/") {
					return "ws://127.0.0.1:" + port + path, nil
				}
			}
		}
		select {
		case <-exited:
			return "", errors.New("the browser exited before opening its DevTools port")
		case <-ctx.Done():
			return "", errors.New("timed out waiting for the browser's DevTools port")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// stop ends the browser: it had a moment to close after Browser.close, then
// its process tree is killed.
func (p *process) stop(grace time.Duration) {
	select {
	case <-p.exited:
	case <-time.After(grace):
		_ = platform.KillGroup(p.cmd.Process.Pid)
		select {
		case <-p.exited:
		case <-time.After(5 * time.Second):
		}
	}
	os.Remove(p.pidFile)
}

// killStale stops a browser a crashed Habakkuk left running on this profile.
// Chrome on Windows keeps <profile>\lockfile open while it runs, so the pid
// is only killed when that file is still held (cannot be removed), which
// leaves a reused pid alone.
func killStale(profile, pidFile string) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	defer os.Remove(pidFile)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 4 || !platform.Alive(pid) {
		return
	}
	lock := filepath.Join(profile, "lockfile")
	if _, err := os.Stat(lock); err != nil || os.Remove(lock) == nil {
		return
	}
	_ = platform.KillGroup(pid)
	for range 40 {
		if !platform.Alive(pid) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}
