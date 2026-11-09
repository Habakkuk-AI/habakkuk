package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// LockName is the lock file in an install folder ($HABAKKUK_HOME/skills,
// $HABAKKUK_HOME/plugins).
const LockName = ".lock.json"

// Lock records every package installed from outside.
type Lock struct {
	Version  int               `json:"version"`
	Packages map[string]*Entry `json:"packages"`
}

// Entry is one installed package.
type Entry struct {
	// Source is what update fetches again: what was given to install, or
	// for one package of a source that held several, that package's folder
	// in it.
	Source   string `json:"source"`
	Path     string `json:"path,omitempty"`     // the package's folder inside an archive source
	Registry string `json:"registry,omitempty"` // the registry that named it
	Resolved string `json:"resolved,omitempty"` // the concrete source a registry name pointed at
	Ref      string `json:"ref,omitempty"`      // the ref asked for (branch, tag, commit)
	// Version is what Ref resolved to: a commit for GitHub, the registry's
	// version, or the sha256 of a downloaded file; empty for local paths.
	Version   string    `json:"version,omitempty"`
	SHA256    string    `json:"sha256"` // TreeHash of the installed files
	Installed time.Time `json:"installed_at"`
	Updated   time.Time `json:"updated_at,omitzero"`
	// Trusted is false until the owner approves the package. Untrusted
	// skills are loaded with a warning not to run their scripts unasked;
	// plugins load only when trusted and enabled.
	Trusted bool     `json:"trusted"`
	Enabled bool     `json:"enabled,omitempty"` // plugins
	Flags   []string `json:"flags,omitempty"`   // scan findings accepted with -y
}

// ReadLock reads a lock file; a missing one is empty.
func ReadLock(path string) (*Lock, error) {
	l := &Lock{Version: 1, Packages: map[string]*Entry{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, l); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if l.Packages == nil {
		l.Packages = map[string]*Entry{}
	}
	return l, nil
}

// Write replaces the lock file atomically.
func (l *Lock) Write(path string) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// withFileLock holds an exclusive lock on path+".lk" while f runs, so two
// habakkuk processes never write the lock file over each other.
func withFileLock(path string, f func() error) error {
	lf, err := os.OpenFile(path+".lk", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := platform.Lock(lf); err != nil {
		return err
	}
	defer func() { _ = platform.Unlock(lf) }()
	return f()
}
