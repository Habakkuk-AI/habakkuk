//go:build !windows

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// inForeground reports whether this process's group is the terminal's
// foreground group. A process the gateway, the TUI or an editor started
// shares the owner's terminal but runs in the background: it must not
// paint a file or a question onto the owner's screen.
func inForeground(tty *os.File) bool {
	pgrp, err := unix.IoctlGetInt(int(tty.Fd()), unix.TIOCGPGRP)
	return err == nil && pgrp == unix.Getpgrp()
}
