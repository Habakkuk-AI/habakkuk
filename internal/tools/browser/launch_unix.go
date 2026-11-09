//go:build !windows

package browser

import (
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

// controlFlag drives the browser over a pipe, so no port is open.
const controlFlag = "--remote-debugging-pipe"

// launch starts the browser with a pipe on fds 3 (commands) and 4 (replies).
func launch(bin, profile string, headless bool, extra []string) (*process, transport, error) {
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, nil, err
	}
	pidFile := filepath.Join(profile, "habakkuk.pid")
	killStale(profile, pidFile)

	cmdR, cmdW, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	respR, respW, err := os.Pipe()
	if err != nil {
		cmdR.Close()
		cmdW.Close()
		return nil, nil, err
	}
	cmd := exec.Command(bin, launchArgs(profile, headless, extra)...)
	cmd.ExtraFiles = []*os.File{cmdR, respW}
	// Its own process group, so closing it takes the helpers it started too.
	platform.SetProcessGroup(cmd)
	st := &tail{max: 8 << 10}
	cmd.Stderr = st
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{cmdR, cmdW, respR, respW} {
			f.Close()
		}
		return nil, nil, fmt.Errorf("starting %s: %w", bin, err)
	}
	cmdR.Close()
	respW.Close()
	p := &process{cmd: cmd, exited: make(chan struct{}), pidFile: pidFile, stderr: st}
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	t := newPipeTransport(cmdW, respR)
	go func() {
		_ = cmd.Wait()
		close(p.exited)
		_ = t.close() // unblocks the read loop if the browser died
	}()
	return p, t, nil
}

// stop ends the browser: it had a moment to close after Browser.close, then
// its whole process group is killed.
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
	// Helpers that outlived the browser.
	_ = platform.KillGroup(p.cmd.Process.Pid)
	os.Remove(p.pidFile)
}

// killStale stops a browser a crashed Habakkuk left running on this profile,
// which would otherwise hold the profile lock. It only kills the recorded pid
// when the profile's lock names it too, so a reused pid is left alone.
func killStale(profile, pidFile string) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	defer os.Remove(pidFile)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return
	}
	lock, err := os.Readlink(filepath.Join(profile, "SingletonLock"))
	if err != nil || !strings.HasSuffix(lock, "-"+strconv.Itoa(pid)) {
		return
	}
	if !platform.Alive(pid) {
		return
	}
	_ = platform.KillGroup(pid)
	_ = platform.Kill(pid)
	for range 40 {
		if !platform.Alive(pid) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}
