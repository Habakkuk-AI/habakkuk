//go:build darwin

package cutover

import (
	"context"
	"strconv"
	"strings"
)

// Inspect asks ps for the environment (macOS shows it for the user's own
// processes only) and lsof for the working folder.
func (h Host) Inspect(ctx context.Context, pid int) ProcInfo {
	var info ProcInfo
	p := strconv.Itoa(pid)
	if out, err := h.Run(ctx, "ps", "-E", "-ww", "-o", "command=", "-p", p); err == nil {
		info.Env = envWords(out)
	}
	if out, err := h.Run(ctx, "lsof", "-a", "-p", p, "-d", "cwd", "-Fn"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if rest, ok := strings.CutPrefix(line, "n"); ok && strings.HasPrefix(rest, "/") {
				info.Cwd = rest
			}
		}
	}
	return info
}

// homeVars are the environment variables that name an old agent's home.
var homeVars = map[string]bool{"HERMES_HOME": true, "OPENCLAW_STATE_DIR": true, "OPENCLAW_PROFILE": true}

// envWords picks NAME=value words out of ps -E output, where the
// environment follows the command line. ps prints no environment for a
// process it may not read, so the environment counts as read only when a
// variable every process has (PATH or HOME) is there. Only the variables
// the cutover reads are kept.
func envWords(out string) map[string]string {
	env := map[string]string{}
	seen := false
	for _, w := range strings.Fields(out) {
		k, v, ok := strings.Cut(w, "=")
		if !ok {
			continue
		}
		if k == "PATH" || k == "HOME" {
			seen = true
		}
		if homeVars[k] {
			env[k] = v
		}
	}
	if !seen {
		return nil
	}
	return env
}
