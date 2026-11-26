//go:build !windows

package main

import (
	"os"
	"syscall"
)

// restartSelf replaces this process with a fresh run of the same binary and
// arguments. The process id stays the same, so a service manager (systemd,
// launchd) sees the service keep running.
func restartSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}
