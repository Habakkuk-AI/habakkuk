package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/hub"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/skills"
)

// A skill held for approval is listed even when the agent has no skill
// that loads.
func TestSkillsListShowsHeld(t *testing.T) {
	home := t.TempDir()
	lib := &skills.Library{Home: home}
	dir := filepath.Join(lib.AgentDir("habakkuk"), "deploy-check")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, skills.FileName), []byte("---\nname: deploy-check\ndescription: x\n---\n\ny\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock := &hub.Lock{Version: 1, Packages: map[string]*hub.Entry{"deploy-check": {Source: "project ~/code/site", Held: true}}}
	if err := lock.Write(filepath.Join(lib.AgentDir("habakkuk"), hub.LockName)); err != nil {
		t.Fatal(err)
	}
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	err := listSkills(&config.Config{Agents: map[string]*config.Agent{"habakkuk": {}}}, lib, nil)
	w.Close()
	os.Stdout = stdout
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "deploy-check") || !strings.Contains(string(out), "hk skills approve -agent habakkuk deploy-check") {
		t.Errorf("output:\n%s", out)
	}
}
