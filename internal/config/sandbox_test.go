package config

import (
	"strings"
	"testing"
)

func agentConfig(extra string) string {
	return `version: 1
providers:
  anthropic:
    type: anthropic
    keys_env: [K]
agents:
  habakkuk:
    model: anthropic/m
` + extra
}

func TestSandboxDefaults(t *testing.T) {
	cfg, err := Parse([]byte(agentConfig("")))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Agents["habakkuk"]
	if a.Sandbox != "none" || a.Docker != nil || a.SSH != nil || !*a.Checkpoints || a.CheckpointMaxMB != 200 {
		t.Fatalf("defaults: %+v", a)
	}
	cfg, err = Parse([]byte(agentConfig("    sandbox: docker\n")))
	if err != nil {
		t.Fatal(err)
	}
	if d := cfg.Agents["habakkuk"].Docker; d == nil || d.Image != "debian:stable-slim" || !*d.Network {
		t.Fatalf("docker defaults: %+v", d)
	}
}

func TestSandboxSettings(t *testing.T) {
	cfg, err := Parse([]byte(agentConfig(`    sandbox: docker
    docker:
      image: golang:1.27
      network: false
      cpus: "1.5"
      memory: 2g
      user: image
    checkpoints: false
    checkpoint_max_mb: 50
`)))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Agents["habakkuk"]
	if d := a.Docker; d.Image != "golang:1.27" || *d.Network || d.CPUs != "1.5" || d.Memory != "2g" || d.User != "image" {
		t.Fatalf("docker: %+v", d)
	}
	if *a.Checkpoints || a.CheckpointMaxMB != 50 {
		t.Fatalf("checkpoints: %v %d", *a.Checkpoints, a.CheckpointMaxMB)
	}
	cfg, err = Parse([]byte(agentConfig(`    sandbox: ssh
    ssh:
      host: build.example
      port: 2222
      user: ci
      key_env: BUILD_KEY
      workdir: /srv/habakkuk
`)))
	if err != nil {
		t.Fatal(err)
	}
	if s := cfg.Agents["habakkuk"].SSH; s.Host != "build.example" || s.Port != 2222 || s.KeyEnv != "BUILD_KEY" || s.Workdir != "/srv/habakkuk" {
		t.Fatalf("ssh: %+v", s)
	}
}

func TestSandboxValidation(t *testing.T) {
	cases := map[string]string{
		"    sandbox: vm\n":  `sandbox "vm" must be none, docker or ssh`,
		"    sandbox: ssh\n": "needs an ssh section",
		"    sandbox: ssh\n    ssh:\n      workdir: /w\n":               "ssh host is required",
		"    sandbox: ssh\n    ssh:\n      host: h\n      workdir: w\n": "workdir must be an absolute path",
		"    docker:\n      image: x\n":                                 "docker settings need sandbox: docker",
		"    ssh:\n      host: h\n      workdir: /w\n":                  "ssh settings need sandbox: ssh",
		"    sandbox: docker\n    docker:\n      memory: lots\n":        "not a size",
		"    sandbox: docker\n    docker:\n      cpus: two\n":           "not a number",
		"    checkpoint_max_mb: -1\n":                                   "must not be negative",
		"    sandbox: docker\n    docker:\n      imag: x\n":             "imag",
	}
	for extra, want := range cases {
		_, err := Parse([]byte(agentConfig(extra)))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", extra, err, want)
		}
	}
}
