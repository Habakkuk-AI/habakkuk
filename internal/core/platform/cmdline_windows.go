//go:build windows

package platform

import (
	"os/exec"
	"syscall"
)

// setCmdLine passes line to the program verbatim: cmd.exe does not follow
// the quoting rules Go uses to join arguments.
func setCmdLine(cmd *exec.Cmd, line string) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CmdLine = line
}
