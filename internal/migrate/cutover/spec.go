package cutover

import (
	"path/filepath"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/migrate"
)

// spec is what the cutover knows about one old agent: how it is
// installed, what its processes look like, and how its schedules are
// paused. Only facts about the other program live here; the steps are in
// cutover.go and finish.go.
type spec struct {
	// services are the services it may have installed for its home.
	services func(w where) []Service
	// role says whether a process belongs to the agent in dir, and what
	// it does there.
	role func(p Process, dir, userHome string) (string, bool)
	// mayBe reports whether a process could be the agent at all, to tell
	// a live pid file from a pid the system gave to another program.
	mayBe func(p Process) bool
	// pidFiles are the pid files its processes leave in dir.
	pidFiles func(dir string) []pidFile
	// jobs are its schedules that are on.
	jobs func(dir string) ([]Job, error)
	// pause turns schedules off in their store; it returns the ids it
	// turned off (ones already off are not returned).
	pause func(store string, ids []string) ([]string, error)
	// resume turns one back on.
	resume func(store, id string) error
	// waSessions are its WhatsApp linked-device sessions that hold a login.
	waSessions func(dir string) []string
	// brake is its own stop switch, when it has one.
	brake *brake
}

type pidFile struct{ path, role string }

// where is the old agent's home and the computer it is on.
type where struct {
	sys    System
	dir    string
	getenv func(string) string
}

func (w where) env(k string) string {
	if w.getenv == nil {
		return ""
	}
	return w.getenv(k)
}

// userUnits is the folder of systemd user units.
func (w where) userUnits() string {
	if x := w.env("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "systemd", "user")
	}
	return filepath.Join(w.sys.UserHome(), ".config", "systemd", "user")
}

// startupDir is the Windows Startup folder.
func (w where) startupDir() string {
	if a := w.env("APPDATA"); a != "" {
		return filepath.Join(a, "Microsoft", "Windows", "Start Menu", "Programs", "Startup")
	}
	return filepath.Join(w.sys.UserHome(), "AppData", "Roaming", "Microsoft", "Windows", "Start Menu", "Programs", "Startup")
}

var specs = map[string]*spec{
	migrate.Hermes:   hermesSpec,
	migrate.OpenClaw: openclawSpec,
}

// program is a command line's program name, without its folder, a
// Windows .exe or .cmd suffix, or the version some installers append.
func program(arg string) string {
	arg = strings.Trim(arg, `"'`)
	base := filepath.Base(strings.ReplaceAll(arg, `\`, "/"))
	for _, ext := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		base = strings.TrimSuffix(strings.ToLower(base), ext)
	}
	return base
}

// hasWord reports whether a word is one of the arguments.
func hasWord(args []string, word string) bool {
	for _, a := range args {
		if strings.Trim(a, `"'`) == word {
			return true
		}
	}
	return false
}

// wordAfter is the argument after flag ("--profile x" or "--profile=x").
func wordAfter(args []string, flags ...string) string {
	for i, a := range args {
		for _, f := range flags {
			if a == f && i+1 < len(args) {
				return strings.Trim(args[i+1], `"'`)
			}
			if v, ok := strings.CutPrefix(a, f+"="); ok {
				return strings.Trim(v, `"'`)
			}
		}
	}
	return ""
}

// under reports whether path is dir or inside it, comparing the way the
// file system would (case-blind on Windows paths).
func under(path, dir string) bool {
	if path == "" || dir == "" {
		return false
	}
	norm := func(p string) string {
		p = slashes(filepath.Clean(strings.Trim(p, `"'`)))
		if len(p) > 1 && p[1] == ':' {
			p = strings.ToLower(p)
		}
		return strings.TrimSuffix(p, "/")
	}
	p, d := norm(path), norm(dir)
	return p == d || strings.HasPrefix(p, d+"/")
}

// slashes writes a path with forward slashes, Windows ones too, on any
// system.
func slashes(p string) string { return strings.ReplaceAll(p, `\`, "/") }
