package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
)

// What plain `habakkuk` (or `hk`) prints. A new user gets one screen that
// says what Habakkuk is and the one command to run next; someone who has
// set it up gets the handful of commands they will use every day. The full
// list stays behind `habakkuk help`.

// cliName is the name this binary was run as, for the commands it
// suggests: "hk" when started through the short name, else "habakkuk".
func cliName() string {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe")
	if base == "hk" {
		return "hk"
	}
	return "habakkuk"
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
