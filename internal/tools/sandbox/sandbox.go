// Package sandbox runs an agent's commands somewhere other than this
// machine's shell: in a long-lived Docker container, or on a remote host over
// SSH. Both drive the platform's own CLI (docker, ssh), so Habakkuk needs no
// client libraries, and both implement tools.Backend.
//
// Each command runs under a small sh wrapper that records the process
// group's id in a pid file where the command runs, so a timeout or
// process_kill ends the whole tree there, not just the local client.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// shq quotes s for a POSIX shell.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// wrapper runs script with bash as the leader of a new session (setsid,
// where the system has it), writing its pid to pidfile first and removing
// the file when it ends. The exit status is the script's.
func wrapper(pidfile, script string) string {
	inner := `echo $$ > "$0"; bash -c "$1"; s=$?; rm -f "$0"; exit $s`
	args := shq(inner) + " " + shq(pidfile) + " " + shq(script)
	return "if setsid -w true 2>/dev/null; then exec setsid -w bash -c " + args +
		"; else exec bash -c " + args + "; fi"
}

// killScript kills the process group recorded in pidfile (or the process,
// if it never became a group leader) and removes the file. It is a command
// line for any shell: it runs bash, whose kill builtin takes a negative
// process group id (dash's and busybox's do not reliably).
func killScript(pidfile string) string {
	q := shq(pidfile)
	script := `p=$(cat ` + q + ` 2>/dev/null); if [ -n "$p" ]; then kill -KILL -- "-$p" 2>/dev/null || kill -KILL "$p" 2>/dev/null; fi; rm -f ` + q + `; true`
	return "bash -c " + shq(script)
}

func pidFile() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "/tmp/habakkuk-" + hex.EncodeToString(b) + ".pid"
}

// remoteKiller returns a kill function that first runs the kill command
// where the process runs, then kills the local client's process group.
func remoteKiller(local func(), bin string, args []string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = exec.CommandContext(ctx, bin, args...).Run()
			local()
		})
	}
}
