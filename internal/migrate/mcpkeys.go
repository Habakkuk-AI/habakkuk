package migrate

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// MCP servers and keys. A server's command line, URL and headers can hold
// a key in more ways than any check can tell apart from ordinary values, so
// the import does not guess. A remote server (one with a URL) is never
// copied as it is. A local server is copied as it is only when:
//
//   - its command is a bare program name, or a plain path;
//   - each argument is the package a runner starts (the first word after
//     npx, uvx, bunx, pipx run and the like), a flag such as "-y" or
//     "--stdio" with nothing attached, a plain path, or a variable
//     reference (${NAME}, or --flag=${NAME}).
//
// A plain path is absolute (or starts with "~/"), has no spaces, "@" or
// ":", and either exists or lies in the source home (a file the old agent
// kept, which the files copy may bring along).
//
// A variable reference stays a reference: config.yaml keeps ${NAME}, and
// the variable's value moves the way keys do (a ${NAME:-default} keeps the
// default as the value when the source has none). Anything else is a value
// only .env may keep. It moves into a variable of its own
// (MCP_<NAME>_COMMAND, MCP_<NAME>_ARG<n>, MCP_<NAME>_URL for the whole
// address, MCP_<NAME>_HEADER_<n>) that config.yaml names as ${...}, and the
// server is marked Keyed: the plan imports it only when the keys are copied
// (--secrets copy), and leaves it out otherwise. A header that is a
// variable reference ("${NAME}", or "Bearer ${NAME}" and the like) stays a
// reference. An env value was always a variable; a literal one moves into
// .env with the other keys.

var (
	reFlag     = regexp.MustCompile(`^--?[a-z][a-z0-9-]*$`)
	reProgram  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	rePackage  = regexp.MustCompile(`^@?[a-z0-9][a-z0-9._-]*(/[a-z0-9._-]+)?(@[A-Za-z0-9._-]+)?$`)
	reArgRef   = regexp.MustCompile(`^(--?[a-z][a-z0-9-]*=)?\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}$`)
	reHeader   = regexp.MustCompile(`^((Bearer|Basic|Token)\s+)?\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}$`)
	pkgRunners = []string{"npx", "bunx", "pnpx", "uvx"}
	subRunners = map[string][]string{"pipx": {"run"}, "pnpm": {"dlx"}, "yarn": {"dlx"}, "npm": {"exec"}, "uv": {"tool", "run"}, "bun": {"x"}}
)

// plainPath reports a plain path (see above).
func (f *Found) plainPath(p string) bool {
	if strings.ContainsAny(p, " \t@:") {
		return false
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok && f.userHome != "" {
		p = filepath.Join(f.userHome, rest)
	}
	if !filepath.IsAbs(p) || filepath.Clean(p) != strings.TrimSuffix(p, "/") && filepath.Clean(p) != p {
		return false
	}
	return exists(p) || f.Dir != "" && under(f.Dir, p)
}

// plainCommand reports a bare program name or a plain path.
func (f *Found) plainCommand(c string) bool { return reProgram.MatchString(c) || f.plainPath(c) }

// Kinds of argument.
const (
	argPlain = iota
	argRef   // a variable reference: kept as a reference
	argKeyed // anything else: moves into a variable
)

// argKinds sorts a local server's arguments.
func (f *Found) argKinds(command string, args []string) []int {
	base := strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(filepath.Base(command), ".exe"), ".cmd"))
	wantPkg := slices.Contains(pkgRunners, base)
	subs := subRunners[base]
	out := make([]int, len(args))
	pkgSeen := false
	for i, a := range args {
		switch {
		case reArgRef.MatchString(a):
			out[i] = argRef
		case reFlag.MatchString(a):
			out[i] = argPlain
		case !pkgSeen && !wantPkg && slices.Contains(subs, a):
			out[i] = argPlain
			if a != "tool" {
				wantPkg = true
			}
		case wantPkg && !pkgSeen && rePackage.MatchString(a):
			out[i], pkgSeen = argPlain, true
		case f.plainPath(a):
			out[i] = argPlain
		default:
			out[i] = argKeyed
		}
	}
	return out
}

// readMCP turns one server definition (the same shape in every source:
// command/args/env or url/headers) into a Habakkuk server. Values that may
// be keys move into variables (see above).
func (f *Found) readMCP(raw string, m map[string]any, env map[string]string, used map[string]bool) *MCPServer {
	name := cleanName(raw, 32)
	if name == "" {
		f.skip("MCP server %q: its name cannot be used", raw)
		return nil
	}
	s := &MCPServer{Name: name}
	s.Command, _ = m["command"].(string)
	s.URL, _ = m["url"].(string)
	if s.URL == "" {
		s.URL, _ = m["baseUrl"].(string)
	}
	s.Args = asList(m["args"])
	if (s.Command == "") == (s.URL == "") {
		f.skip("MCP server %s: needs exactly one of command or url", raw)
		return nil
	}
	if typ, _ := m["type"].(string); strings.EqualFold(typ, "sse") || strings.EqualFold(fmt.Sprint(m["transport"]), "sse") {
		f.skip("MCP server %s: it uses the older SSE transport, which Habakkuk does not speak; if the server also offers Streamable HTTP (often at /mcp), add that URL to mcp_servers in config.yaml", raw)
		return nil
	}
	prefix := "MCP_" + envSuffix(name) + "_"
	keyed := func(variable, value string) string {
		s.Keyed = append(s.Keyed, variable)
		f.secret(variable, expandFrom(value, env))
		return "${" + variable + "}"
	}
	// reference keeps a variable reference, and moves its value as keys
	// move: the source's value, else the reference's default.
	reference := func(variable, def string) {
		v := env[variable]
		if v == "" {
			v = def
		}
		used[variable] = true
		f.secret(variable, v)
	}
	if s.Command != "" {
		if !f.plainCommand(s.Command) {
			s.Command = keyed(prefix+"COMMAND", s.Command)
		}
		for i, kind := range f.argKinds(s.Command, s.Args) {
			switch kind {
			case argRef:
				sub := reArgRef.FindStringSubmatch(s.Args[i])
				reference(sub[2], sub[4])
				s.Args[i] = sub[1] + "${" + sub[2] + "}"
			case argKeyed:
				s.Args[i] = keyed(fmt.Sprintf("%sARG%d", prefix, i+1), s.Args[i])
			}
		}
	} else {
		// A remote server's address is kept whole in .env: a key can be
		// anywhere in it, the host name included.
		s.URL = keyed(prefix+"URL", s.URL)
	}
	if auth, _ := m["auth"].(string); strings.EqualFold(auth, "oauth") && s.URL != "" {
		s.OAuth = true
	}
	if envs, _ := m["env"].(map[string]any); s.Command != "" && len(envs) > 0 {
		s.Env = map[string]string{}
		for _, k := range sortedKeys(envs) {
			v := fmt.Sprint(envs[k])
			if sub := reArgRef.FindStringSubmatch(strings.TrimSpace(v)); sub != nil && sub[1] == "" {
				reference(sub[2], sub[4])
				s.Env[k] = sub[2]
				continue
			}
			if ref, ok := refOnly(v); ok {
				reference(ref, "")
				s.Env[k] = ref
				continue
			}
			// A literal (or a template): its value moves into a variable.
			v2 := prefix + envSuffix(k)
			if envName.MatchString(k) && strings.ToUpper(k) == k && !slices.Contains(systemVars, k) {
				v2 = k
			}
			if prev, ok := f.Secrets[v2]; ok && prev != v {
				v2 = prefix + envSuffix(k)
			}
			for _, r := range varRefs(v) {
				used[r] = true
			}
			f.secret(v2, expandFrom(v, env))
			s.Env[k] = v2
		}
	}
	if hdrs, _ := m["headers"].(map[string]any); s.URL != "" && len(hdrs) > 0 {
		s.Headers = map[string]string{}
		for n, k := range sortedKeys(hdrs) {
			v := strings.TrimSpace(fmt.Sprint(hdrs[k]))
			if sub := reHeader.FindStringSubmatch(v); sub != nil {
				reference(sub[3], sub[5])
				if sub[1] == "" {
					s.Headers[k] = sub[3]
				} else {
					s.Headers[k] = sub[1] + "${" + sub[3] + "}"
				}
				continue
			}
			for _, r := range varRefs(v) {
				used[r] = true
			}
			variable := fmt.Sprintf("%sHEADER_%d", prefix, n+1)
			s.Keyed = append(s.Keyed, variable)
			f.secret(variable, expandFrom(v, env))
			s.Headers[k] = variable
		}
	}
	return s
}

// systemVars are variables every program gets from the system; the import
// never asks you to set them.
var systemVars = []string{"PATH", "HOME", "USER", "SHELL", "TMPDIR", "TEMP", "TMP", "LANG", "PWD"}

// expandFrom fills the ${NAME} references in a value from the source's
// variables, keeping any it has no value for as they are.
func expandFrom(v string, env map[string]string) string {
	return reRef.ReplaceAllStringFunc(v, func(m string) string {
		if sub := reRef.FindStringSubmatch(m); sub != nil {
			if env[sub[1]] != "" {
				return env[sub[1]]
			}
			if sub[2] != "" {
				return strings.TrimPrefix(sub[2], ":-")
			}
		}
		return m
	})
}
