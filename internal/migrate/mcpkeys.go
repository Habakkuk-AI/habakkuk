package migrate

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
)

// MCP servers and keys. A server's command line, URL and headers can hold
// a key in more ways than any check can tell apart from ordinary values (a
// short flag's value, a UUID, a token in a path), so the import does not
// guess. A server is copied as it is only when everything in it is
// plainly not a key:
//
//   - its command is a program name or a path;
//   - each argument is a flag with no value, the package a runner starts
//     (the name after npx, uvx, bunx, pipx run and the like), or a path;
//   - its URL has no user, query or fragment, and each path segment is a
//     short plain word;
//   - each header is a variable reference: "${NAME}", or "Bearer ${NAME}"
//     (also Basic and Token) with nothing else in it.
//
// Anything else in an argument, the URL or a header is a value only .env
// may keep. It moves into a variable of its own (MCP_<NAME>_ARG<n>,
// MCP_<NAME>_URL, MCP_<NAME>_HEADER_<n>) that config.yaml names as
// ${...}, and the server is marked Keyed: the plan imports it only when the
// keys are copied (--secrets copy), and leaves it out otherwise. Literal
// values never reach config.yaml or the plan. An env value was always a
// variable; a literal one moves into .env with the other keys.

var (
	reFlag     = regexp.MustCompile(`^--?[A-Za-z0-9][A-Za-z0-9._-]*$|^--$`)
	reProgram  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	rePackage  = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*(\[[a-z0-9,_-]+\])?((@|==|>=|~=)[A-Za-z0-9._*^~+-]+)?$`)
	rePath     = regexp.MustCompile(`^(~?[/\\]|\.{1,2}[/\\]|[A-Za-z]:[/\\])[^=]*$`)
	reSegment  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,23}$`)
	reHeader   = regexp.MustCompile(`^((Bearer|Basic|Token)\s+)?\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)
	reDigitRun = regexp.MustCompile(`[0-9]`)
)

// runners start a package named by their first plain argument; the ones
// with a subcommand take it from after that word.
var (
	pkgRunners = []string{"npx", "bunx", "pnpx", "uvx"}
	subRunners = map[string][]string{"pipx": {"run"}, "pnpm": {"dlx"}, "yarn": {"dlx"}, "npm": {"exec"}, "uv": {"tool", "run"}, "bun": {"x"}}
)

// plainCommand reports a program name or a path.
func plainCommand(c string) bool {
	return reProgram.MatchString(c) || rePath.MatchString(c) && !strings.Contains(c, "://")
}

func plainPath(a string) bool { return rePath.MatchString(a) && !strings.Contains(a, "://") }

// plainArgs says which arguments are plain: flags with no value, the
// package a runner starts, and paths.
func plainArgs(command string, args []string) []bool {
	base := strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(filepath.Base(command), ".exe"), ".cmd"))
	wantPkg := slices.Contains(pkgRunners, base)
	subs := subRunners[base]
	out := make([]bool, len(args))
	pkgSeen := false
	for i, a := range args {
		switch {
		case reFlag.MatchString(a):
			out[i] = true
		case !pkgSeen && !wantPkg && slices.Contains(subs, a):
			out[i] = true
			if a != "tool" {
				wantPkg = true
			}
		case wantPkg && !pkgSeen && rePackage.MatchString(a) && len(a) < 80 && memory.CheckSecrets(a) == nil:
			out[i], pkgSeen = true, true
		case plainPath(a):
			out[i] = true
		}
	}
	return out
}

// splitURL parts a URL into what may stay in config.yaml and what may
// not: "" rest when all of it is plain.
func splitURL(raw string) (keep, rest string) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", raw
	}
	if u.User != nil {
		return u.Scheme + "://", strings.TrimPrefix(raw, u.Scheme+"://")
	}
	keep = u.Scheme + "://" + u.Host
	plain := u.RawQuery == "" && u.Fragment == "" && !strings.HasSuffix(raw, "?") && !strings.HasSuffix(raw, "#")
	for _, seg := range strings.Split(strings.Trim(u.EscapedPath(), "/"), "/") {
		if seg == "" {
			continue
		}
		if !reSegment.MatchString(seg) || len(reDigitRun.FindAllString(seg, -1)) > 4 {
			plain = false
		}
	}
	if plain {
		return raw, ""
	}
	return keep, strings.TrimPrefix(raw, keep)
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
	if s.Command != "" && !plainCommand(s.Command) {
		f.skip("MCP server %s: its command is not a plain program or path, so it is not imported; add it to mcp_servers in config.yaml yourself, keeping any key in $HABAKKUK_HOME/.env (or hk mcp add %s if it is in the catalog)", raw, name)
		return nil
	}
	prefix := "MCP_" + envSuffix(name) + "_"
	keyed := func(variable, value string) string {
		s.Keyed = append(s.Keyed, variable)
		f.secret(variable, value)
		return "${" + variable + "}"
	}
	for i, plain := range plainArgs(s.Command, s.Args) {
		if !plain {
			s.Args[i] = keyed(fmt.Sprintf("%sARG%d", prefix, i+1), s.Args[i])
		}
	}
	if s.URL != "" {
		if keep, rest := splitURL(s.URL); rest != "" {
			s.URL = keep + keyed(prefix+"URL", rest)
		}
	}
	if auth, _ := m["auth"].(string); strings.EqualFold(auth, "oauth") && s.URL != "" {
		s.OAuth = true
		f.leftover("the sign-in to MCP server "+name, "hk mcp login "+name)
	}
	refs := func(v string) {
		for _, ref := range varRefs(v) {
			used[ref] = true
			f.secret(ref, env[ref])
		}
	}
	if envs, _ := m["env"].(map[string]any); s.Command != "" && len(envs) > 0 {
		s.Env = map[string]string{}
		for _, k := range sortedKeys(envs) {
			v := fmt.Sprint(envs[k])
			if ref, ok := refOnly(v); ok {
				s.Env[k] = ref
				used[ref] = true
				f.secret(ref, env[ref])
				continue
			}
			// A literal (or a template): its value moves into a variable.
			v2 := prefix + envSuffix(k)
			if envName.MatchString(k) && strings.ToUpper(k) == k {
				v2 = k
			}
			if prev, ok := f.Secrets[v2]; ok && prev != v {
				v2 = prefix + envSuffix(k)
			}
			refs(v)
			f.secret(v2, expandFrom(v, env))
			s.Env[k] = v2
		}
	}
	if hdrs, _ := m["headers"].(map[string]any); s.URL != "" && len(hdrs) > 0 {
		s.Headers = map[string]string{}
		for n, k := range sortedKeys(hdrs) {
			v := fmt.Sprint(hdrs[k])
			if ref, ok := refOnly(v); ok && strings.HasPrefix(strings.TrimSpace(v), "${") {
				s.Headers[k] = ref
				used[ref] = true
				f.secret(ref, env[ref])
				continue
			}
			if reHeader.MatchString(strings.TrimSpace(v)) {
				s.Headers[k] = strings.TrimSpace(v)
				refs(v)
				continue
			}
			refs(v)
			variable := fmt.Sprintf("%sHEADER_%d", prefix, n+1)
			s.Keyed = append(s.Keyed, variable)
			f.secret(variable, expandFrom(v, env))
			s.Headers[k] = variable
		}
	}
	return s
}

// expandFrom fills the ${NAME} references in a value from the source's
// variables, keeping any it has no value for as they are.
func expandFrom(v string, env map[string]string) string {
	return reRef.ReplaceAllStringFunc(v, func(m string) string {
		if sub := reRef.FindStringSubmatch(m); sub != nil && env[sub[1]] != "" {
			return env[sub[1]]
		}
		return m
	})
}
