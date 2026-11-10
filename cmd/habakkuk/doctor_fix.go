package main

import (
	"fmt"
	"html"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/ops/security"
)

// hk doctor --fix applies the repairs that are mechanical and can be
// undone: it never needs a decision or a secret. Each one either keeps a
// copy of what it replaces or only tightens or adds something:
//
//   - config.yaml in an older format is rewritten in the current one, after
//     a timestamped copy of the old file (hk config migrate);
//   - folders Habakkuk needs and that are missing are created (owner-only);
//   - files and folders open to other users are made owner-only, and
//     skills and hook programs others could change lose that write bit
//     (the fixes hk security -fix makes);
//   - an installed background service whose program is gone (the binary
//     moved, or it was a temporary go run build) is written again for this
//     binary, with a .bak copy of the old file.
//
// Everything else stays advice: keys, logins, unknown config keys, channel
// settings, broken databases and anything outside $HABAKKUK_HOME.

// repair is one problem doctor --fix can fix.
type repair struct {
	what  string                 // what is wrong, one line
	apply func() (string, error) // makes the fix; returns what changed
}

// repairEnv is what planning repairs looks at; tests fake it.
type repairEnv struct {
	home     string
	userHome string // the user's home folder, for the service file
	goos     string
	exe      string // this binary
	path     string // PATH for a rewritten service file
	now      time.Time
}

func defaultRepairEnv(home string) repairEnv {
	e := repairEnv{home: home, goos: runtime.GOOS, path: os.Getenv("PATH"), now: time.Now()}
	e.userHome, _ = os.UserHomeDir()
	if exe, err := os.Executable(); err == nil {
		if p, err := filepath.EvalSymlinks(exe); err == nil {
			exe = p
		}
		e.exe = exe
	}
	if abs, err := filepath.Abs(home); err == nil {
		e.home = abs
	}
	return e
}

// planRepairs lists what doctor --fix would fix now, in the order it
// fixes them: the config first, so the later steps read the current one.
func planRepairs(e repairEnv) []repair {
	var out []repair
	cfg, cfgErr := config.Load(e.home)
	if cfgErr == nil && cfg.Report.NeedsMigration() {
		out = append(out, repair{
			what: fmt.Sprintf("config.yaml is in format version %d, not %d", cfg.Report.FileVersion, config.CurrentVersion),
			apply: func() (string, error) {
				res, err := config.MigrateFile(e.home, e.now, false)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("migrated config.yaml from version %d to %d; the old file is %s", res.From, res.To, res.Backup), nil
			},
		})
		cfg, cfgErr = config.Load(e.home) // the same settings, now read as current
	}
	if cfgErr == nil {
		out = append(out, missingFolders(e.home, cfg)...)
	}
	opts := security.Options{Home: e.home, Config: cfg, ConfigErr: cfgErr, Now: e.now}
	for _, f := range security.Audit(opts) {
		if f.Fixable() {
			out = append(out, repair{what: f.Message, apply: func() (string, error) {
				if err := f.Apply(); err != nil {
					return "", err
				}
				return f.Message + ": " + strings.TrimSuffix(f.Fix, " (hk security -fix)"), nil
			}})
		}
	}
	if r, ok := serviceRepair(e); ok {
		out = append(out, r)
	}
	return out
}

// missingFolders: the folders open() would create, listed so the fix says
// what it did.
func missingFolders(home string, cfg *config.Config) []repair {
	dirs := []string{home}
	for _, name := range slices.Sorted(maps.Keys(cfg.Agents)) {
		dirs = append(dirs, cfg.AgentDir(name), cfg.WorkspaceDir(name))
	}
	var out []repair
	for _, d := range dirs {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			continue
		}
		out = append(out, repair{what: d + " is missing", apply: func() (string, error) {
			if err := os.MkdirAll(d, 0o700); err != nil {
				return "", err
			}
			return "created " + d, nil
		}})
	}
	return out
}

// installedService is a service file the system runs hk serve from.
type installedService struct {
	path    string // where the system reads it
	program string // the binary it runs
	home    string // its HABAKKUK_HOME
	restart string // the command that reloads it
}

var (
	plistString  = regexp.MustCompile(`<string>([^<]*)</string>`)
	plistHome    = regexp.MustCompile(`<key>HABAKKUK_HOME</key>\s*<string>([^<]*)</string>`)
	plistPath    = regexp.MustCompile(`<key>PATH</key>\s*<string>([^<]*)</string>`)
	unitExec     = regexp.MustCompile(`(?m)^ExecStart=("(?:[^"\\]|\\.)*"|\S+)`)
	unitEnv      = regexp.MustCompile(`(?m)^Environment="((?:[^"\\]|\\.)*)"`)
	unitUnescape = strings.NewReplacer(`\\`, `\`, `\"`, `"`, "%%", "%")
)

// readService finds and parses the installed service file, if any.
func readService(e repairEnv) (installedService, bool) {
	var s installedService
	switch e.goos {
	case "darwin":
		s.path = filepath.Join(e.userHome, "Library", "LaunchAgents", launchdLabel+".plist")
		s.restart = fmt.Sprintf("launchctl bootout gui/$(id -u)/%s; launchctl bootstrap gui/$(id -u) %s", launchdLabel, shellQuote(s.path))
	case "linux":
		s.path = filepath.Join(e.userHome, ".config", "systemd", "user", "habakkuk.service")
		s.restart = "systemctl --user daemon-reload && systemctl --user restart habakkuk"
	default:
		return s, false
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return s, false
	}
	text := string(data)
	if e.goos == "darwin" {
		if i := strings.Index(text, "<key>ProgramArguments</key>"); i >= 0 {
			if m := plistString.FindStringSubmatch(text[i:]); m != nil {
				s.program = html.UnescapeString(m[1])
			}
		}
		if m := plistHome.FindStringSubmatch(text); m != nil {
			s.home = html.UnescapeString(m[1])
		}
		return s, s.program != ""
	}
	if m := unitExec.FindStringSubmatch(text); m != nil {
		s.program = unitUnescape.Replace(strings.Trim(m[1], `"`))
	}
	for _, m := range unitEnv.FindAllStringSubmatch(text, -1) {
		if v, ok := strings.CutPrefix(unitUnescape.Replace(m[1]), "HABAKKUK_HOME="); ok {
			s.home = v
		}
	}
	return s, s.program != ""
}

// servicePath is the PATH a service file sets, or "".
func servicePath(goos, text string) string {
	if goos == "darwin" {
		if m := plistPath.FindStringSubmatch(text); m != nil {
			return html.UnescapeString(m[1])
		}
		return ""
	}
	for _, m := range unitEnv.FindAllStringSubmatch(text, -1) {
		if v, ok := strings.CutPrefix(unitUnescape.Replace(m[1]), "PATH="); ok {
			return v
		}
	}
	return ""
}

// serviceProblem says what is wrong with the installed service for this
// home, or "".
func serviceProblem(e repairEnv) (installedService, string) {
	s, ok := readService(e)
	if !ok || filepath.Clean(s.home) != filepath.Clean(e.home) {
		return s, "" // none, or another home's service: not ours to touch
	}
	if _, err := os.Stat(s.program); err == nil {
		return s, ""
	}
	return s, fmt.Sprintf("the background service at %s runs %s, which no longer exists", s.path, s.program)
}

// serviceRepair writes the service file again for this binary, keeping the
// PATH the old one set. It leaves alone a service for another home, and
// one it would point at a temporary go run build.
func serviceRepair(e repairEnv) (repair, bool) {
	s, problem := serviceProblem(e)
	if problem == "" || e.exe == "" || strings.Contains(e.exe, "go-build") {
		return repair{}, false
	}
	return repair{what: problem, apply: func() (string, error) {
		old, err := os.ReadFile(s.path)
		if err != nil {
			return "", err
		}
		path := servicePath(e.goos, string(old))
		if path == "" {
			path = e.path
		}
		text := systemdUnit(e.exe, e.home, path)
		if e.goos == "darwin" {
			text = launchdPlist(e.exe, e.home, path)
		}
		if err := store.WriteFileAtomic(s.path+".bak", old, 0o600); err != nil {
			return "", err
		}
		if err := store.WriteFileAtomic(s.path, []byte(text), 0o600); err != nil {
			return "", err
		}
		return fmt.Sprintf("pointed the background service at %s (the old file is %s.bak); reload it with: %s", e.exe, s.path, s.restart), nil
	}}, true
}

// serviceDoctor is doctor's check of the installed service. It returns the
// number of problems.
func serviceDoctor(e repairEnv, w io.Writer) int {
	if _, problem := serviceProblem(e); problem != "" {
		fmt.Fprintf(w, "service: %s; fix: hk doctor --fix, or run hk setup -service again\n", strings.TrimPrefix(problem, "the background service at "))
		return 1
	}
	return 0
}

// applyRepairs runs doctor --fix's repairs and reports each one. It
// returns how many failed.
func applyRepairs(w io.Writer, repairs []repair) int {
	if len(repairs) == 0 {
		fmt.Fprintln(w, "Nothing to fix automatically.")
		return 0
	}
	failed := 0
	for _, r := range repairs {
		done, err := r.apply()
		if err != nil {
			fmt.Fprintf(w, "could not fix: %s: %v\n", r.what, err)
			failed++
			continue
		}
		fmt.Fprintf(w, "fixed: %s\n", done)
	}
	return failed
}
