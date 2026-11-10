package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
)

// What plain `hk` (or `habakkuk`) prints. A new user gets one screen that
// says what Habakkuk is and the one command to run next; someone who has
// set it up gets the handful of commands they will use every day. The full
// list stays behind `hk help`.

// cliName is the name to use in the commands this binary suggests. It is
// "hk", the short name, whenever hk runs this program: when it was started
// as hk, or when an hk that points here sits beside the binary. Only an
// install without the short name (go install, a bare archive) gets
// "habakkuk".
func cliName() string {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe")
	if base == "hk" {
		return "hk"
	}
	if exe, err := os.Executable(); err == nil && hasShortName(exe) {
		return "hk"
	}
	return "habakkuk"
}

// hasShortName reports whether the folder holding exe also holds the short
// name for it: a link to the same file, or on Windows the hk.cmd shim the
// installer writes.
func hasShortName(exe string) bool {
	dir := filepath.Dir(exe)
	if runtime.GOOS == "windows" {
		_, err := os.Stat(filepath.Join(dir, "hk.cmd"))
		return err == nil
	}
	self, err := os.Stat(exe)
	if err != nil {
		return false
	}
	short, err := os.Stat(filepath.Join(dir, "hk"))
	return err == nil && os.SameFile(self, short)
}

// hasConfig reports whether home holds a config.yaml.
func hasConfig(home string) bool {
	_, err := os.Stat(filepath.Join(home, "config.yaml"))
	return !errors.Is(err, os.ErrNotExist)
}

// noArgs prints the screen for a bare `habakkuk`.
func noArgs(w io.Writer, home, name string, now time.Time) {
	if !hasConfig(home) {
		firstRun(w, name)
		return
	}
	agentName := "your agent"
	if id, ok := agent.ReadIdentity(filepath.Join(home, "agents", "habakkuk")); ok && id.Name != "" {
		agentName = id.Name
	}
	fmt.Fprintf(w, "Habakkuk is set up in %s.\n", home)
	fmt.Fprintf(w, "serve: %s\n\n", serveState(home, now))
	rows := [][2]string{
		{"chat", "talk to " + agentName + " in this terminal"},
		{"serve", "keep it running for your channels, schedules and the dashboard"},
		{"status", "what is running, today's cost, recent errors"},
		{"doctor", "check config, keys and storage, with a fix for each problem"},
		{"setup", "change the model, add a channel, rename your agent"},
	}
	for _, r := range rows {
		fmt.Fprintf(w, "  %-*s  %s\n", len(name)+8, name+" "+r[0], r[1])
	}
	fmt.Fprintf(w, "\nEvery command: %s help\n", name)
}

// firstRun is the welcome for someone who has not run setup yet.
func firstRun(w io.Writer, name string) {
	fmt.Fprintf(w, `Welcome to Habakkuk, your personal AI agent.

It runs on your computer, remembers what you tell it, and gets real work
done: reminders, research, drafts, files and recurring check-ins. Talk to
it in WhatsApp, Telegram, Slack, your browser or right here.

Start here (about a minute):

  %s setup

Setup finds the API keys and local models on this computer, connects the
app you want to talk in, and checks that your agent answers.

Coming from another agent? %s migrate brings its memory, skills and keys.
Every command: %s help
`, name, name, name)
}
