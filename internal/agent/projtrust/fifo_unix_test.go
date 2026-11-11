//go:build !windows

package projtrust

import "syscall"

// mkfifo makes a named pipe at path.
func mkfifo(path string) error { return syscall.Mkfifo(path, 0o644) }
