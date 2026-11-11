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
// says why. A positive sign wins over a negative one: a path into the
// home is stronger than an environment the process may have changed.
func proof(sp *spec, w where, p Process, info ProcInfo, units []string) (verdict, string) {
	for _, a := range p.Args() {
		if path := argPath(a, w.sys.UserHome()); path != "" && under(path, w.dir) {
			return ours, "its command line names " + path
		}
	}
	if info.Cwd != "" && under(info.Cwd, w.dir) {
		return ours, "it runs in " + info.Cwd
	}
	for _, u := range units {
		if info.Cgroup != "" && strings.Contains(info.Cgroup, "/"+u) {
			return ours, "it runs under " + u
		}
	}
	if sp.home != nil {
		return sp.home(w, p, info.Env)
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
