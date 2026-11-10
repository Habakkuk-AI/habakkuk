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
	return fmt.Sprintf("add %s to %s, or run habakkuk setup model", env, filepath.Join(home, ".env"))
}

// doctorAllGood closes a clean doctor run.
func doctorAllGood(w io.Writer, home string, cfg *config.Config) {
	running := strings.HasPrefix(serveState(home, time.Now()), "running")
	needsServe := len(cfg.Channels) > 0 || cfg.HTTP != nil
	switch {
	case running:
		fmt.Fprintln(w, "\nAll good, and habakkuk serve is running.")
	case needsServe:
		fmt.Fprintln(w, "\nAll good. Next: habakkuk serve, so your channels, schedules and dashboard come alive.")
	default:
		fmt.Fprintln(w, "\nAll good. Next: habakkuk chat.")
	}
}
