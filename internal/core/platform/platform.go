// Package platform holds the few operating-system specific pieces Habakkuk
// needs: process groups, liveness checks, file locks, free disk space, the
// shell that runs commands, the data directory and opening a URL. Everything
// else in the tree is portable Go and calls these instead of syscall.
//
// Unix and Windows each get a real implementation; nothing here silently
// does nothing. Where Windows has no equivalent (Unix permission bits, file
// owners) the function says so, so callers can skip a check instead of
// reporting nonsense.
package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

// IsWindows reports whether this binary runs on Windows.
const IsWindows = runtime.GOOS == "windows"

// HasUnixPermissions reports whether file mode bits mean anything here. On
// Windows access is governed by ACLs, and Go reports every file as 0666 or
// 0444, so permission audits must be skipped rather than trusted.
const HasUnixPermissions = !IsWindows

// ExeSuffix is ".exe" on Windows and "" elsewhere.
const ExeSuffix = exeSuffix

// DataDir is Habakkuk's home: $HABAKKUK_HOME when set, else ~/.habakkuk on
// Unix and %APPDATA%\Habakkuk on Windows.
func DataDir() string {
	if h := os.Getenv("HABAKKUK_HOME"); h != "" {
		return h
	}
	return defaultDataDir()
}

func defaultDataDirUnix() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".habakkuk"
	}
	return filepath.Join(home, ".habakkuk")
}

// GroupKiller puts cmd in its own process group and returns a function that
// kills that group (on Windows, the process tree) once cmd has started. The
// function is safe to call more than once or before Start.
func GroupKiller(cmd *exec.Cmd) func() {
	SetProcessGroup(cmd)
	var once sync.Once
	return func() {
		if cmd.Process == nil {
			return
		}
		once.Do(func() { _ = KillGroup(cmd.Process.Pid) })
	}
}

// KillGroupOnCancel puts cmd (made with exec.CommandContext) in its own
// process group and makes cancelling its context kill the whole group, not
// only the direct child.
func KillGroupOnCancel(cmd *exec.Cmd) {
	SetProcessGroup(cmd)
	cmd.Cancel = func() error { return KillGroup(cmd.Process.Pid) }
}

// OpenURL opens u in the user's default browser (or the program registered
// for a file path) without waiting for it.
func OpenURL(u string) error {
	cmd := openCommand(runtime.GOOS, u)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func openCommand(goos, u string) *exec.Cmd {
	switch goos {
	case "darwin":
		return exec.Command("open", u)
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	}
	return exec.Command("xdg-open", u)
}
