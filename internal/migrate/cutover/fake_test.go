package cutover

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// fakeSystem is a computer in memory: a process table, services with a
// state, and a log of every command run. Nothing real is touched.
type fakeSystem struct {
	mu       sync.Mutex
	goos     string
	home     string
	procs    []Process
	services map[string]*fakeService // by name
	// respawn: stopping these pids does nothing (a supervisor brings the
	// process straight back), to test the refusal.
	respawn map[int]bool
	// ignoreTerm: these pids end only when forced.
	ignoreTerm map[int]bool
	ran        []string
	stopped    []int
}

type fakeService struct {
	manager         string
	active, enabled bool
	// pids are the processes the service runs: they end when it stops
	// and come back when it starts.
	pids []Process
}

func newFake(home string) *fakeSystem {
	return &fakeSystem{goos: "linux", home: home, services: map[string]*fakeService{},
		respawn: map[int]bool{}, ignoreTerm: map[int]bool{}}
}

func (f *fakeSystem) GOOS() string     { return f.goos }
func (f *fakeSystem) UserHome() string { return f.home }
func (f *fakeSystem) Self() int        { return 1 }
func (f *fakeSystem) UID() int         { return 501 }
func (f *fakeSystem) Wait(time.Duration) {
}

func (f *fakeSystem) Processes(context.Context) ([]Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.procs), nil
}

func (f *fakeSystem) Alive(pid int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.procs, func(p Process) bool { return p.PID == pid })
}

func (f *fakeSystem) Stop(pid int, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, pid)
	if f.respawn[pid] || (f.ignoreTerm[pid] && !force) {
		return nil
	}
	f.procs = slices.DeleteFunc(f.procs, func(p Process) bool { return p.PID == pid })
	return nil
}

// run adds a process.
func (f *fakeSystem) run(p Process) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.procs = append(f.procs, p)
}

// service installs a service running the given processes.
func (f *fakeSystem) service(manager, name string, active, enabled bool, pids ...Process) {
	f.services[name] = &fakeService{manager: manager, active: active, enabled: enabled, pids: pids}
	if active {
		for _, p := range pids {
			f.run(p)
		}
	}
}

func (f *fakeSystem) setActive(s *fakeService, on bool) {
	if s.active == on {
		return
	}
	s.active = on
	for _, p := range s.pids {
		if on {
			f.procs = append(f.procs, p)
		} else {
			f.procs = slices.DeleteFunc(f.procs, func(q Process) bool { return q.PID == p.PID })
		}
	}
}

func (f *fakeSystem) Run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := name + " " + strings.Join(args, " ")
	f.ran = append(f.ran, line)
	last := ""
	if len(args) > 0 {
		last = args[len(args)-1]
	}
	switch name {
	case "systemctl":
		a := slices.DeleteFunc(slices.Clone(args), func(s string) bool { return s == "--user" || s == "--now" })
		s := f.services[last]
		if s == nil || s.manager != map[bool]string{true: Systemd, false: SystemdSystem}[slices.Contains(args, "--user")] {
			if a[0] == "is-enabled" {
				return "not-found", fmt.Errorf("exit status 4")
			}
			return "inactive", fmt.Errorf("exit status 3")
		}
		switch a[0] {
		case "is-active":
			if s.active {
				return "active", nil
			}
			return "inactive", fmt.Errorf("exit status 3")
		case "is-enabled":
			if s.enabled {
				return "enabled", nil
			}
			return "disabled", fmt.Errorf("exit status 1")
		case "disable":
			s.enabled = false
			if slices.Contains(args, "--now") {
				f.setActive(s, false)
			}
		case "enable":
			s.enabled = true
		case "start":
			f.setActive(s, true)
		case "stop":
			f.setActive(s, false)
		}
		return "", nil
	case "launchctl":
		label := last[strings.LastIndex(last, "/")+1:]
		s := f.services[label]
		switch args[0] {
		case "print":
			if s == nil || !s.active && !s.enabled {
				return "", fmt.Errorf("exit status 113")
			}
			if s.active {
				return "state = running", nil
			}
			return "state = not running", nil
		case "print-disabled":
			var b strings.Builder
			for n, s := range f.services {
				if s.manager == Launchd && !s.enabled {
					fmt.Fprintf(&b, "\t%q => disabled\n", n)
				}
			}
			return b.String(), nil
		case "bootout":
			f.setActive(s, false)
		case "disable":
			s.enabled = false
		case "enable":
			s.enabled = true
		case "bootstrap":
			for n, s := range f.services {
				if s.manager == Launchd && strings.HasSuffix(last, n+".plist") {
					f.setActive(s, true)
				}
			}
		}
		return "", nil
	case "schtasks":
		task := args[slices.Index(args, "/TN")+1]
		s := f.services[task]
		if s == nil {
			return "", fmt.Errorf("exit status 1")
		}
		switch {
		case slices.Contains(args, "/End"):
			f.setActive(s, false)
		case slices.Contains(args, "/DISABLE"):
			s.enabled = false
		case slices.Contains(args, "/ENABLE"):
			s.enabled = true
		case slices.Contains(args, "/Run"):
			f.setActive(s, true)
		}
		return "", nil
	case "powershell":
		for n, s := range f.services {
			if s.manager == Schtasks && strings.Contains(last, "'"+n+"'") {
				switch {
				case s.active:
					return "Running", nil
				case s.enabled:
					return "Ready", nil
				}
				return "Disabled", nil
			}
		}
		return "", fmt.Errorf("no task")
	}
	return "", fmt.Errorf("%s: not faked", name)
}

// changed reports whether any command that changes something ran.
func (f *fakeSystem) changed() []string {
	var out []string
	for _, l := range f.ran {
		w := strings.Fields(l)
		if len(w) < 2 {
			continue
		}
		switch {
		case slices.ContainsFunc(w, func(s string) bool {
			return slices.Contains([]string{"disable", "enable", "start", "stop", "bootout", "bootstrap", "/End", "/DISABLE", "/ENABLE", "/Run"}, s)
		}):
			out = append(out, l)
		}
	}
	for _, p := range f.stopped {
		out = append(out, fmt.Sprintf("stop %d", p))
	}
	return out
}
