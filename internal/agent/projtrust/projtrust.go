// Package projtrust is the owner's record of trusted project instruction
// files: for each one, where it is and the SHA-256 of the exact content the
// owner trusted. A project's AGENTS.md, CLAUDE.md or .cursorrules is
// followed as instructions only while its content still has that hash; any
// change, by anyone, makes it untrusted again until the owner trusts it
// again with hk projects trust.
//
// A file is matched by identity, not by how its path is spelled: the same
// file reached through another letter case or a link matches, and so does
// the file at the same place in another checkout of the same repository (a
// git worktree, as delegated tasks use), when its content is the same.
//
// The record lives in $HABAKKUK_HOME/projects/trusted.json. It counts only
// while it is the owner's alone: a regular file of mode 0600 in a folder of
// mode 0700, both owned by the user, neither a link. Otherwise nothing is
// trusted. The file tools and every tool that saves files refuse that
// folder (tools.Policy.Locked).
package projtrust

import (
	"bufio"
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

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// MaxBytes is the largest file that can be trusted: its whole content is
// hashed.
const MaxBytes = 1 << 20

// Dir is the folder under $HABAKKUK_HOME that holds the record.
const Dir = "projects"

// Entry is one trusted file.
type Entry struct {
	Path      string    `json:"path"`           // absolute, symlinks resolved
	Repo      string    `json:"repo,omitempty"` // the repository's shared git folder, when in one
	Rel       string    `json:"rel,omitempty"`  // the file's path in the repository, slash-separated
	SHA256    string    `json:"sha256"`         // of the exact content trusted
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

// ErrNotPrivate is returned when the record or its folder could have been
// written by someone else: then nothing is trusted.
var ErrNotPrivate = errors.New("the trust record is not private to you")

// private checks that path is the owner's alone: not a link, owned by the
// user, with no permission bits for others.
func private(path string, dir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%w: %s is a link", ErrNotPrivate, path)
	case dir && !info.IsDir(), !dir && !info.Mode().IsRegular():
		return fmt.Errorf("%w: %s is not a plain %s", ErrNotPrivate, path, map[bool]string{true: "folder", false: "file"}[dir])
	}
	if !platform.HasUnixPermissions {
		return nil
	}
	if o := platform.FileOwner(info); o >= 0 && o != os.Getuid() {
		return fmt.Errorf("%w: %s belongs to another user", ErrNotPrivate, path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s has mode %04o (want %s)", ErrNotPrivate, path, info.Mode().Perm(), map[bool]string{true: "0700", false: "0600"}[dir])
	}
	return nil
}

func (s *Store) load() ([]Entry, error) {
	if _, err := os.Lstat(s.file()); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err := private(s.Folder(), true); err != nil {
		return nil, err
	}
	if err := private(s.file(), false); err != nil {
		return nil, err
	}
	data, err := ReadRegular(s.file(), 16<<20)
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
	if err := os.Chmod(s.Folder(), 0o700); err != nil {
		return err
	}
	if err := private(s.Folder(), true); err != nil {
		return err
	}
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

// ReadRegular reads up to max bytes of a regular file. It opens first and
// checks the type of what it opened, without following a final link and
// without waiting on a FIFO, so a swap between a check and the read cannot
// pull in another file or hang. More than max bytes is an error.
func ReadRegular(path string, max int64) ([]byte, error) {
	fh, err := openFile(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(fh, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return data, ErrTooLarge
	}
	return data, nil
}

// ErrTooLarge is returned by ReadRegular for a file over its limit; the
// first max+1 bytes come with it.
var ErrTooLarge = errors.New("file too large")

// Hash reads a regular file whole (up to MaxBytes) and returns its
// SHA-256 and its content.
func Hash(path string) (sum string, data []byte, err error) {
	data, err = ReadRegular(path, MaxBytes)
	if errors.Is(err, ErrTooLarge) {
		return "", nil, fmt.Errorf("%s is larger than %d KB, too large to trust", path, MaxBytes>>10)
	}
	if err != nil {
		return "", nil, err
	}
	return Sum(data), data, nil
}

// Sum is the hex SHA-256 of data.
func Sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// place is where a file is: its real path and, in a git repository, the
// repository's shared git folder and the file's path in it.
type place struct {
	real, repo, rel string
}

func locate(path string) place {
	p := place{real: path}
	if r, err := Real(path); err == nil {
		p.real = r
	}
	for dir := filepath.Dir(p.real); ; dir = filepath.Dir(dir) {
		if common, found := gitCommonDir(dir); found {
			// A checkout top whose .git does not hold up gives no
			// repository identity: only the file itself can match.
			if rel, err := filepath.Rel(dir, p.real); err == nil && common != "" {
				p.repo, p.rel = common, filepath.ToSlash(rel)
			}
			return p
		}
		if filepath.Dir(dir) == dir {
			return p
		}
	}
}

// gitCommonDir finds the shared git folder of a checkout whose top is
// dir. found reports that dir holds a .git at all; common is "" when that
// .git does not hold up as a checkout, and then the files there match only
// by their own identity. It does not run git.
//
// A main checkout has a .git folder: that folder is the shared one. A
// worktree has a .git file naming its entry, <common>/worktrees/<name>,
// and git keeps the way back in that entry's gitdir file. Only a .git file
// whose entry names this very .git file back counts, so a folder cannot
// claim a trusted repository's identity by writing a .git file that points
// at it; and a .git file that points straight at a main git folder (which
// has no gitdir file) never counts.
func gitCommonDir(dir string) (common string, found bool) {
	dotgit := filepath.Join(dir, ".git")
	info, err := os.Lstat(dotgit)
	if err != nil {
		return "", false
	}
	if info.IsDir() {
		return realOr(dotgit), true
	}
	data, err := ReadRegular(dotgit, 4096)
	if err != nil {
		return "", true
	}
	line, _, _ := strings.Cut(string(data), "\n")
	entry, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return "", true
	}
	entry = strings.TrimSpace(entry)
	if !filepath.IsAbs(entry) {
		entry = filepath.Join(dir, entry)
	}
	// The way back: the entry's gitdir file names this .git file.
	back, err := ReadRegular(filepath.Join(entry, "gitdir"), 4096)
	if err != nil {
		return "", true
	}
	backPath := strings.TrimSpace(firstLine(string(back)))
	if !filepath.IsAbs(backPath) {
		backPath = filepath.Join(entry, backPath)
	}
	if !sameFile(backPath, dotgit) {
		return "", true
	}
	c, err := ReadRegular(filepath.Join(entry, "commondir"), 4096)
	if err != nil {
		return "", true
	}
	common = strings.TrimSpace(firstLine(string(c)))
	if !filepath.IsAbs(common) {
		common = filepath.Join(entry, common)
	}
	common = realOr(common)
	// The entry must be one of that repository's worktree entries.
	if !sameFile(filepath.Dir(entry), filepath.Join(common, "worktrees")) {
		return "", true
	}
	return common, true
}

func firstLine(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	if sc.Scan() {
		return sc.Text()
	}
	return ""
}

func realOr(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// sameFile reports whether a and b name the same file or folder.
func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

// matches reports whether a record is for the file at p.
func (e Entry) matches(p place) bool {
	if sameFile(e.Path, p.real) {
		return true
	}
	return e.Repo != "" && p.repo != "" && e.Rel == p.rel && sameFile(e.Repo, p.repo)
}

// Check reports whether the file at path is trusted with exactly this
// content, and whether it was ever trusted (so a changed file can say so).
// A record that is not private, or unreadable, trusts nothing.
func (s *Store) Check(path, sum string) (trusted, known bool) {
	s.mu.Lock()
	entries, err := s.load()
	s.mu.Unlock()
	if err != nil {
		return false, false
	}
	p := locate(path)
	for _, e := range entries {
		if e.matches(p) {
			if e.SHA256 == sum {
				return true, true
			}
			known = true
		}
	}
	return false, known
}

// ErrChanged is returned by Trust when the file's content is no longer the
// content the owner was shown.
var ErrChanged = errors.New("the file changed since it was shown")

// Trust records a file as trusted with exactly the content whose SHA-256
// is want (what the owner was shown), replacing any earlier record for the
// same file. If the file's content is no longer that, nothing is written
// and ErrChanged is returned. It returns the record and the content.
func (s *Store) Trust(path, want string) (Entry, []byte, error) {
	real, err := Real(path)
	if err != nil {
		return Entry{}, nil, err
	}
	sum, data, err := Hash(real)
	if err != nil {
		return Entry{}, nil, err
	}
	if sum != want {
		return Entry{}, nil, fmt.Errorf("%w: %s", ErrChanged, path)
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	p := locate(real)
	e := Entry{Path: p.real, Repo: p.repo, Rel: p.rel, SHA256: sum, Size: int64(len(data)), TrustedAt: now().UTC().Truncate(time.Second)}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.load()
	if err != nil && !errors.Is(err, ErrNotPrivate) {
		return Entry{}, nil, err
	}
	if err != nil {
		entries = nil // a record others could have written is not kept
	}
	entries = slices.DeleteFunc(entries, func(x Entry) bool { return sameFile(x.Path, p.real) || x.Path == p.real })
	return e, data, s.save(append(entries, e))
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
		hit := sameFile(target, e.Path) || err == nil && (rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)))
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

// CheckPrivate reports whether the record could have been written by
// someone else (nil when it is private or there is none). Habakkuk then
// trusts nothing; tightening the mode would not make it the owner's, so
// fixers must leave it alone: the owner looks at it and runs hk projects
// trust again, which writes a fresh record.
func (s *Store) CheckPrivate() error {
	if _, err := os.Lstat(s.Folder()); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := private(s.Folder(), true); err != nil {
		return err
	}
	if _, err := os.Lstat(s.file()); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return private(s.file(), false)
}
