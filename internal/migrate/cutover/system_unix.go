//go:build unix

package cutover

import (
	"syscall"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
)

func (Host) Alive(pid int) bool { return platform.Alive(pid) }

func (Host) Stop(pid int, force bool) error {
	if pid <= 1 {
		return errBadPID
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(pid, sig)
}
