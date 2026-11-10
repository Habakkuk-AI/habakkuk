package main

import (
	"context"
	"flag"
	"os"

	"golang.org/x/term"

	"github.com/habakkuk-ai/habakkuk/internal/tui"
)

// chatCmd is `hk chat`: the terminal UI when stdin and stdout are a
// terminal, the line-based chat otherwise or with -plain.
func chatCmd(ctx context.Context, home string, args []string) error {
	plain := false
	rest := make([]string, 0, len(args))
	for _, a := range args {
		switch a {
		case "-plain", "--plain", "-plain=true", "--plain=true":
			plain = true
		case "-plain=false", "--plain=false":
		default:
			rest = append(rest, a)
		}
	}
	if plain || !term.IsTerminal(int(os.Stdout.Fd())) || !term.IsTerminal(int(os.Stdin.Fd())) {
		return chat(ctx, home, rest)
	}
	return tuiCmd(ctx, home, rest)
}

// tuiCmd is `hk tui`: the full-screen terminal UI.
func tuiCmd(ctx context.Context, home string, args []string) error {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	agentName := fs.String("agent", "", "agent to talk to")
	sessionID := fs.String("session", "", "resume this session")
	resume := fs.Bool("resume", false, "pick a recent session to resume")
	fs.Parse(args)

	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	name, err := defaultAgent(a.Cfg, *agentName)
	if err != nil {
		return err
	}
	be := tui.NewAppBackend(a)
	opts := tui.Options{Session: *sessionID, Agent: name, Resume: *resume && *sessionID == "", Version: version}
	return tui.Run(ctx, be, opts, func() func() { return be.Start(ctx) }, os.Stderr)
}
