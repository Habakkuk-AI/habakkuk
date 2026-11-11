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
	// Processes are its processes running now, by role.
	Processes []Running
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
	Role string // gateway, worker, scheduler, bridge
	// Via says how it was found: "process list" or the pid file.
	Via string
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
	for _, p := range s.Processes {
		if p.Role == RoleGateway || p.Role == RoleScheduler || p.Role == RoleWorker {
			return true
		}
	}
	return false
}

// Process roles.
const (
	RoleGateway   = "gateway"
	RoleWorker    = "worker"
	RoleScheduler = "scheduler"
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
		{RoleWorker, "worker", "workers"}, {RoleScheduler, "scheduler", "schedulers"}, {RoleBridge, "channel bridge", "channel bridges"},
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
	procs, err := sys.Processes(ctx)
	if err != nil {
		st.Problems = append(st.Problems, "could not list the running processes: "+err.Error())
	}
	byPID := map[int]Process{}
	for _, p := range procs {
		byPID[p.PID] = p
	}
	seen := map[int]bool{sys.Self(): true}
	for _, p := range procs {
		if seen[p.PID] {
			continue
		}
		if role, ok := sp.role(p, dir, sys.UserHome()); ok {
			seen[p.PID] = true
			st.Processes = append(st.Processes, Running{Process: p, Role: role, Via: "process list"})
		}
	}
	// A pid file names a process the list may not show as ours (a
	// renamed interpreter, a list we could not read).
	for _, pf := range sp.pidFiles(dir) {
		pid, ok := readPID(pf.path)
		if !ok {
			continue
		}
		if !sys.Alive(pid) {
			st.Stale = append(st.Stale, pf.path)
			continue
		}
		if seen[pid] {
			continue
		}
		p, listed := byPID[pid]
		if listed && !sp.mayBe(p) {
			// The pid was reused by an unrelated program.
			st.Stale = append(st.Stale, pf.path)
			continue
		}
		if !listed {
			p = Process{PID: pid, Cmdline: "(pid " + strconv.Itoa(pid) + " from " + filepath.Base(pf.path) + ")"}
		}
		seen[pid] = true
		st.Processes = append(st.Processes, Running{Process: p, Role: pf.role, Via: pf.path})
	}
	slices.SortFunc(st.Processes, func(a, b Running) int { return a.PID - b.PID })
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
