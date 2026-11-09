//go:build !windows

package platform

import "os/exec"

// setCmdLine is only needed on Windows, where cmd.exe parses its own
// command line.
func setCmdLine(*exec.Cmd, string) {}
