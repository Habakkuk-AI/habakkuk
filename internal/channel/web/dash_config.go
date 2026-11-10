package web

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// Redacted replaces every secret in the config view.
const Redacted = "[redacted]"

// configView shows the effective config with secrets removed, and the
// doctor's checks.
func (s *Server) configView(w http.ResponseWriter, r *http.Request) {
	d := s.dash(w)
	if d == nil {
		return
	}
	if d.Config == nil {
		notAvailable(w, "the config view")
		return
	}
	tree, err := RedactConfig(d.Config, d.Secret)
	if err != nil {
		s.fail(w, "render the config", err)
		return
	}
	s.liveDisplay(tree) // progress and approvals_detail may have changed since serve started
	out := map[string]any{"config": tree, "home": d.Config.Home}
	if d.Doctor != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		checks := d.Doctor(ctx)
		for i := range checks {
			checks[i].Detail = scrubString(checks[i].Detail, secretValues(tree, d.Secret))
		}
		out["doctor"] = nonNil(checks)
	}
	writeJSON(w, http.StatusOK, out)
}

// RedactConfig renders cfg as a JSON-ready tree with secrets removed. The
// config mostly names variables rather than holding secrets, but values can
// still leak through other fields: a URL with a password, an MCP server's
// arguments, a literal header, a hook's command. So, in this order:
//
//   - the value of every variable the config names (fields ending in _env,
//     MCP env maps, ${VAR} in headers) is replaced wherever it appears;
//   - string fields whose key reads like a secret (token, password, key,
//     ...) but does not name a variable are replaced whole, as are header
//     values that are not variable references;
//   - credentials in URLs (user info, secret-looking query parameters) and
//     strings shaped like well-known API keys or private keys are replaced.
func RedactConfig(cfg *config.Config, secret func(string) string) (any, error) {
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var tree any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return nil, err
	}
	tree = plain(tree)
	return scrub(tree, "", secretValues(tree, secret)), nil
}

// plain turns yaml's maps into map[string]any so the tree encodes as JSON.
func plain(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = plain(x)
		}
		return t
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[fmt.Sprint(k)] = plain(x)
		}
		return m
	case []any:
		for i, x := range t {
			t[i] = plain(x)
		}
		return t
	}
	return v
}

var (
	varRef     = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	varName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretKey  = regexp.MustCompile(`(?i)(secret|token|passw|api_?key|private|credential|cookie|signature|bearer)`)
	secretLike = []*regexp.Regexp{
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$)`),
		regexp.MustCompile(`\b(sk|pk|rk)-[A-Za-z0-9_-]{16,}`),           // OpenAI, Anthropic and similar
		regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`),           // Slack bot and user tokens
		regexp.MustCompile(`\bxapp-[A-Za-z0-9-]{10,}`),                  // Slack app tokens
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),              // GitHub
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),            // GitHub fine-grained
		regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{16,}`),                // GitLab
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),                      // AWS access key id
		regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}`),                  // Google API key
		regexp.MustCompile(`\b\d{6,}:[A-Za-z0-9_-]{30,}`),               // Telegram bot token
		regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{12,}`),    // a literal bearer token
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), // a JWT
	}
)

// secretValues resolves every variable the config names. Values shorter
// than six characters are left alone: replacing "1" or "us" everywhere
// would only garble the view.
func secretValues(tree any, secret func(string) string) []string {
	if secret == nil {
		return nil
	}
	names := map[string]bool{}
	collectVars(tree, "", names)
	var out []string
	for n := range names {
		if v := strings.TrimSpace(secret(n)); len(v) >= 6 {
			out = append(out, v)
		}
	}
	// Longest first, so a value that contains another is replaced whole.
	slices.SortFunc(out, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	return slices.Compact(out)
}

func collectVars(v any, key string, names map[string]bool) {
	lk := strings.ToLower(key)
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			switch lk {
			case "env": // an MCP server's env: child variable -> our variable
				if s, ok := x.(string); ok {
					names[s] = true
				}
			case "headers":
				if s, ok := x.(string); ok {
					headerVars(s, names)
				}
			}
			collectVars(x, k, names)
		}
	case []any:
		for _, x := range t {
			collectVars(x, key, names)
		}
	case string:
		if strings.HasSuffix(lk, "_env") && varName.MatchString(t) {
			names[t] = true
		}
	}
}

func headerVars(spec string, names map[string]bool) {
	if varName.MatchString(spec) {
		names[spec] = true
		return
	}
	for _, m := range varRef.FindAllStringSubmatch(spec, -1) {
		names[m[1]] = true
	}
}

func scrub(v any, key string, values []string) any {
	lk := strings.ToLower(key)
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if s, ok := x.(string); ok && s != "" {
				switch {
				case lk == "headers" && !varName.MatchString(s) && !varRef.MatchString(s):
					t[k] = Redacted // a literal header value
					continue
				case lk == "headers" && varRef.MatchString(s):
					// "Bearer ${VAR}": keep the template, drop anything literal
					// beside the reference that looks like a token.
					t[k] = scrubString(s, values)
					continue
				case lk == "env":
					// An MCP server's env: the values name our variables.
				case secretKey.MatchString(k) && !strings.HasSuffix(strings.ToLower(k), "_env"):
					t[k] = Redacted
					continue
				}
			}
			t[k] = scrub(x, k, values)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = scrub(x, key, values)
		}
		return t
	case string:
		return scrubString(t, values)
	}
	return v
}

func scrubString(s string, values []string) string {
	for _, v := range values {
		s = strings.ReplaceAll(s, v, Redacted)
	}
	for _, re := range secretLike {
		s = re.ReplaceAllString(s, Redacted)
	}
	if strings.Contains(s, "://") {
		s = scrubURLs(s)
	}
	return s
}

var urlish = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)

// scrubURLs drops passwords from URLs' user info and the values of query
// parameters whose names read like secrets.
func scrubURLs(s string) string {
	return urlish.ReplaceAllStringFunc(s, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return raw
		}
		changed := false
		if u.User != nil {
			if _, has := u.User.Password(); has {
				u.User = url.UserPassword(u.User.Username(), "redacted")
			} else {
				u.User = url.User("redacted")
			}
			changed = true
		}
		if u.RawQuery != "" {
			q := u.Query()
			for k := range q {
				if secretKey.MatchString(k) || strings.EqualFold(k, "key") || strings.EqualFold(k, "sig") {
					q.Set(k, "redacted")
					changed = true
				}
			}
			if changed {
				u.RawQuery = q.Encode()
			}
		}
		if !changed {
			return raw
		}
		return u.String()
	})
}
