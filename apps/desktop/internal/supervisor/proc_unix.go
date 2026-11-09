//go:build !windows

package supervisor

import (
	"os/exec"
	"syscall"
)

// The child leads its own process group, so stopping it also reaches what it
// started (MCP servers, a browser) if it does not clean them up itself.
func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

func interrupt(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

func kill(cmd *exec.Cmd) {
	if syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) != nil {
		_ = cmd.Process.Kill()
	}
}
