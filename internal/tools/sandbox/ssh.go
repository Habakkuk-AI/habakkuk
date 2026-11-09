package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// SSH runs an agent's commands on a remote host through the ssh CLI. One
// master connection per host is shared by every command (ControlMaster), so
// only the first command pays for the handshake. The local workspace maps
// to Workdir there: a cwd inside the workspace becomes the same relative
// path under Workdir.
//
// The remote files are not the local workspace, so the registry refuses the
// local file-writing tools for such an agent (see tools.LocalFileTool), and
// its turns take no checkpoints.
type SSH struct {
	Host       string
	Port       int
	User       string
	KeyFile    string // private key; "" uses the ssh agent and ssh config
	Workdir    string // remote directory the workspace maps to
	Workspace  string // the local workspace
	ControlDir string // where master connection sockets live
	Bin        string // the ssh CLI; default "ssh"
}

func (s *SSH) Remote() bool { return true }
func (s *SSH) Describe() string {
	if s.User != "" {
		return "ssh host " + s.User + "@" + s.Host
	}
	return "ssh host " + s.Host
}

func (s *SSH) bin() string {
	if s.Bin != "" {
		return s.Bin
	}
	return "ssh"
}

// BaseArgs are the connection options and destination every command uses.
// noStdin (-n) gives the remote command an empty input.
func (s *SSH) BaseArgs(noStdin bool) []string {
	args := []string{"-T", "-o", "BatchMode=yes"}
	if !platform.IsWindows { // Windows OpenSSH cannot share a master connection
		args = append(args, "-o", "ControlMaster=auto",
			"-o", "ControlPath="+filepath.Join(s.ControlDir, "%C"), "-o", "ControlPersist=10m")
	}
	args = append(args, "-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=3")
	if noStdin {
		args = append(args, "-n")
	}
	if s.Port != 0 {
		args = append(args, "-p", fmt.Sprint(s.Port))
	}
	if s.KeyFile != "" {
		args = append(args, "-i", s.KeyFile, "-o", "IdentitiesOnly=yes")
	}
	if s.User != "" {
		args = append(args, "-l", s.User)
	}
	return append(args, s.Host)
}

// RemoteDir maps a local directory to the remote host: inside the workspace
// it moves under Workdir; anything else is taken as a remote path as is.
func (s *SSH) RemoteDir(dir string) string {
	rel, err := filepath.Rel(s.Workspace, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return dir
	}
	return path.Join(s.Workdir, filepath.ToSlash(rel))
}

// CommandArgs are the ssh arguments that run script in dir remotely.
func (s *SSH) CommandArgs(script, dir, pidfile string, stdin bool) []string {
	remote := "cd " + shq(s.RemoteDir(dir)) + " && " + wrapper(pidfile, script)
	return append(s.BaseArgs(!stdin), remote)
}

// KillArgs are the ssh arguments that kill the command whose pid file is given.
func (s *SSH) KillArgs(pidfile string) []string {
	return append(s.BaseArgs(true), killScript(pidfile))
}

func (s *SSH) Command(_ context.Context, script, dir string, stdin bool) (*exec.Cmd, func(), error) {
	pf := pidFile()
	cmd := exec.Command(s.bin(), s.CommandArgs(script, dir, pf, stdin)...)
	return cmd, remoteKiller(tools.GroupKiller(cmd), s.bin(), s.KillArgs(pf)), nil
}

// Close ends the shared master connection, if one is up.
func (s *SSH) Close() {
	args := append([]string{"-O", "exit"}, s.BaseArgs(true)...)
	_ = exec.Command(s.bin(), args...).Run()
}
