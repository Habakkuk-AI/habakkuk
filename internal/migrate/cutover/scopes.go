package cutover

import (
	"context"
	"strconv"
	"strings"
)

// Worker scopes. An old agent may run each worker in a transient systemd
// user scope of its own (spec.scopePrefix). Whatever a worker starts stays
// in its scope after the worker itself ends, so a scope is judged on its
// own, not only through a worker found running: it belongs to this home
// when a process of the home runs in it, or when its own command line
// and the processes in it point here and none points elsewhere. Stopping
// a scope stops everything in it; there is nothing to disable.

// scopeUnit is one scope: its name and description, which for a scope
// systemd-run made is the command it started.
type scopeUnit struct{ name, desc string }

// listScopes lists the user's scopes whose names start with prefix.
func listScopes(ctx context.Context, sys System, prefix string) []scopeUnit {
	out, err := sys.Run(ctx, "systemctl", "--user", "list-units", "--type=scope", "--all", "--plain", "--no-legend", "--no-pager", prefix+"*")
	if err != nil {
		return nil
	}
	var list []scopeUnit
	for _, line := range strings.Split(out, "\n") {
		// UNIT LOAD ACTIVE SUB DESCRIPTION
		f := splitFields(strings.TrimSpace(line), 5)
		if len(f) == 0 || !strings.HasPrefix(f[0], prefix) || !strings.HasSuffix(f[0], ".scope") {
			continue
		}
		u := scopeUnit{name: f[0]}
		if len(f) == 5 {
			u.desc = f[4]
		}
		list = append(list, u)
	}
	return list
}

// inScope reports whether a process's control group (/proc/PID/cgroup:
// one line per hierarchy) is the scope or inside it.
func inScope(cgroup, scope string) bool {
	for _, line := range strings.Split(cgroup, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, "/"+scope) || strings.Contains(line, "/"+scope+"/") {
			return true
		}
	}
	return false
}

// scopeResult is what findScopes found.
type scopeResult struct {
	// scopes are the scopes of this home, to stop.
	scopes []string
	// found are processes in those scopes not already known to be the
	// home's: what its workers left behind.
	found []Running
	// unsure are processes in scopes that could not be tied to this home
	// or another: listed, never stopped.
	unsure []Running
}

// findScopes finds the worker scopes of this home. known are the
// processes already shown to be the home's, and listed every process
// already looked at, which it does not report again.
func findScopes(ctx context.Context, sp *spec, w where, procs []Process, known []Running, listed map[int]bool) scopeResult {
	var res scopeResult
	if sp.scopePrefix == "" || w.sys.GOOS() != "linux" {
		return res
	}
	units := listScopes(ctx, w.sys, sp.scopePrefix)
	if len(units) == 0 {
		return res
	}
	members := map[string][]scopeMember{}
	add := func(p Process, info ProcInfo, isKnown bool) {
		for _, u := range units {
			if inScope(info.Cgroup, u.name) {
				members[u.name] = append(members[u.name], scopeMember{p, info, isKnown})
				return
			}
		}
	}
	knownPID := map[int]bool{}
	for _, r := range known {
		knownPID[r.PID] = true
		add(r.Process, w.sys.Inspect(ctx, r.PID), true)
	}
	for _, p := range procs {
		if p.Other || p.PID <= 1 || p.PID == w.sys.Self() || knownPID[p.PID] {
			continue
		}
		add(p, w.sys.Inspect(ctx, p.PID), false)
	}
	for _, u := range units {
		v, why := judgeScope(sp, w, u, members[u.name])
		if v == theirs {
			continue
		}
		for _, m := range members[u.name] {
			if m.known || listed[m.PID] && v != ours {
				continue
			}
			r := Running{Process: m.Process, Role: RoleLeftover, Via: u.name}
			if v == ours {
				r.Why = "it runs in " + u.name + ", a worker scope of the home: " + why
				res.found = append(res.found, r)
			} else {
				r.Why = "it runs in " + u.name + ", one of its worker scopes, but nothing ties the scope to " + w.dir
				res.unsure = append(res.unsure, r)
			}
		}
		if v == ours {
			res.scopes = append(res.scopes, u.name)
		}
	}
	return res
}

// scopeMember is a process in a scope; known when it is already shown to
// be the home's.
type scopeMember struct {
	Process
	info  ProcInfo
	known bool
}

// judgeScope decides whether a scope is this home's, and says why. A
// process of the home in it decides. Otherwise its command line (its
// description) and the processes in it are asked what home they use: a
// process started in the scope inherits the worker's environment, so its
// home variable, or a readable environment with none, names the worker's
// home. Any answer of another home wins over ours.
func judgeScope(sp *spec, w where, u scopeUnit, members []scopeMember) (verdict, string) {
	for _, m := range members {
		if m.known {
			return ours, "the home's " + shortCmd(m.Cmdline) + " (pid " + strconv.Itoa(m.PID) + ") runs in it"
		}
	}
	v, why := unknown, ""
	note := func(mv verdict, mwhy string) {
		switch {
		case mv == theirs:
			v, why = theirs, mwhy
		case mv == ours && v == unknown:
			v, why = ours, mwhy
		}
	}
	if u.desc != "" && sp.explicit != nil {
		mv, mwhy := sp.explicit(w, Process{Cmdline: u.desc}, nil)
		note(mv, "the command it started: "+mwhy)
	}
	for _, m := range members {
		mv, mwhy := unknown, ""
		if sp.explicit != nil {
			mv, mwhy = sp.explicit(w, m.Process, m.info.Env)
		}
		if mv == unknown && sp.implicit != nil {
			mv, mwhy = sp.implicit(w, m.Process, m.info.Env)
		}
		note(mv, "pid "+strconv.Itoa(m.PID)+": "+mwhy)
	}
	return v, why
}
