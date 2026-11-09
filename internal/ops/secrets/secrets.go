// Package secrets resolves credential variables from secret managers: a
// command, the 1Password CLI (op read), the Bitwarden CLI (bw get) or a
// file. Config maps variable names to references in secrets.refs; values
// are cached in memory for the source's cache period and never logged.
package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
)

// failCache is how long a reference that could not be read is left alone,
// so a missing CLI or a locked vault is not asked again on every call.
const failCache = 30 * time.Second

// Runner runs a program and returns its stdout. Tests replace it.
type Runner func(ctx context.Context, env []string, name string, args ...string) ([]byte, error)

// Resolver reads variables named in secrets.refs. A nil Resolver resolves
// nothing.
type Resolver struct {
	cfg  *config.Config
	home string
	log  *slog.Logger
	// Run runs commands; nil uses os/exec with a 30-second timeout.
	Run Runner
	Now func() time.Time

	mu    sync.Mutex
	cache map[string]entry
	inner map[string]*sync.Mutex // one read at a time per variable
}

type entry struct {
	value string
	until time.Time
}

// New returns a resolver for cfg's secrets section, or nil when it has no
// refs.
func New(cfg *config.Config, home string, log *slog.Logger) *Resolver {
	if cfg == nil || cfg.Secrets == nil || len(cfg.Secrets.Refs) == 0 {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &Resolver{cfg: cfg, home: home, log: log, cache: map[string]entry{}, inner: map[string]*sync.Mutex{}}
}

// Has reports whether name has a reference.
func (r *Resolver) Has(name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.cfg.Secrets.Refs[name]
	return ok
}

// Names lists the variables with references.
func (r *Resolver) Names() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.cfg.Secrets.Refs))
	for k := range r.cfg.Secrets.Refs {
		out = append(out, k)
	}
	return out
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Get returns the value of name from its source, or "" when it has no
// reference or the source fails (the failure is logged without the value).
func (r *Resolver) Get(ctx context.Context, name string) string {
	if r == nil {
		return ""
	}
	ref, ok := r.cfg.Secrets.Refs[name]
	if !ok {
		return ""
	}
	r.mu.Lock()
	if e, ok := r.cache[name]; ok && r.now().Before(e.until) {
		r.mu.Unlock()
		return e.value
	}
	lock := r.inner[name]
	if lock == nil {
		lock = &sync.Mutex{}
		r.inner[name] = lock
	}
	r.mu.Unlock()

	lock.Lock()
	defer lock.Unlock()
	r.mu.Lock() // another caller may have read it while we waited
	if e, ok := r.cache[name]; ok && r.now().Before(e.until) {
		r.mu.Unlock()
		return e.value
	}
	r.mu.Unlock()

	srcName, rest, err := r.cfg.SecretRef(ref)
	var src *config.SecretSource
	if err == nil {
		src = r.cfg.Secrets.Sources[srcName]
	}
	var value string
	if err == nil {
		value, err = r.read(ctx, name, src, rest)
	}
	keep := failCache
	if err != nil {
		r.log.Warn("secret source failed", "var", name, "source", srcName, "err", err)
		value = ""
	} else {
		keep = src.Cache
	}
	r.mu.Lock()
	r.cache[name] = entry{value: value, until: r.now().Add(keep)}
	r.mu.Unlock()
	return value
}

// Forget drops cached values, so the next Get reads each source again.
func (r *Resolver) Forget() {
	if r == nil {
		return
	}
	r.mu.Lock()
	clear(r.cache)
	r.mu.Unlock()
}

func (r *Resolver) read(ctx context.Context, name string, src *config.SecretSource, ref string) (string, error) {
	var out []byte
	var err error
	switch src.Type {
	case "file":
		out, err = os.ReadFile(r.path(ref))
	case "1password":
		if !strings.HasPrefix(ref, "op://") {
			return "", errors.New("1password references start with op://")
		}
		out, err = r.run(ctx, nil, "op", "read", "--no-newline", ref)
	case "bitwarden":
		return r.bitwarden(ctx, ref)
	case "command":
		out, err = r.runShell(ctx, []string{"HABAKKUK_SECRET_REF=" + ref, "HABAKKUK_SECRET_NAME=" + name}, src.Command)

	default:
		return "", fmt.Errorf("unknown source type %q", src.Type)
	}
	if err != nil {
		return "", err
	}
	v := strings.TrimRight(string(out), "\r\n")
	if v == "" {
		return "", errors.New("empty value")
	}
	return v, nil
}

// bitwarden reads "ITEM" (the password) or "ITEM#FIELD": username,
// password, totp, notes or uri, else a custom field of that name.
func (r *Resolver) bitwarden(ctx context.Context, ref string) (string, error) {
	item, field, _ := strings.Cut(ref, "#")
	if field == "" {
		field = "password"
	}
	switch field {
	case "username", "password", "totp", "notes", "uri":
		out, err := r.run(ctx, nil, "bw", "get", field, item)
		if err != nil {
			return "", err
		}
		v := strings.TrimRight(string(out), "\r\n")
		if v == "" {
			return "", errors.New("empty value")
		}
		return v, nil
	}
	out, err := r.run(ctx, nil, "bw", "get", "item", item)
	if err != nil {
		return "", err
	}
	var it struct {
		Fields []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(out, &it); err != nil {
		return "", errors.New("bw get item: unreadable output")
	}
	for _, f := range it.Fields {
		if f.Name == field && f.Value != "" {
			return f.Value, nil
		}
	}
	return "", fmt.Errorf("item has no field %q", field)
}

func (r *Resolver) path(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, rest)
		}
	}
	if !filepath.IsAbs(p) {
		return filepath.Join(r.home, p)
	}
	return p
}

func (r *Resolver) run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	if r.Run != nil {
		return r.Run(ctx, env, name, args...)
	}
	return Exec(ctx, env, name, args...)
}

// runShell runs a command string with /bin/sh, or PowerShell on Windows.
func (r *Resolver) runShell(ctx context.Context, env []string, command string) ([]byte, error) {
	if !platform.IsWindows {
		return r.run(ctx, env, "/bin/sh", "-c", command)
	}
	name, args := platform.ShellArgv(command)
	return r.run(ctx, env, name, args...)
}

// Exec runs a program with the process environment plus env and returns
// its stdout. Errors carry the first line of stderr, never stdout.
func Exec(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if msg != "" {
			return nil, fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return stdout.Bytes(), nil
}
