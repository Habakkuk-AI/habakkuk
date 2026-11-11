// Package cutover makes a move from another agent end with exactly one
// agent running. hk migrate imports a setup with its schedules paused;
// this package then finds out whether the old agent still runs (its
// services, processes, pid files and its own schedules), and, when you
// say so, stops it, pauses its schedules and resumes the imported ones.
// Everything it changes is recorded, so Rollback can put it back.
//
// Detect only reads. Finish and Rollback change things outside Habakkuk
// only through System, which tests fake.
package cutover

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/migrate"
)

// Options say which old agent to look at.
type Options struct {
	From   string // migrate.Hermes or migrate.OpenClaw
	Source string // its home; "" is its default home
	// Getenv reads the environment, for the source's home overrides; nil
	// is os.Getenv.
	Getenv func(string) string
	Now    func() time.Time
}

func (o Options) now() time.Time {
	if o.Now == nil {
		return time.Now()
	}
	return o.Now()
}

// Runs reports whether a source is an agent that runs in the background:
// Claude Code and Codex do not.
func Runs(from string) bool { return specs[from] != nil }

// Status is what is running of the old agent, and its schedules.
type Status struct {
	From  string
	Label string
	Dir   string
	// Services are its installed services, with their state.
	Services []ServiceStatus
	// Processes are its processes running now, by role: each one shown to
	// belong to this home.
	Processes []Running
	// Maybe are the user's processes that look like the old agent's but
	// could not be tied to this home. They are listed, never stopped.
	Maybe []Running
	// Scopes are the systemd scopes its workers run in (Linux).
	Scopes []string
	// Jobs are its own schedules that are on.
	Jobs []Job
	// Stale are pid and lock files left by processes that are gone.
	Stale []string
	// Problems are checks that could not run (no ps, say); they are named
	// so a quiet result is not taken for a clean one.
	Problems []string
}

// ServiceStatus is one service and its state.
type ServiceStatus struct {
	Service
	ServiceState
}

// Running is one process of the old agent.
type Running struct {
	Process
	Role string // gateway, worker, scheduler, dashboard, bridge
	// Via says how it was found: "process list" or the pid file.
	Via string
	// Why says how it is tied to the home, or why it could not be.
	Why string
}

// Job is one of the old agent's own schedules.
type Job struct {
	ID    string
	Name  string
	Agent string // the profile or agent it belongs to, as the source names it
	Store string // the file or database that holds it
}

// Live reports whether anything of the old agent runs, or would start by
// itself.
func (s *Status) Live() bool {
	if len(s.Processes) > 0 {
		return true
	}
	for _, sv := range s.Services {
		if sv.Active || sv.Enabled {
			return true
		}
	}
	return false
}

// SchedulerRunning reports whether a process that fires the old agent's
// schedules runs now.
func (s *Status) SchedulerRunning() bool {
	return slices.ContainsFunc(s.Processes, func(p Running) bool { return firesSchedules(p.Role) })
}

// MaybeScheduler lists the processes that may be the old agent's and
// would fire its schedules if they are.
func (s *Status) MaybeScheduler() []Running {
	var out []Running
	for _, p := range s.Maybe {
		if firesSchedules(p.Role) {
			out = append(out, p)
		}
	}
	return out
}

// UsesChannels reports whether a process that polls the old agent's
// channels runs now: a gateway, a scheduler (which delivers) or a bridge.
// The dashboard alone does not.
func (s *Status) UsesChannels() bool {
	return slices.ContainsFunc(s.Processes, func(p Running) bool {
		return p.Role == RoleGateway || p.Role == RoleScheduler || p.Role == RoleBridge
	})
}

func firesSchedules(role string) bool {
	switch role {
	case RoleGateway, RoleScheduler, RoleWorker, RoleDashboard:
		return true
	}
	return false
}

// Process roles.
const (
	RoleGateway   = "gateway"
	RoleWorker    = "worker"
	RoleScheduler = "scheduler"
	RoleDashboard = "dashboard" // ticks the schedules, polls no channel
	RoleBridge    = "bridge"
)

// Summary is the one line the plan shows: "Hermes Agent is still running:
// gateway (systemd hermes-gateway), 1 worker, 37 active schedules".
func (s *Status) Summary() string {
	var parts []string
	roles := map[string]int{}
	pids := map[int]bool{}
	for _, p := range s.Processes {
		roles[p.Role]++
		pids[p.PID] = true
	}
	var svcs []string
	for _, sv := range s.Services {
		if sv.Active {
			svcs = append(svcs, sv.String())
		}
	}
	if n := roles[RoleGateway]; n > 0 || len(svcs) > 0 {
		g := "gateway"
		if n > 1 {
			g = fmt.Sprintf("%d gateways", n)
		}
		if len(svcs) > 0 {
			g += " (" + strings.Join(svcs, ", ") + ")"
		}
		parts = append(parts, g)
	}
	for _, r := range []struct{ role, one, many string }{
		{RoleWorker, "worker", "workers"}, {RoleScheduler, "scheduler", "schedulers"}, {RoleDashboard, "dashboard", "dashboards"},
		{RoleBridge, "channel bridge", "channel bridges"},
	} {
		if n := roles[r.role]; n > 0 {
			parts = append(parts, count(n, r.one, r.many))
		}
	}
	if len(s.Jobs) > 0 {
		parts = append(parts, count(len(s.Jobs), "active schedule", "active schedules"))
	}
	switch {
	case len(s.Processes) > 0 || len(svcs) > 0:
		return s.Label + " is still running: " + strings.Join(parts, ", ")
	case s.Live():
		var on []string
		for _, sv := range s.Services {
			if sv.Enabled {
				on = append(on, sv.String())
			}
		}
		line := s.Label + " is stopped, but starts again by itself (" + strings.Join(on, ", ") + ")"
		if len(s.Jobs) > 0 {
			line += ", with " + count(len(s.Jobs), "active schedule", "active schedules")
		}
		return line
	case len(s.Jobs) > 0:
		return s.Label + " is not running, but " + count(len(s.Jobs), "of its schedules is", "of its schedules are") + " still on"
	}
	return s.Label + " is not running"
}

// Detect looks at what of the old agent runs. It changes nothing.
func Detect(ctx context.Context, sys System, o Options) (*Status, error) {
	sp := specs[o.From]
	if sp == nil {
		return nil, fmt.Errorf("%s does not run in the background; there is nothing to stop", migrate.Label(o.From))
	}
	dir := o.Source
	if dir == "" {
		dir = migrate.DefaultSource(o.From, migrate.Options{Getenv: o.Getenv, UserHome: sys.UserHome()})
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	st := &Status{From: o.From, Label: migrate.Label(o.From), Dir: dir}
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	for _, svc := range sp.services(where{sys: sys, dir: dir, getenv: getenv}) {
		if s, ok := state(ctx, sys, svc); ok {
			st.Services = append(st.Services, ServiceStatus{Service: svc, ServiceState: s})
		}
	}
	switch sys.GOOS() {
	case "linux", "darwin", "windows":
	default:
		st.Problems = append(st.Problems, "its services are not looked for on "+sys.GOOS())
	}
	w := where{sys: sys, dir: dir, getenv: getenv}
	var units []string
	for _, sv := range st.Services {
		units = append(units, sv.Name)
	}
	procs, err := sys.Processes(ctx)
	if err != nil {
		st.Problems = append(st.Problems, "could not list the running processes: "+err.Error())
	} else if len(procs) == 0 {
		st.Problems = append(st.Problems, "the process list came back empty")
	}
	byPID := map[int]Process{}
	for _, p := range procs {
		byPID[p.PID] = p
	}
	pidFiles := map[int]pidFile{}
	for _, pf := range sp.pidFiles(dir) {
		pid, ok := readPID(pf.path)
		if !ok {
			continue
		}
		if !sys.Alive(pid) {
			st.Stale = append(st.Stale, pf.path)
			continue
		}
		if p, listed := byPID[pid]; listed && !sp.mayBe(p) {
			st.Stale = append(st.Stale, pf.path) // the pid now belongs to another program
			continue
		}
		pidFiles[pid] = pf
		if _, listed := byPID[pid]; !listed {
			// Alive but not in the list: tied to the home by its pid file.
			st.Processes = append(st.Processes, Running{Process: Process{PID: pid,
				Cmdline: "(pid " + strconv.Itoa(pid) + " from " + filepath.Base(pf.path) + ")"}, Role: pf.role, Via: pf.path, Why: "its pid file is in the home"})
		}
	}
	// Ours: tied to the home by its pid file, its command line, its
	// folder, its service, its environment, or a parent that is ours.
	verdicts := map[int]verdict{}
	var cands []Running
	for _, p := range procs {
		if p.PID == sys.Self() || p.Other {
			continue
		}
		role, ok := sp.role(p, dir, sys.UserHome())
		pf, named := pidFiles[p.PID]
		if !ok && !named {
			continue
		}
		if !ok {
			role = pf.role
		}
		r := Running{Process: p, Role: role, Via: "process list"}
		if named {
			verdicts[p.PID], r.Why = ours, "its pid file "+pf.path+" is in the home"
		} else {
			verdicts[p.PID], r.Why = proof(sp, w, p, sys.Inspect(ctx, p.PID), units)
		}
		cands = append(cands, r)
	}
	for changed := true; changed; {
		changed = false
		for i, r := range cands {
			if verdicts[r.PID] == unknown && verdicts[r.PPID] == ours && r.PPID > 1 {
				verdicts[r.PID], cands[i].Why = ours, "its parent "+strconv.Itoa(r.PPID)+" is"
				changed = true
			}
		}
	}
	for _, r := range cands {
		switch verdicts[r.PID] {
		case ours:
			st.Processes = append(st.Processes, r)
		case unknown:
			if r.Why == "" {
				r.Why = "nothing ties it to " + dir
			}
			st.Maybe = append(st.Maybe, r)
		}
	}
	if sp.scopes != nil {
		st.Scopes = sp.scopes(ctx, w, st.Processes)
	}
	slices.SortFunc(st.Processes, func(a, b Running) int { return a.PID - b.PID })
	slices.SortFunc(st.Maybe, func(a, b Running) int { return a.PID - b.PID })
	jobs, err := sp.jobs(dir)
	if err != nil {
		st.Problems = append(st.Problems, "could not read its schedules: "+err.Error())
	}
	st.Jobs = jobs
	return st, nil
}

// readPID reads a pid file: a number on the first line, or JSON with a
// "pid" field.
func readPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	text := strings.TrimSpace(string(data))
	if n, err := strconv.Atoi(strings.TrimSpace(firstLine(text))); err == nil && n > 0 {
		return n, true
	}
	if i := strings.Index(text, `"pid"`); i >= 0 {
		rest := strings.TrimLeft(text[i+len(`"pid"`):], " \t\r\n:")
		end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
		if end < 0 {
			end = len(rest)
		}
		if n, err := strconv.Atoi(rest[:end]); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
