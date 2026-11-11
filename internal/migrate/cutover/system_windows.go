//go:build windows

package cutover

import (
	"context"
	"strconv"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
)

func (Host) Alive(pid int) bool { return platform.Alive(pid) }

// Stop ends a process with taskkill. Without force taskkill asks the
// program to close; console programs often ignore that, so the caller
// forces it after a wait.
func (h Host) Stop(pid int, force bool) error {
	if pid <= 4 {
		return errBadPID
	}
	args := []string{"/PID", strconv.Itoa(pid), "/T"}
	if force {
		args = append(args, "/F")
	}
	_, err := h.Run(context.Background(), "taskkill", args...)
	return err
}
