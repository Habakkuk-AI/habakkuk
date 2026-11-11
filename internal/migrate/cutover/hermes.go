package cutover

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Hermes Agent, as it runs. Facts, from its public source:
//
//   - Its gateway runs as `hermes [-p PROFILE] gateway run` or
//     `python -m hermes_cli.main ... gateway run`. Cron runs inside it, on
//     a timer, and so does its board dispatcher. A job may run in its own
//     `python -m cron.scheduler --external-worker-file ...` process, board
//     workers run as `python -m hermes_cli.kanban_worker_client`, and the
//     desktop dashboard backend ticks cron too. Its WhatsApp channel is a
//     Node bridge: `node .../whatsapp-bridge/bridge.js --session DIR`.
//   - Services: systemd user units hermes-gateway.service (default home),
//     hermes-gateway-<profile>.service and hermes-gateway-<hash>.service
//     (other homes), older hermes.service, all Restart=always; system units
//     of the same names under /etc/systemd/system; launchd agents
//     ai.hermes.gateway[-<suffix>] that start at login and restart on a
//     crash; Windows scheduled tasks Hermes_Gateway[_<suffix>] that run
//     <home>\gateway-service\<task>.vbs, or the same .vbs or .cmd in the
//     Startup folder.
//   - Pid files in each home: gateway.pid (JSON with "pid", or a number),
//     cron.pid, and the WhatsApp bridge's <session>/bridge.pid.
//   - Schedules: cron/jobs.json in each home (profiles/<name>/ for the
//     others). A job fires when "enabled" is on and it has no pause
//     marker (state "paused" or paused_at). Each tick reads the file again.
//   - ESTOP in the home is its own brake: while the file is there, no new
//     cron run, board run or gateway turn starts. hermes resume removes it.

var hermesSpec = &spec{
	services: hermesServices,
	role:     hermesRole,
	home:     hermesHomeOf,
	mayBe:    func(p Process) bool { return hermesish(p) },
	pidFiles: func(dir string) []pidFile {
		var out []pidFile
		for _, h := range hermesHomes(dir) {
			out = append(out,
				pidFile{filepath.Join(h.dir, "gateway.pid"), RoleGateway},
				pidFile{filepath.Join(h.dir, "cron.pid"), RoleScheduler})
			for _, s := range hermesWASessionDirs(h.dir) {
				out = append(out, pidFile{filepath.Join(s, "bridge.pid"), RoleBridge})
			}
		}
		return out
	},
	jobs: func(dir string) ([]Job, error) {
		var out []Job
		var errs []string
		for _, h := range hermesHomes(dir) {
			jobs, err := runnableJobs(filepath.Join(h.dir, "cron", "jobs.json"), h.name)
			if err != nil {
				errs = append(errs, err.Error())
			}
			out = append(out, jobs...)
		}
		return out, joinErrs(errs)
	},
	store: func(path string) jobStore {
		return jsonStore(path, []string{"enabled", "state", "paused_at", "paused_reason"}, func(now time.Time) fields {
			// What its own pause command writes.
			return fields{"enabled": rawOf(false), "state": rawOf("paused"),
				"paused_at": rawOf(now.Format("2006-01-02T15:04:05.000000-07:00")), "paused_reason": rawOf(pauseReason)}
		})
	},
	waSessions: func(dir string) []string {
		var out []string
		for _, h := range hermesHomes(dir) {
			for _, s := range hermesWASessionDirs(h.dir) {
				if p := filepath.Join(s, "creds.json"); fileExists(p) {
					out = append(out, s)
				}
			}
		}
		return out
	},
	// Each home has its own: a profile's gateway reads its profile's.
	brakes: func(dir string) []string {
		var out []string
		for _, h := range hermesHomes(dir) {
			out = append(out, filepath.Join(h.dir, "ESTOP"))
		}
		return out
	},
	brakeText: func(now time.Time) string {
		return `{"engaged_at": "` + now.Format("2006-01-02T15:04:05.000000-07:00") + `", "reason": "` + pauseReason + `"}` + "\n"
	},
	scopes: hermesScopes,
}

type hermesHome struct{ name, dir string }

// hermesHomes are the home and its profiles: the folders under profiles/
// that hold a profile's own files.
func hermesHomes(dir string) []hermesHome {
	homes := []hermesHome{{"default", dir}}
	entries, _ := os.ReadDir(filepath.Join(dir, "profiles"))
	for _, e := range entries {
		p := filepath.Join(dir, "profiles", e.Name())
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		for _, mark := range []string{"config.yaml", ".env", "SOUL.md", "profile.yaml", "auth.json", "state.db", "cron", "gateway.pid"} {
			if fileExists(filepath.Join(p, mark)) {
				homes = append(homes, hermesHome{e.Name(), p})
				break
			}
		}
	}
	return homes
}

func hermesWASessionDirs(home string) []string {
	return []string{filepath.Join(home, "platforms", "whatsapp", "session"), filepath.Join(home, "whatsapp", "session")}
}

// hermesish reports a command line of the Hermes Agent program: its
// command, or a Python interpreter running its modules or its command.
// Python's own flags (-u, -X utf8) may come first.
func hermesish(p Process) bool {
	switch exe := p.Exe(); {
	case exe == "hermes" || exe == "hermes-agent":
		return true
	case !isPython(exe):
		return false
	}
	args := p.Args()
	switch m := wordAfter(args, "-m"); {
	case strings.HasPrefix(m, "hermes_cli."), m == "gateway.run":
		return true
	case m == "cron.scheduler":
		// A module name other projects use too: only with the flag
		// Hermes Agent starts its job workers with.
		return slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--external-worker-file") })
	}
	// python [flags] /path/to/hermes ...: the first word that is not a
	// flag or a flag's value.
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-X" || a == "-W":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			p := program(a)
			return p == "hermes" || p == "hermes-agent"
		}
	}
	return false
}

func isPython(exe string) bool {
	return exe == "python" || exe == "pythonw" || exe == "py" || strings.HasPrefix(exe, "python3") || strings.HasPrefix(exe, "python2")
}

// hermesHomeOf: HERMES_HOME names the home a process uses; a profile
// flag names one inside the default home; no HERMES_HOME means the
// default home.
func hermesHomeOf(w where, p Process, env map[string]string) (verdict, string) {
	uh := w.sys.UserHome()
	def := isDefaultHome(w.dir, filepath.Join(uh, ".hermes"))
	if h, ok := env["HERMES_HOME"]; ok && h != "" {
		if under(expandTilde(h, uh), w.dir) {
			return ours, "its HERMES_HOME is " + h
		}
		return theirs, "its HERMES_HOME is " + h
	}
	if prof := wordAfter(p.Args(), "-p", "--profile"); prof != "" && prof != "default" {
		if def && fileExists(filepath.Join(w.dir, "profiles", prof)) {
			return ours, "it runs profile " + prof + " of this home"
		}
		if env != nil {
			return theirs, "its profile " + prof + " is not in this home"
		}
		return unknown, ""
	}
	if env != nil {
		if def {
			return ours, "it has no HERMES_HOME, so it uses the default home"
		}
		return theirs, "it has no HERMES_HOME, so it uses the default home"
	}
	return unknown, ""
}

func hermesRole(p Process, dir, userHome string) (string, bool) {
	args := p.Args()
	if len(args) == 0 {
		return "", false
	}
	// The WhatsApp bridge is a Node program, bridge.js, given the session
	// folder; its folder is Hermes Agent's tree or a copy in the home.
	if prog := p.Exe(); prog == "node" || prog == "nodejs" {
		for _, a := range args[1:] {
			s := slashes(a)
			if !strings.HasSuffix(s, "/bridge.js") && s != "bridge.js" {
				continue
			}
			session := wordAfter(args, "--session")
			if strings.Contains(s, "whatsapp-bridge/") || under(expandTilde(a, userHome), dir) || session != "" {
				return RoleBridge, true
			}
		}
		return "", false
	}
	if !hermesish(p) {
		return "", false
	}
	switch wordAfter(args, "-m") {
	case "hermes_cli.kanban_worker_client", "cron.scheduler":
		return RoleWorker, true
	case "hermes_cli.web_server":
		return RoleDashboard, true
	}
	sub := hermesSubcommand(args)
	switch sub {
	case "gateway":
		// "gateway" alone runs it too; gateway status, stop and the
		// like are short commands, not the gateway.
		next := wordAfterSub(args, "gateway")
		if next == "" || next == "run" || strings.HasPrefix(next, "-") {
			return RoleGateway, true
		}
	case "kanban":
		if wordAfterSub(args, "kanban") == "daemon" {
			return RoleWorker, true
		}
	case "dashboard", "web":
		return RoleDashboard, true
	}
	return "", false
}

// hermesSubcommand is the first word after the program and its global
// flags (-p PROFILE, --profile PROFILE).
func hermesSubcommand(args []string) string {
	start := 1
	for i, a := range args {
		if program(a) == "hermes" || program(a) == "hermes-agent" || a == "hermes_cli.main" {
			start = i + 1
			break
		}
	}
	for i := start; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-p" || a == "--profile":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a
		}
	}
	return ""
}

func wordAfterSub(args []string, sub string) string {
	i := slices.Index(args, sub)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

// hermesServices finds the gateway services installed for this home.
func hermesServices(w where) []Service {
	defaultHome := filepath.Join(w.sys.UserHome(), ".hermes")
	isDefault := under(w.dir, defaultHome) && under(defaultHome, w.dir)
	profiles := map[string]bool{}
	for _, h := range hermesHomes(w.dir)[1:] {
		profiles[h.name] = true
	}
	// ours: a service file names this home (or a profile in it); one that
	// names no home at all is the default home's.
	ours := func(text, suffix string) bool {
		if mentionsDir(text, w.dir) {
			return true
		}
		if strings.Contains(text, "HERMES_HOME") {
			return false
		}
		return isDefault && (suffix == "" || profiles[suffix])
	}
	var out []Service
	switch w.sys.GOOS() {
	case "linux":
		for _, d := range []struct{ dir, manager string }{{w.userUnits(), Systemd}, {"/etc/systemd/system", SystemdSystem}} {
			for _, f := range globAll(d.dir, "hermes-gateway*.service", "hermes.service") {
				name := filepath.Base(f)
				suffix := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSuffix(name, ".service"), "hermes-gateway"), "-")
				if name == "hermes.service" {
					suffix = ""
				}
				if ours(readSmall(f), suffix) {
					out = append(out, Service{Manager: d.manager, Name: name, File: f, Role: RoleGateway})
				}
			}
		}
	case "darwin":
		for _, f := range globAll(filepath.Join(w.sys.UserHome(), "Library", "LaunchAgents"), "ai.hermes.gateway*.plist") {
			label := strings.TrimSuffix(filepath.Base(f), ".plist")
			suffix := strings.TrimPrefix(strings.TrimPrefix(label, "ai.hermes.gateway"), "-")
			if ours(readSmall(f), suffix) {
				out = append(out, Service{Manager: Launchd, Name: label, File: f, Role: RoleGateway})
			}
		}
	case "windows":
		// Its task runs a launcher kept in the home, named after the task.
		seen := map[string]bool{}
		for _, h := range hermesHomes(w.dir) {
			for _, f := range globAll(filepath.Join(h.dir, "gateway-service"), "Hermes_Gateway*.vbs", "Hermes_Gateway*.cmd") {
				task := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(f), ".vbs"), ".cmd")
				if !seen[task] {
					seen[task] = true
					out = append(out, Service{Manager: Schtasks, Name: task, Role: RoleGateway})
				}
			}
		}
		for _, f := range globAll(w.startupDir(), "Hermes_Gateway*.vbs", "Hermes_Gateway*.cmd") {
			task := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(f), ".vbs"), ".cmd")
			if seen[task] || ours(readSmall(f), strings.TrimPrefix(strings.TrimPrefix(task, "Hermes_Gateway"), "_")) {
				out = append(out, Service{Manager: Startup, Name: filepath.Base(f), File: f, Role: RoleGateway})
			}
		}
	}
	return out
}

// hermesScopes are the systemd scopes (hermes-worker-*) its job and board
// workers run in, when a worker of this home is in one. Stopping a scope
// stops everything in it; there is nothing to disable.
func hermesScopes(ctx context.Context, w where, procs []Running) []string {
	if w.sys.GOOS() != "linux" || len(procs) == 0 {
		return nil
	}
	out, err := w.sys.Run(ctx, "systemctl", "--user", "list-units", "--type=scope", "--all", "--plain", "--no-legend", "--no-pager", "hermes-worker-*")
	if err != nil {
		return nil
	}
	var cgroups []string
	for _, p := range procs {
		if cg := w.sys.Inspect(ctx, p.PID).Cgroup; cg != "" {
			cgroups = append(cgroups, cg)
		}
	}
	var scopes []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasPrefix(f[0], "hermes-worker-") || !strings.HasSuffix(f[0], ".scope") {
			continue
		}
		if slices.ContainsFunc(cgroups, func(cg string) bool { return strings.Contains(cg, "/"+f[0]) }) {
			scopes = append(scopes, f[0])
		}
	}
	return scopes
}
