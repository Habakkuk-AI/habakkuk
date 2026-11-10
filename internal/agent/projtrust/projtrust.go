// Package projtrust is the owner's record of trusted project instruction
// files: for each one, its path and the SHA-256 of the exact content the
// owner trusted. A project's AGENTS.md, CLAUDE.md or .cursorrules is
// followed as instructions only while its content still has that hash; any
// change, by anyone, makes it untrusted again until the owner trusts it
// again with hk projects trust.
//
// The record lives in $HABAKKUK_HOME/projects/trusted.json, owner-only.
// Only hk projects writes it: the file tools refuse that folder in every
// turn (tools.Policy.Locked).
package projtrust

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// MaxBytes is the largest file that can be trusted: its whole content is
// hashed.
const MaxBytes = 1 << 20

// Dir is the folder under $HABAKKUK_HOME that holds the record.
const Dir = "projects"

// Entry is one trusted file.
type Entry struct {
	Path      string    `json:"path"`   // absolute, symlinks resolved
	SHA256    string    `json:"sha256"` // of the exact content trusted
	Size      int64     `json:"size"`
	TrustedAt time.Time `json:"trusted_at"`
}

// Store reads and writes the record.
type Store struct {
	Home string // $HABAKKUK_HOME
	Now  func() time.Time

	mu sync.Mutex
}

// Folder is the store's folder, which no tool may write.
func (s *Store) Folder() string { return filepath.Join(s.Home, Dir) }

func (s *Store) file() string { return filepath.Join(s.Folder(), "trusted.json") }

func (s *Store) load() ([]Entry, error) {
	data, err := os.ReadFile(s.file())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", s.file(), err)
	}
	return out, nil
}

func (s *Store) save(entries []Entry) error {
	if err := os.MkdirAll(s.Folder(), 0o700); err != nil {
		return err
	}
	_ = os.Chmod(s.Folder(), 0o700)
	slices.SortFunc(entries, func(a, b Entry) int { return strings.Compare(a.Path, b.Path) })
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(s.file(), append(data, '\n'), 0o600)
}

// Real is the path a file is recorded under: absolute, symlinks resolved.
func Real(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// Hash reads a regular file whole (up to MaxBytes) and returns its
// SHA-256 and its content.
func Hash(path string) (sum string, data []byte, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("%s is not a regular file", path)
	}
	fh, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer fh.Close()
	data, err = io.ReadAll(io.LimitReader(fh, MaxBytes+1))
	if err != nil {
		return "", nil, err
	}
	if len(data) > MaxBytes {
		return "", nil, fmt.Errorf("%s is larger than %d KB, too large to trust", path, MaxBytes>>10)
	}
	return Sum(data), data, nil
}

// Sum is the hex SHA-256 of data.
func Sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// Check reports whether the file at path (resolved) is trusted with
// exactly this content, and whether it was ever trusted (so a changed file
// can say so). An unreadable record trusts nothing.
func (s *Store) Check(path, sum string) (trusted, known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.load()
	if err != nil {
		return false, false
	}
	for _, e := range entries {
		if e.Path == path {
			return e.SHA256 == sum, true
		}
	}
	return false, false
}

// Trust records a file's current content as trusted, replacing any
// earlier record for it.
func (s *Store) Trust(path string) (Entry, error) {
	real, err := Real(path)
	if err != nil {
		return Entry{}, err
	}
	sum, data, err := Hash(real)
	if err != nil {
		return Entry{}, err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	e := Entry{Path: real, SHA256: sum, Size: int64(len(data)), TrustedAt: now().UTC().Truncate(time.Second)}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.load()
	if err != nil {
		return Entry{}, err
	}
	entries = slices.DeleteFunc(entries, func(x Entry) bool { return x.Path == real })
	return e, s.save(append(entries, e))
}

// Untrust removes the record for a file, or for every file under a
// folder. It returns what it removed.
func (s *Store) Untrust(path string) ([]Entry, error) {
	target, err := Real(path)
	if err != nil {
		// A file that is gone can still be untrusted by the path it had.
		if target, err = filepath.Abs(path); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.load()
	if err != nil {
		return nil, err
	}
	var removed []Entry
	kept := slices.DeleteFunc(entries, func(e Entry) bool {
		rel, err := filepath.Rel(target, e.Path)
		hit := err == nil && (rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)))
		if hit {
			removed = append(removed, e)
		}
		return hit
	})
	if len(removed) == 0 {
		return nil, nil
	}
	return removed, s.save(kept)
}

// Status says what a recorded file is now.
type Status string

// Statuses of a recorded file.
const (
	StatusTrusted Status = "trusted"
	StatusChanged Status = "changed since it was trusted"
	StatusMissing Status = "missing"
)

// List returns every record with its file's status now.
func (s *Store) List() ([]Entry, []Status, error) {
	s.mu.Lock()
	entries, err := s.load()
	s.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	st := make([]Status, len(entries))
	for i, e := range entries {
		sum, _, err := Hash(e.Path)
		switch {
		case err != nil:
			st[i] = StatusMissing
		case sum != e.SHA256:
			st[i] = StatusChanged
		default:
			st[i] = StatusTrusted
		}
	}
	return entries, st, nil
}
