package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// Doctor's fixes: every problem line ends with what to do about it, and a
// clean run ends with the next useful step instead of silence.

// keyFix says how to give a provider the key it lacks.
func keyFix(home string, keysEnv []string) string {
	env := "the key"
	if len(keysEnv) > 0 {
		env = keysEnv[0] + "=..."
	}
	return fmt.Sprintf("add %s to %s, or run hk setup model", env, filepath.Join(home, ".env"))
}

// doctorAllGood closes a clean doctor run with the next step. It never
// says "Next: hk serve" while a serve process is alive: one that is
// running, still starting (connecting channels), or alive but quiet.
func doctorAllGood(w io.Writer, home string, cfg *config.Config) {
	state := serveState(home, time.Now())
	needsServe := len(cfg.Channels) > 0 || cfg.HTTP != nil
	switch {
	case strings.HasPrefix(state, "running"):
		fmt.Fprintln(w, "\nAll good, and hk serve is running.")
	case strings.HasPrefix(state, "starting"):
		fmt.Fprintln(w, "\nAll good, and hk serve is running: it is still starting up. hk status shows what it waits for.")
	case strings.HasPrefix(state, "stuck?"):
		fmt.Fprintln(w, "\nThe checks pass and hk serve is running, but it has not reported for a while. hk status says more; restart it if that does not clear.")
	case needsServe:
		fmt.Fprintln(w, "\nAll good. Next: hk serve, so your channels, schedules and dashboard come alive.")
	default:
		fmt.Fprintln(w, "\nAll good. Next: hk chat.")
	}
}
