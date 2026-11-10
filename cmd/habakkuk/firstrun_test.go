package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
)

// TestNoArgsFirstRun: plain `hk` on a new computer is one screen that says
// what Habakkuk is and the one command to run, not the full reference.
func TestNoArgsFirstRun(t *testing.T) {
	var out bytes.Buffer
	noArgs(&out, t.TempDir(), "hk", time.Now())
	got := out.String()
	for _, want := range []string{"Welcome to Habakkuk", "\n  hk setup\n", "hk migrate", "Every command: hk help"} {
		if !strings.Contains(got, want) {
			t.Errorf("first-run screen lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Usage:") || strings.Count(got, "\n") > 20 {
		t.Errorf("first-run screen is the full reference or too long:\n%s", got)
	}
	if strings.Contains(got, "\u2014") {
		t.Error("em dash in user-facing text")
	}
}

// TestNoArgsConfigured: once set up, plain `hk` lists the everyday
// commands, using the agent's name and the name the binary was run as.
func TestNoArgsConfigured(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte("version: 1\n"), 0o600)
	dir := filepath.Join(home, "agents", "habakkuk")
	if err := agent.WriteIdentity(dir, agent.Identity{Name: "Nova"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	noArgs(&out, home, "habakkuk", time.Now())
	got := out.String()
	for _, want := range []string{"Habakkuk is set up in " + home, "serve: not running", "habakkuk chat", "talk to Nova", "habakkuk serve", "Every command: habakkuk help"} {
		if !strings.Contains(got, want) {
			t.Errorf("screen lacks %q:\n%s", want, got)
		}
	}
}
