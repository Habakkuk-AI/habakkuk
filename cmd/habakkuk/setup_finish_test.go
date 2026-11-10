package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/testutil"
)

// TestSetupSummaryNonInteractive: a scripted setup ends with what is set
// up and what to try, writes the starter defaults, and never opens a chat
// (which would also use up the first-chat welcome).
func TestSetupSummaryNonInteractive(t *testing.T) {
	home := t.TempDir()
	model := newFakeModel(t)
	r := &setupRun{env: map[string]string{"OPENAI_API_KEY": "sk-test"}}
	out, err := r.run(t, home, "-y", "-provider", "openai", "-base-url", model.srv.URL+"/v1", "-model", "gpt-test", "-name", "Nova")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if r.chatted {
		t.Fatal("setup -y opened a chat")
	}
	if !agent.NeedsOnboarding(filepath.Join(home, "agents", "habakkuk")) {
		t.Fatal("setup used up the first-chat welcome")
	}
	cfg := mustLoad(t, home)
	if cfg.HTTP == nil || !strings.HasPrefix(cfg.HTTP.Listen, "127.0.0.1:") {
		t.Fatalf("no dashboard on loopback: %+v", cfg.HTTP)
	}
	if cfg.Memory == nil || !cfg.Memory.Review {
		t.Fatalf("memory review is off: %+v", cfg.Memory)
	}
	for _, want := range []string{
		"Nova is ready. Setup took ",
		"  Model      openai/gpt-test\n",
		"  Talk       this terminal, with hk chat, or Chat in the dashboard\n",
		"  Memory     on: Nova keeps what matters",
		"  Dashboard  http://" + cfg.HTTP.Listen + "/app/",
		"Try these first:\n  1. Run hk chat and say hi.\n",
		`2. Get a morning brief: "Every weekday at 8, send me a short brief of my day."`,
		"run while hk serve does",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\u2014") {
		t.Error("em dash in user-facing text")
	}

	// A re-run never adds the starter defaults back once removed.
	path := filepath.Join(home, "config.yaml")
	raw, _ := os.ReadFile(path)
	trimmed := string(raw[:strings.Index(string(raw), "\n# The dashboard")]) + "\n"
	os.WriteFile(path, []byte(trimmed), 0o600)
	if out, err := r.run(t, home, "-y", "-name", "Nova"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if cfg := mustLoad(t, home); cfg.HTTP != nil || cfg.Memory != nil {
		t.Fatalf("a re-run added defaults back: http %+v memory %+v", cfg.HTTP, cfg.Memory)
	}
}

// TestSetupInstallsBackgroundService: answering yes to keeping the agent
// running installs the launchd agent, starts it and waits for serve's
// heartbeat, so setup can say it is live.
func TestSetupInstallsBackgroundService(t *testing.T) {
	testutil.UnixOnly(t, "launchd plists hold Unix paths")
	home := t.TempDir()
	model, wa := newFakeModel(t), newFakeWA(t)
	wa.wait(waHello)
	model.set(0, true)
	r := &setupRun{env: map[string]string{"PATH": "/usr/bin:/bin"}, goos: "darwin", userHome: t.TempDir(),
		ep:    endpoints{whatsappAgent: wa.srv.URL, local: map[string]string{"ollama": model.srv.URL + "/v1"}},
		stdin: strings.Join([]string{"1", "1", "wa-key", "", "", ""}, "\n") + "\n"}
	r.onCmd = func() { // the service starting: serve writes its heartbeat
		data, _ := json.Marshal(heartbeat{PID: os.Getpid(), Version: version, Started: time.Now(), Beat: time.Now()})
		os.WriteFile(filepath.Join(home, heartbeatFile), data, 0o600)
	}
	out, err := r.run(t, home)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	plist := filepath.Join(r.userHome, "Library", "LaunchAgents", launchdLabel+".plist")
	if data, err := os.ReadFile(plist); err != nil || !strings.Contains(string(data), "HABAKKUK_HOME") {
		t.Fatalf("plist not installed: %v", err)
	}
	if len(r.cmds) != 2 || r.cmds[1][0] != "launchctl" || r.cmds[1][1] != "bootstrap" || r.cmds[1][3] != plist {
		t.Fatalf("commands = %v", r.cmds)
	}
	if !strings.Contains(out, "Habakkuk is running in the background. Now your agent answers the message you sent it in WhatsApp.") {
		t.Fatalf("no live line:\n%s", out)
	}
	if r.served {
		t.Fatal("also started serve in the foreground")
	}
}

func TestFreeListenSkipsBusyPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().String()
	got := freeListen(busy, 4)
	if got == busy || !strings.HasPrefix(got, "127.0.0.1:") {
		t.Fatalf("freeListen(%s) = %s", busy, got)
	}
}
