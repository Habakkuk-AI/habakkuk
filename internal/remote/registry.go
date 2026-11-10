// Package remote lets the CLI work on a Habakkuk that runs on another
// machine. It keeps a small registry of named remotes in the user's config
// directory (never in HABAKKUK_HOME), runs any command on an SSH remote with
// the system ssh client, and runs the few commands the HTTP API covers on an
// HTTPS remote.
package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// FileName is the registry's name inside the user's config directory.
const FileName = "remotes.json"

// Local is the remote name that means "this machine". It overrides a
// registry default or HABAKKUK_REMOTE: hk -r local chat.
const Local = "local"

// None clears the default: hk remote default none.
const None = "none"

var (
	nameRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Remote is one entry in the registry. It holds no secrets: an SSH identity
// is a path to a key file, and an HTTP token is read from the variable
// TokenEnv names.
type Remote struct {
	Target   string `json:"target"`              // ssh://user@host[:port] or https://host[:port][/path]
	Identity string `json:"identity,omitempty"`  // ssh: private key file (ssh -i)
	Home     string `json:"home,omitempty"`      // ssh: HABAKKUK_HOME on the server
	Bin      string `json:"bin,omitempty"`       // ssh: the habakkuk binary on the server
	TokenEnv string `json:"token_env,omitempty"` // http: variable holding the bearer token
}

// Registry is the content of remotes.json.
type Registry struct {
	Default string            `json:"default,omitempty"`
	Remotes map[string]Remote `json:"remotes"`
}

// DefaultPath is os.UserConfigDir()/habakkuk/remotes.json.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find the config directory: %w", err)
	}
	return filepath.Join(dir, "habakkuk", FileName), nil
}

// Load reads the registry at path. A missing file is an empty registry.
func Load(path string) (*Registry, error) {
	reg := &Registry{Remotes: map[string]Remote{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return reg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, reg); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if reg.Remotes == nil {
		reg.Remotes = map[string]Remote{}
	}
	return reg, nil
}

// Save writes the registry to path with mode 0600 (its directory 0700),
// replacing the old file in one rename.
func (r *Registry) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".remotes-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op after the rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Names lists the remotes in order.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.Remotes))
	for n := range r.Remotes {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Add stores rem under name. An existing name is an error unless replace.
func (r *Registry) Add(name string, rem Remote, replace bool) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	norm, err := rem.normalize()
	if err != nil {
		return err
	}
	if _, ok := r.Remotes[name]; ok && !replace {
		return fmt.Errorf("remote %q exists; remove it first or add -force", name)
	}
	r.Remotes[name] = norm
	return nil
}

// Remove deletes a remote, and the default when it pointed there.
func (r *Registry) Remove(name string) error {
	if _, ok := r.Remotes[name]; !ok {
		return fmt.Errorf("no remote %q", name)
	}
	delete(r.Remotes, name)
	if r.Default == name {
		r.Default = ""
	}
	return nil
}

// SetDefault makes name the default remote; "none" or "" clears it.
func (r *Registry) SetDefault(name string) error {
	if name == "" || name == None || name == Local {
		r.Default = ""
		return nil
	}
	if _, ok := r.Remotes[name]; !ok {
		return fmt.Errorf("no remote %q", name)
	}
	r.Default = name
	return nil
}

// ValidateName checks a remote name: lower-case letters, digits, '-' and
// '_', at most 32, and not one of the reserved words.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("remote name %q: use 1-32 lower-case letters, digits, '-' or '_'", name)
	}
	if name == Local || name == None {
		return fmt.Errorf("remote name %q is reserved", name)
	}
	return nil
}

// normalize validates rem and returns it with its target in canonical form.
func (rem Remote) normalize() (Remote, error) {
	t, err := ParseTarget(rem.Target)
	if err != nil {
		return rem, err
	}
	rem.Target = t.String()
	switch t.Kind {
	case SSH:
		if rem.TokenEnv != "" {
			return rem, errors.New("-token-env is for https remotes; an ssh remote signs in with ssh")
		}
		for flag, v := range map[string]string{"-identity": rem.Identity, "-home": rem.Home, "-bin": rem.Bin} {
			if strings.ContainsAny(v, "\x00\r\n") {
				return rem, fmt.Errorf("%s has a control character", flag)
			}
		}
		if strings.HasPrefix(rem.Identity, "-") {
			return rem, errors.New("-identity must be a file path")
		}
	case HTTP:
		if rem.Identity != "" || rem.Home != "" || rem.Bin != "" {
			return rem, errors.New("-identity, -home and -bin are for ssh remotes")
		}
		if rem.TokenEnv != "" && !envNameRE.MatchString(rem.TokenEnv) {
			return rem, fmt.Errorf("-token-env %q: give the name of a variable, not the token", rem.TokenEnv)
		}
	}
	return rem, nil
}
