//go:build !windows

package migrate

import "syscall"

// mkfifo makes a named pipe: a file a copy must leave out.
func mkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }
