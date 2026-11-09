//go:build windows

package supervisor

import (
	"errors"
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

// No console window flashes up for the child.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createNoWindow | syscall.CREATE_NEW_PROCESS_GROUP}
}

// Windows has no SIGTERM for a process without a console; Stop kills at once.
func interrupt(*exec.Cmd) error { return errors.New("not supported") }

func kill(cmd *exec.Cmd) { _ = cmd.Process.Kill() }
