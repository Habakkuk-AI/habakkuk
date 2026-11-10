//go:build !windows

package projtrust

import (
	"os"
	"syscall"
)

// openFile opens path for reading without following a final symlink and
// without blocking on a FIFO, so what is read is checked after opening.
func openFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
