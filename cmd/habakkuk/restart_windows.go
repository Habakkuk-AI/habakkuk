//go:build windows

package main

import (
	"os"
	"os/exec"
)

// restartSelf starts the same binary and arguments again and lets this
// process end. Windows has no exec, so the new process gets a new id.
func restartSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}
