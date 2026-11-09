package migrate

import (
	"errors"
	"os"
	"regexp"
	"strings"
)

// readDotEnv reads KEY=VALUE lines (with optional "export " and quotes);
// a missing file is empty.
func readDotEnv(path string) (map[string]string, error) {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		} else if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i]) // an unquoted value's trailing comment
		}
		if envName.MatchString(k) {
			out[k] = v
		}
	}
	return out, nil
}

var (
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	reRef   = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-[^}]*)?\}`)
	reBare  = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)$`)
)

// varRefs lists the ${VAR} names a string refers to.
func varRefs(s string) []string {
	var out []string
	for _, m := range reRef.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// refOnly reports whether s is nothing but a reference to one variable:
// "${VAR}", "${VAR:-x}" or "$VAR".
func refOnly(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if m := reRef.FindStringSubmatch(s); m != nil && m[0] == s {
		return m[1], true
	}
	if m := reBare.FindStringSubmatch(s); m != nil {
		return m[1], true
	}
	return "", false
}

// secretValue resolves a config value that holds a key or token: a literal,
// a ${VAR} reference or a {source: env, id: VAR} object. It returns the
// value it found (from env when referenced) and the variable it came from
// ("" for a literal).
func secretValue(v any, env map[string]string) (value, from string) {
	switch t := v.(type) {
	case string:
		if name, ok := refOnly(t); ok {
			return env[name], name
		}
		if len(varRefs(t)) > 0 {
			return "", "" // a template: not a plain secret
		}
		return t, ""
	case map[string]any:
		if s, _ := t["source"].(string); s == "env" {
			if id, _ := t["id"].(string); id != "" {
				return env[id], id
			}
		}
	}
	return "", ""
}
