package config

import (
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Metrics configures GET /metrics on the HTTP surface (Prometheus text
// format). It needs the API token unless Public is set.
type Metrics struct {
	Public bool `yaml:"public"`
}

// Telemetry exports traces over OTLP/HTTP (JSON): one span per turn, with
// child spans for its model and tool calls. Off unless OTLPEndpoint is set.
type Telemetry struct {
	// OTLPEndpoint is the collector's base URL (http://localhost:4318);
	// spans go to <endpoint>/v1/traces unless the URL already ends there.
	OTLPEndpoint string `yaml:"otlp_endpoint"`
	// HeadersEnv names a variable holding extra request headers in the
	// OTEL_EXPORTER_OTLP_HEADERS form: "key=value,key2=value2".
	HeadersEnv string `yaml:"headers_env"`
	// IncludeContent adds message text, tool inputs and tool results to
	// spans. Off by default: spans carry names, ids, sizes and outcomes only.
	IncludeContent bool   `yaml:"include_content"`
	ServiceName    string `yaml:"service_name"` // default habakkuk
}

// Logging configures $HABAKKUK_HOME/habakkuk.log.
type Logging struct {
	MaxMB   int `yaml:"max_mb"`  // rotate past this size (default 20)
	Backups int `yaml:"backups"` // rotated files kept as habakkuk.log.1 ... (default 5)
}

// ToolSafety holds settings for the built-in tools.
type ToolSafety struct {
	// AllowPrivateURLs lets fetch_url reach loopback, private, link-local
	// and cloud metadata addresses. Off by default (SSRF guard).
	AllowPrivateURLs bool `yaml:"allow_private_urls"`
}

// Secrets resolves credential variables from secret managers. Refs maps a
// variable name (anything a *_env setting names) to a reference
// "<source>:<reference>"; references starting op:// go to the 1password
// source. The process environment still wins; .env is the fallback when a
// reference cannot be read.
type Secrets struct {
	Sources map[string]*SecretSource `yaml:"sources"`
	Refs    map[string]string        `yaml:"refs"`
}

// SecretSource is one place secrets come from.
type SecretSource struct {
	// Type is command, 1password, bitwarden or file; default the source's
	// name when that is one of these.
	Type string `yaml:"type"`
	// Command (type command) runs with /bin/sh -c; the reference is in
	// $HABAKKUK_SECRET_REF and the variable name in $HABAKKUK_SECRET_NAME.
	// Its stdout, without the trailing newline, is the value.
	Command string `yaml:"command"`
	// Cache keeps a value this long before reading it again (default 5m).
	Cache time.Duration `yaml:"cache"`
}

// SecretSourceTypes are the kinds of secret source.
var SecretSourceTypes = []string{"command", "1password", "bitwarden", "file"}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (c *Config) applyOpsDefaults() {
	if c.Logging.MaxMB == 0 {
		c.Logging.MaxMB = 20
	}
	if c.Logging.Backups == 0 {
		c.Logging.Backups = 5
	}
	if t := c.Telemetry; t != nil && t.ServiceName == "" {
		t.ServiceName = "habakkuk"
	}
	if s := c.Secrets; s != nil {
		for name, src := range s.Sources {
			if src == nil {
				src = &SecretSource{}
				s.Sources[name] = src
			}
			if src.Type == "" && slices.Contains(SecretSourceTypes, name) {
				src.Type = name
			}
			if src.Cache == 0 {
				src.Cache = 5 * time.Minute
			}
		}
	}
}

func (c *Config) validateOps() []error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if c.Logging.MaxMB < 0 || c.Logging.Backups < 0 {
		bad("logging: max_mb and backups must not be negative")
	}
	if t := c.Telemetry; t != nil && t.OTLPEndpoint != "" {
		if u, err := url.Parse(t.OTLPEndpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			bad("telemetry: otlp_endpoint %q must be an http:// or https:// URL", t.OTLPEndpoint)
		}
	}
	if s := c.Secrets; s != nil {
		for name, src := range s.Sources {
			switch src.Type {
			case "command":
				if strings.TrimSpace(src.Command) == "" {
					bad("secrets: source %s: command is required", name)
				}
			case "1password", "bitwarden", "file":
				if src.Command != "" {
					bad("secrets: source %s: command applies to type command only", name)
				}
			default:
				bad("secrets: source %s: type %q must be one of %s", name, src.Type, strings.Join(SecretSourceTypes, ", "))
			}
			if src.Cache < 0 {
				bad("secrets: source %s: cache must not be negative", name)
			}
		}
		for env, ref := range s.Refs {
			if !envName.MatchString(env) {
				bad("secrets: ref %q is not a variable name", env)
			}
			if _, _, err := c.SecretRef(ref); err != nil {
				bad("secrets: ref %s: %v", env, err)
			}
		}
	}
	return errs
}

// SecretRef splits a reference into its source's name and the part the
// source reads.
func (c *Config) SecretRef(ref string) (source, rest string, err error) {
	var srcs map[string]*SecretSource
	if c.Secrets != nil {
		srcs = c.Secrets.Sources
	}
	if strings.HasPrefix(ref, "op://") {
		for _, name := range sortedKeys(srcs) {
			if srcs[name].Type == "1password" {
				return name, ref, nil
			}
		}
		return "", "", fmt.Errorf("%q needs a source of type 1password", ref)
	}
	name, rest, ok := strings.Cut(ref, ":")
	if !ok || rest == "" {
		return "", "", fmt.Errorf("%q: want <source>:<reference>", ref)
	}
	if srcs[name] == nil {
		return "", "", fmt.Errorf("%q names no source in secrets.sources", ref)
	}
	return name, rest, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// SecretEnvNames lists every variable the config names as holding a secret:
// each *_env setting anywhere in the config, the variables MCP servers read
// for env and headers, and the names in secrets.refs. Sorted, no repeats.
func (c *Config) SecretEnvNames() []string {
	seen := map[string]bool{}
	add := func(s string) {
		if envName.MatchString(s) {
			seen[s] = true
		}
	}
	collectEnv(reflect.ValueOf(c), "", add)
	for _, m := range c.MCP {
		if m == nil {
			continue
		}
		for _, v := range m.Env {
			add(v)
		}
		for _, v := range m.Headers {
			for _, name := range headerVars(v) {
				add(name)
			}
		}
	}
	if c.A2A != nil {
		for _, p := range c.A2A.Peers {
			if p == nil {
				continue
			}
			for _, v := range p.Headers {
				for _, name := range headerVars(v) {
					add(name)
				}
			}
		}
	}
	if c.Secrets != nil {
		for name := range c.Secrets.Refs {
			add(name)
		}
	}
	return sortedKeys(seen)
}

var headerVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// headerVars reads an MCP header spec: a variable name, or a template
// holding ${VAR} references.
func headerVars(spec string) []string {
	if envName.MatchString(spec) {
		return []string{spec}
	}
	var out []string
	for _, m := range headerVar.FindAllStringSubmatch(spec, -1) {
		out = append(out, m[1])
	}
	return out
}

// collectEnv walks the config and reports the values of string and
// []string fields whose YAML key ends in _env (token_env, keys_env, ...).
func collectEnv(v reflect.Value, key string, add func(string)) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			collectEnv(v.Elem(), key, add)
		}
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if name == "-" {
				continue
			}
			collectEnv(v.Field(i), name, add)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			collectEnv(v.MapIndex(k), key, add)
		}
	case reflect.Slice:
		for i := range v.Len() {
			collectEnv(v.Index(i), key, add)
		}
	case reflect.String:
		if strings.HasSuffix(key, "_env") {
			add(v.String())
		}
	}
}
