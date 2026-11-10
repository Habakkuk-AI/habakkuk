package remote

import (
	"fmt"
	"slices"
)

// Select picks where a command runs: the -r flag first, then the
// HABAKKUK_REMOTE variable, then the registry default. It returns ok false
// for this machine (nothing set, or the name "local").
func Select(flag, env string, reg *Registry) (name string, rem Remote, ok bool, err error) {
	var from string
	switch {
	case flag != "":
		name, from = flag, "-r"
	case env != "":
		name, from = env, "HABAKKUK_REMOTE"
	case reg != nil && reg.Default != "":
		name, from = reg.Default, "the default remote"
	default:
		return "", Remote{}, false, nil
	}
	if name == Local {
		return "", Remote{}, false, nil
	}
	if reg != nil {
		if rem, ok = reg.Remotes[name]; ok {
			return name, rem, true, nil
		}
	}
	return "", Remote{}, false, fmt.Errorf("unknown remote %q (from %s); hk remote list shows them", name, from)
}

// Risky says why a command should not run on a remote without
// --yes-remote: it would change the server's install or start a second
// server. It returns "" for everything else, including the read-only forms
// (update -check, migrate -dry-run, config migrate -dry-run).
func Risky(args []string) string {
	if len(args) == 0 {
		return ""
	}
	rest := args[1:]
	switch args[0] {
	case "serve":
		return "it starts a second server in this ssh session, beside the one the service manager runs"
	case "init":
		return "it writes a starter config into the server's home"
	case "restore":
		return "it unpacks a backup over the server's home"
	case "update":
		if hasFlag(rest, "check") {
			return ""
		}
		return "it replaces the server's habakkuk binary"
	case "migrate":
		if hasFlag(rest, "dry-run") {
			return ""
		}
		return "it imports another agent's data into the server's home"
	case "setup":
		if len(rest) > 0 && rest[0] == "service" {
			return "it installs a service on the server"
		}
	case "config":
		if len(rest) > 0 && rest[0] == "migrate" && !hasFlag(rest[1:], "dry-run") {
			return "it rewrites the server's config.yaml"
		}
	}
	return ""
}

func hasFlag(args []string, name string) bool {
	return slices.ContainsFunc(args, func(a string) bool {
		for _, p := range []string{"-", "--"} {
			if a == p+name || a == p+name+"=true" {
				return true
			}
		}
		return false
	})
}
