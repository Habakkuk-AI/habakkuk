package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// DefaultImage is used when an agent's docker sandbox names no image. It has
// bash and setsid, which the command wrapper uses.
const DefaultImage = "debian:stable-slim"

// Docker runs an agent's commands in one long-lived container per agent,
// started on first use with the workspace bind-mounted at the same path, so
// file tools and commands see the same files at the same paths. The
// container is labelled with a hash of its settings and recreated when they
// change; otherwise it outlives Habakkuk and is reused.
type Docker struct {
	Agent     string
	Workspace string
	Image     string
	Network   bool   // false runs with --network none
	CPUs      string // --cpus, e.g. "2"
	Memory    string // --memory, e.g. "2g"
	User      string // --user, e.g. "501:20"; "" keeps the image's user
	Bin       string // the docker CLI; default "docker"

	mu      sync.Mutex
	checked time.Time // when the container was last seen running
}

func (d *Docker) Remote() bool     { return false }
func (d *Docker) Describe() string { return "docker container " + d.Container() }

func (d *Docker) bin() string {
	if d.Bin != "" {
		return d.Bin
	}
	return "docker"
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

// Container is the container's name.
func (d *Docker) Container() string {
	return "habakkuk-" + unsafeName.ReplaceAllString(d.Agent, "_")
}

func (d *Docker) image() string {
	if d.Image != "" {
		return d.Image
	}
	return DefaultImage
}

// ConfigHash identifies the settings the container was created with.
func (d *Docker) ConfigHash() string {
	h := sha256.Sum256([]byte(strings.Join([]string{d.image(), fmt.Sprint(d.Network), d.CPUs, d.Memory, d.User, d.Workspace}, "\x00")))
	return hex.EncodeToString(h[:6])
}

// RunArgs are the arguments that create the container.
func (d *Docker) RunArgs() []string {
	args := []string{"run", "-d", "--name", d.Container(), "--init",
		"--label", "habakkuk.agent=" + d.Agent, "--label", "habakkuk.config=" + d.ConfigHash(),
		"-v", d.Workspace + ":" + d.containerPath(d.Workspace), "-w", d.containerPath(d.Workspace), "-e", "HOME=/tmp"}
	if !d.Network {
		args = append(args, "--network", "none")
	}
	if d.CPUs != "" {
		args = append(args, "--cpus", d.CPUs)
	}
	if d.Memory != "" {
		args = append(args, "--memory", d.Memory)
	}
	if d.User != "" {
		args = append(args, "--user", d.User)
	}
	return append(args, d.image(), "sleep", "infinity")
}

// ExecArgs are the arguments that run script in dir inside the container.
func (d *Docker) ExecArgs(script, dir, pidfile string, stdin bool) []string {
	args := []string{"exec"}
	if stdin {
		args = append(args, "-i")
	}
	return append(args, "-w", d.containerPath(dir), d.Container(), "sh", "-c", wrapper(pidfile, script))
}

// containerWorkspace is where a Windows workspace is mounted: Linux
// containers cannot use a C:\ path, so the same-path mount only works on
// Unix hosts.
const containerWorkspace = "/workspace"

// containerPath maps a host path to the container: unchanged on Unix, and
// under /workspace on Windows for paths inside the workspace.
func (d *Docker) containerPath(p string) string { return d.containerPathFor(runtime.GOOS, p) }

func (d *Docker) containerPathFor(goos, p string) string {
	if goos != "windows" {
		return p
	}
	rel, err := filepath.Rel(d.Workspace, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return path.Join(containerWorkspace, filepath.ToSlash(rel))
}

// KillArgs are the arguments that kill the command whose pid file is given.
func (d *Docker) KillArgs(pidfile string) []string {
	return []string{"exec", d.Container(), "sh", "-c", killScript(pidfile)}
}

func (d *Docker) Command(ctx context.Context, script, dir string, stdin bool) (*exec.Cmd, func(), error) {
	if err := d.ensure(ctx); err != nil {
		return nil, nil, err
	}
	pf := pidFile()
	cmd := exec.Command(d.bin(), d.ExecArgs(script, dir, pf, stdin)...)
	return cmd, remoteKiller(tools.GroupKiller(cmd), d.bin(), d.KillArgs(pf)), nil
}

// ensure makes sure the container exists with the current settings and is
// running. A container seen running in the last 30 seconds is trusted.
func (d *Docker) ensure(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if time.Since(d.checked) < 30*time.Second {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute) // a first run may pull the image
	defer cancel()
	name := d.Container()
	out, err := d.docker(ctx, "inspect", "--format", `{{.State.Running}} {{index .Config.Labels "habakkuk.config"}}`, name)
	running, label, _ := strings.Cut(strings.TrimSpace(out), " ")
	switch {
	case err != nil: // no such container
		if err := d.create(ctx); err != nil {
			return err
		}
	case label != d.ConfigHash():
		if _, err := d.docker(ctx, "rm", "-f", name); err != nil {
			return fmt.Errorf("replace container %s: %w", name, err)
		}
		if err := d.create(ctx); err != nil {
			return err
		}
	case running != "true":
		if _, err := d.docker(ctx, "start", name); err != nil {
			return fmt.Errorf("start container %s: %w", name, err)
		}
	}
	d.checked = time.Now()
	return nil
}

// create runs the container. Another Habakkuk process may have created it a
// moment earlier; a running container with our settings is as good.
func (d *Docker) create(ctx context.Context) error {
	_, err := d.docker(ctx, d.RunArgs()...)
	if err == nil {
		return nil
	}
	out, ierr := d.docker(ctx, "inspect", "--format", `{{.State.Running}} {{index .Config.Labels "habakkuk.config"}}`, d.Container())
	if ierr == nil && strings.TrimSpace(out) == "true "+d.ConfigHash() {
		return nil
	}
	return fmt.Errorf("start container %s: %w", d.Container(), err)
}

// Remove deletes the container; the next command creates it again.
func (d *Docker) Remove(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.checked = time.Time{}
	_, err := d.docker(ctx, "rm", "-f", d.Container())
	return err
}

func (d *Docker) docker(ctx context.Context, args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, d.bin(), args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return out.String(), fmt.Errorf("%w: %s", err, msg)
		}
		return out.String(), err
	}
	return out.String(), nil
}
