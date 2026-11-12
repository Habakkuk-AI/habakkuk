package cutover

import (
	"path/filepath"
	"strings"
)

// The cutover stops a process only when it can show the process belongs
// to the old agent in the home being moved: being the user's own is not
// enough, and neither is a command line that looks like the agent's. A
// process it cannot tie to the home is listed as one that may be the old
// agent's, for you to stop.

// verdict is what one piece of evidence says about a process and a home.
type verdict int

const (
	unknown verdict = iota
	ours
	theirs // another home's
)

// proof decides whether a process is the old agent's in this home, and
// says why. In order:
//
//  1. what the process says about its home itself: its home variable
//     (HERMES_HOME, OPENCLAW_STATE_DIR), a profile flag, a session folder.
//     This decides, either way: every home's processes may run the same
//     installed program, so the program's path proves nothing against it.
//  2. a data path into the home on its command line (not its program);
//  3. its working folder in the home;
//  4. the service or worker scope it runs under;
//  5. an environment that could be read and names no home: the default
//     home.
//
// When none of these decides, a program installed inside the home only
// makes it one that may be the old agent's.
func proof(sp *spec, w where, p Process, info ProcInfo, units []string) (verdict, string) {
	if sp.explicit != nil {
		if v, why := sp.explicit(w, p, info.Env); v != unknown {
			return v, why
		}
	}
	uh := w.sys.UserHome()
	exe := map[string]bool{}
	if sp.programs != nil {
		for _, a := range sp.programs(p) {
			exe[a] = true
		}
	}
	for _, a := range p.Args() {
		if exe[a] {
			continue
		}
		if path := argPath(a, uh); path != "" && under(path, w.dir) {
			return ours, "its command line names " + path
		}
	}
	if info.Cwd != "" && under(info.Cwd, w.dir) {
		return ours, "it runs in " + info.Cwd
	}
	if info.Cgroup != "" {
		for _, u := range units {
			if strings.Contains(info.Cgroup, "/"+u) {
				return ours, "it runs under " + u
			}
		}
		if sp.scopePrefix != "" && strings.Contains(info.Cgroup, "/"+sp.scopePrefix) {
			return ours, "it runs in one of its worker scopes"
		}
	}
	if sp.implicit != nil {
		if v, why := sp.implicit(w, p, info.Env); v != unknown {
			return v, why
		}
	}
	for a := range exe {
		if path := argPath(a, uh); path != "" && under(path, w.dir) {
			return unknown, "its program is in " + w.dir + ", but which home it uses could not be read"
		}
	}
	return unknown, ""
}

// argPath is the absolute path an argument names, if any: the argument
// itself, the value of --flag=PATH, with ~/ expanded.
func argPath(a, userHome string) string {
	a = strings.Trim(a, `"'`)
	if _, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(a, "-") {
		a = strings.Trim(v, `"'`)
	}
	if rest, ok := strings.CutPrefix(a, "~/"); ok && userHome != "" {
		a = filepath.Join(userHome, rest)
	}
	if filepath.IsAbs(a) || (len(a) > 2 && a[1] == ':' && (a[2] == '\\' || a[2] == '/')) {
		return a
	}
	return ""
}

// isDefaultHome reports whether dir is the source's default home.
func isDefaultHome(dir, def string) bool { return under(dir, def) && under(def, dir) }

func expandTilde(p, userHome string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return filepath.Join(userHome, rest)
	}
	if p == "~" {
		return userHome
	}
	return p
}
