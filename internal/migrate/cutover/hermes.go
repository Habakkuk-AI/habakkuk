package cutover

import (
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
	mayBe:    func(p Process) bool { return hermesish(p.Args()) },
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
	pause: func(store string, ids []string) ([]string, error) {
		return pauseJobs(store, ids, time.Now(), true)
	},
	resume: func(store, id string) error { return resumeJob(store, id, true) },
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
	brake: &brake{
		file: func(dir string) string { return filepath.Join(dir, "ESTOP") },
		text: func(now time.Time) string {
			return `{"engaged_at": "` + now.Format("2006-01-02T15:04:05.000000-07:00") + `", "reason": "` + pauseReason + `"}` + "\n"
		},
	},
}

type hermesHome struct{ name, dir string }

// hermesHomes are the home and its profiles.
func hermesHomes(dir string) []hermesHome {
	homes := []hermesHome{{"default", dir}}
	entries, _ := os.ReadDir(filepath.Join(dir, "profiles"))
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			homes = append(homes, hermesHome{e.Name(), filepath.Join(dir, "profiles", e.Name())})
		}
	}
	return homes
}

func hermesWASessionDirs(home string) []string {
	return []string{filepath.Join(home, "platforms", "whatsapp", "session"), filepath.Join(home, "whatsapp", "session")}
}

// hermesish reports a command line of the Hermes Agent program: its
// command, or its Python modules.
func hermesish(args []string) bool {
	for i, a := range args {
		if i > 3 {
			break
		}
		if p := program(a); p == "hermes" || p == "hermes-agent" {
			return true
		}
		if i > 0 && args[i-1] == "-m" && (strings.HasPrefix(a, "hermes_cli.") || strings.HasPrefix(a, "cron.") || a == "gateway.run") {
			return true
		}
	}
	return false
}

func hermesRole(p Process, dir, _ string) (string, bool) {
	args := p.Args()
	if len(args) == 0 {
		return "", false
	}
	// The WhatsApp bridge is a Node program in Hermes Agent's tree or home.
	if prog := p.Exe(); prog == "node" || prog == "nodejs" {
		for _, a := range args[1:] {
			if strings.HasSuffix(slashes(a), "whatsapp-bridge/bridge.js") {
				if s := wordAfter(args, "--session"); s != "" && !under(s, dir) {
					return "", false // the bridge of another home
				}
				return RoleBridge, true
			}
		}
		return "", false
	}
	if !hermesish(args) {
		return "", false
	}
	switch wordAfter(args, "-m") {
	case "hermes_cli.kanban_worker_client", "cron.scheduler":
		return RoleWorker, true
	case "hermes_cli.web_server":
		return RoleScheduler, true
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
		return RoleScheduler, true
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
				suffix := strings.TrimPrefix(strings.TrimSuffix(strings.TrimSuffix(name, ".service"), "hermes-gateway"), "-")
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
