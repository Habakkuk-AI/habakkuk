package config

import (
	"fmt"
	"path"
	"regexp"
)

// DockerSandbox runs an agent's commands in a long-lived container with the
// workspace bind-mounted at the same path.
type DockerSandbox struct {
	Image   string `yaml:"image"`   // default debian:stable-slim; needs bash
	Network *bool  `yaml:"network"` // default true; false runs with no network
	CPUs    string `yaml:"cpus"`    // e.g. "2"
	Memory  string `yaml:"memory"`  // e.g. "2g"
	// User is the uid:gid commands run as. Default: Habakkuk's own, so files
	// written in the workspace belong to you; "image" keeps the image's user.
	User string `yaml:"user"`
}

// SSHSandbox runs an agent's commands on a remote host. The workspace maps
// to Workdir there; local file-writing tools are refused for such an agent.
type SSHSandbox struct {
	Host    string `yaml:"host"`
	Port    int    `yaml:"port"` // default 22 (or the ssh config's)
	User    string `yaml:"user"`
	KeyEnv  string `yaml:"key_env"` // variable holding a private key, or the path to one; default the ssh agent
	Workdir string `yaml:"workdir"` // absolute remote directory
}

func (a *Agent) applySandboxDefaults() {
	if a.Sandbox == "" {
		a.Sandbox = "none"
	}
	if a.Sandbox == "docker" && a.Docker == nil {
		a.Docker = &DockerSandbox{}
	}
	if a.Docker != nil {
		if a.Docker.Image == "" {
			a.Docker.Image = "debian:stable-slim"
		}
		if a.Docker.Network == nil {
			on := true
			a.Docker.Network = &on
		}
	}
	if a.Checkpoints == nil {
		on := true
		a.Checkpoints = &on
	}
	if a.CheckpointMaxMB == 0 {
		a.CheckpointMaxMB = 200
	}
}

var (
	dockerSize = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[bkmgBKMG]?$`)
	dockerCPUs = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
)

func validateSandbox(name string, a *Agent) []error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("agent %s: "+format, append([]any{name}, args...)...))
	}
	switch a.Sandbox {
	case "none", "docker":
	case "ssh":
		if a.SSH == nil {
			bad("sandbox ssh needs an ssh section with host and workdir")
		}
	default:
		bad("sandbox %q must be none, docker or ssh", a.Sandbox)
	}
	if a.Docker != nil && a.Sandbox != "docker" {
		bad("docker settings need sandbox: docker")
	}
	if a.SSH != nil && a.Sandbox != "ssh" {
		bad("ssh settings need sandbox: ssh")
	}
	if d := a.Docker; d != nil {
		if d.Memory != "" && !dockerSize.MatchString(d.Memory) {
			bad("docker memory %q is not a size like 512m or 2g", d.Memory)
		}
		if d.CPUs != "" && !dockerCPUs.MatchString(d.CPUs) {
			bad("docker cpus %q is not a number", d.CPUs)
		}
	}
	if s := a.SSH; s != nil {
		if s.Host == "" {
			bad("ssh host is required")
		}
		if s.Workdir == "" || !path.IsAbs(s.Workdir) {
			bad("ssh workdir must be an absolute path on the remote host")
		}
		if s.Port < 0 || s.Port > 65535 {
			bad("ssh port %d is out of range", s.Port)
		}
	}
	if a.CheckpointMaxMB < 0 {
		bad("checkpoint_max_mb must not be negative")
	}
	return errs
}
