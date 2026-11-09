package hub

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ErrFlagged means the security scan found something and the caller did
// not accept it; nothing was installed. High-risk findings are ErrHighRisk.
var ErrFlagged = errors.New("the security scan flagged this package: read the findings, then rerun with -y to install it anyway")

// Manager installs packages of one kind into one folder.
type Manager struct {
	Kind    string // "skills" or "plugins": picks the registry list, names things in messages
	Dir     string // where packages are installed, one folder each
	Marker  string // the file that marks a package folder: SKILL.md, habakkuk-plugin.yaml
	Fetcher *Fetcher
	// ValidName checks a package name.
	ValidName func(string) error
	// NameOf reads the name a package folder declares; "" when it names none.
	NameOf func(dir string) string
	// Check validates a package folder before it is installed (a plugin's
	// manifest, say); nil accepts any.
	Check func(dir string) error
	Now   func() time.Time
}

// Options change how Install behaves.
type Options struct {
	Name string // install under this name (sources with one package only)
	Yes  bool   // install even when the scan flags something (not high-risk findings)
	// Force replaces a package of the same name that is already there, and
	// installs even when the scan finds high-risk patterns.
	Force bool
}

// UpdateOptions change how Update behaves.
type UpdateOptions struct {
	Yes   bool // update even when the scan flags something (not high-risk findings)
	Force bool // update even when the scan finds high-risk patterns
	// Confirm is shown each changed package (its diff, scripts and
	// findings) before it replaces the installed one; false keeps the
	// installed version. Nil applies every update.
	Confirm func(r Result) bool
}

// Result is what happened to one package.
type Result struct {
	Name     string
	Dir      string
	Entry    Entry
	Findings []Finding
	Status   string   // installed, updated, unchanged, flagged, declined
	Backup   string   // the backup id of the version this one replaced
	Files    []string // the package's files (slash paths)
	Scripts  []string // those an agent could run as programs
	Changes  []Change // update: what differs from the installed version
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) noun() string { return strings.TrimSuffix(m.Kind, "s") }

// LockPath is the lock file of this folder.
func (m *Manager) LockPath() string { return filepath.Join(m.Dir, LockName) }

// Backups keeps the versions that installs, updates and removals replaced.
func (m *Manager) Backups() Backups {
	return Backups{Dir: filepath.Join(m.Dir, ".backups"), Now: m.Now}
}

// Entries reads the lock file.
func (m *Manager) Entries() (map[string]*Entry, error) {
	l, err := ReadLock(m.LockPath())
	if err != nil {
		return nil, err
	}
	return l.Packages, nil
}

// locked runs f with the lock file read, under an exclusive file lock, and
// writes the lock file back afterwards (also when f fails part way, so
// packages that did change are recorded).
func (m *Manager) locked(f func(*Lock) error) error {
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return err
	}
	return withFileLock(m.LockPath(), func() error {
		l, err := ReadLock(m.LockPath())
		if err != nil {
			return err
		}
		ferr := f(l)
		if err := l.Write(m.LockPath()); err != nil {
			return errors.Join(ferr, err)
		}
		return ferr
	})
}

type staged struct {
	name, dir, rel string
	hash           string
	findings       []Finding
	files, scripts []string
}

// Install fetches raw, finds the packages in it (the source itself when it
// holds the marker, else every folder below it that does), scans and
// hashes them, and moves them into place. When the scan flags anything and
// opt.Yes is not set, nothing is installed and the error is ErrFlagged.
func (m *Manager) Install(ctx context.Context, raw string, opt Options) ([]Result, error) {
	src, err := ParseSource(raw)
	if err != nil {
		return nil, err
	}
	var results []Result
	err = m.locked(func(lock *Lock) error {
		work, err := os.MkdirTemp(m.Dir, ".tmp-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(work) }()
		got, err := m.Fetcher.Fetch(ctx, src, work, m.Marker, m.Kind)
		if err != nil {
			return err
		}
		rels, err := findPackages(got.Root, m.Marker)
		if err != nil {
			return err
		}
		switch {
		case len(rels) == 0:
			return fmt.Errorf("no %s found in %s (looked for %s)", m.noun(), raw, m.Marker)
		case len(rels) > 1 && opt.Name != "":
			return fmt.Errorf("%s holds %d %ss; -name works with one only", raw, len(rels), m.noun())
		case len(rels) > 1 && got.Registry != "":
			return fmt.Errorf("registry entry %s holds %d %ss; it must point at one", raw, len(rels), m.noun())
		}
		var pkgs []staged
		flagged, high := false, false
		for _, rel := range rels {
			p := staged{rel: rel, dir: filepath.Join(got.Root, filepath.FromSlash(rel))}
			if p.name, err = m.pickName(p.dir, rel, src, opt.Name); err != nil {
				return err
			}
			if slices.ContainsFunc(pkgs, func(q staged) bool { return q.name == p.name }) {
				return fmt.Errorf("%s holds two %ss named %q", raw, m.noun(), p.name)
			}
			if m.Check != nil {
				if err := m.Check(p.dir); err != nil {
					return fmt.Errorf("%s %s: %w", m.noun(), p.name, err)
				}
			}
			if p.hash, err = TreeHash(p.dir); err != nil {
				return err
			}
			if got.Expect != "" && got.Expect != p.hash {
				return fmt.Errorf("%s %s: sha256 %s does not match %s from registry %s; not installed", m.noun(), p.name, p.hash, got.Expect, got.Registry)
			}
			if p.findings, err = Scan(p.dir); err != nil {
				return err
			}
			if p.files, err = Files(p.dir); err != nil {
				return err
			}
			p.scripts = Scripts(p.dir)
			flagged = flagged || len(p.findings) > 0
			high = high || anyHigh(p.findings)
			if _, err := os.Stat(filepath.Join(m.Dir, p.name)); err == nil && !opt.Force {
				from := "by hand"
				if e := lock.Packages[p.name]; e != nil {
					from = "from " + e.Source
				}
				return fmt.Errorf("%s %q is already installed (%s): use update, or -force to replace it (the old one is backed up)", m.noun(), p.name, from)
			}
			pkgs = append(pkgs, p)
		}
		if (high && !opt.Force) || (flagged && !opt.Yes && !opt.Force) {
			for _, p := range pkgs {
				results = append(results, Result{Name: p.name, Findings: p.findings, Status: "flagged",
					Files: p.files, Scripts: p.scripts, Entry: Entry{Source: raw, Ref: got.Origin.Ref, Version: got.Version, SHA256: p.hash}})
			}
			if high && !opt.Force {
				return ErrHighRisk
			}
			return ErrFlagged
		}
		for _, p := range pkgs {
			e := &Entry{Source: raw, Path: p.rel, Ref: got.Origin.Ref, Version: got.Version, SHA256: p.hash,
				Installed: m.now().UTC(), Registry: got.Registry, Flags: Summaries(p.findings)}
			if got.Registry != "" {
				e.Resolved = got.Origin.String()
			} else if got.Origin.Kind != KindURL && p.rel != "" {
				e.Source, e.Path = got.Origin.Sub(p.rel).String(), ""
			}
			r, err := m.commit(p.name, p.dir, "replaced by install of "+raw, lock.Packages[p.name])
			if err != nil {
				return err
			}
			r.Entry, r.Findings, r.Status = *e, p.findings, "installed"
			r.Files, r.Scripts = p.files, p.scripts
			lock.Packages[p.name] = e
			results = append(results, r)
		}
		return nil
	})
	return results, err
}

// commit moves a staged folder into place, backing up what was there with
// its lock entry (old, nil when it had none).
func (m *Manager) commit(name, staged, reason string, old *Entry) (Result, error) {
	dest := filepath.Join(m.Dir, name)
	r := Result{Name: name, Dir: dest}
	if _, err := os.Stat(dest); err == nil {
		bk, err := m.Backups().save(name, dest, reason, true, clone(old))
		if err != nil {
			return r, fmt.Errorf("back up %s: %w", dest, err)
		}
		r.Backup = bk.ID
	}
	return r, os.Rename(staged, dest)
}

func (m *Manager) pickName(dir, rel string, src Source, override string) (string, error) {
	name := override
	if name == "" && m.NameOf != nil {
		if n := m.NameOf(dir); n != "" && m.ValidName(n) == nil {
			name = n
		}
	}
	if name == "" && rel != "" {
		name = Slug(filepath.Base(filepath.FromSlash(rel)))
	}
	if name == "" {
		name = src.NameHint()
	}
	if err := m.ValidName(name); err != nil {
		return "", fmt.Errorf("%w; pass -name", err)
	}
	return name, nil
}

// findPackages lists the folders under root (slash paths, "" for root
// itself) holding marker, without looking inside a package or hidden
// folders, at most four levels down.
func findPackages(root, marker string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(root, marker)); err == nil {
		return []string{""}, nil
	}
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if strings.HasPrefix(d.Name(), ".") || strings.Count(rel, string(filepath.Separator)) >= 4 {
			return filepath.SkipDir
		}
		if _, err := os.Stat(filepath.Join(p, marker)); err == nil {
			out = append(out, filepath.ToSlash(rel))
			return filepath.SkipDir
		}
		return nil
	})
	return out, err
}

// Update fetches each named package (every locked one when name is "")
// from its recorded source again and replaces it when its files changed.
// The old version is backed up. A package stays trusted only when the new
// version scans clean.
func (m *Manager) Update(ctx context.Context, name string, opt UpdateOptions) ([]Result, error) {
	var results []Result
	err := m.locked(func(lock *Lock) error {
		names := []string{name}
		if name == "" {
			names = slices.Sorted(maps.Keys(lock.Packages))
		}
		var errs []error
		for _, n := range names {
			e := lock.Packages[n]
			if e == nil {
				errs = append(errs, fmt.Errorf("%s %q was not installed from a source (see list)", m.noun(), n))
				continue
			}
			r, err := m.update(ctx, n, e, opt)
			if r.Name != "" {
				results = append(results, r)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", n, err))
			}
		}
		return errors.Join(errs...)
	})
	return results, err
}

func (m *Manager) update(ctx context.Context, name string, e *Entry, opt UpdateOptions) (Result, error) {
	old := clone(e)
	before := *e
	src, err := ParseSource(e.Source)
	if err != nil {
		return Result{}, err
	}
	work, err := os.MkdirTemp(m.Dir, ".tmp-")
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = os.RemoveAll(work) }()
	got, err := m.Fetcher.Fetch(ctx, src, work, m.Marker, m.Kind)
	if err != nil {
		return Result{}, err
	}
	root := filepath.Join(got.Root, filepath.FromSlash(e.Path))
	if _, err := os.Stat(filepath.Join(root, m.Marker)); err != nil {
		return Result{}, fmt.Errorf("no %s at %s any more", m.Marker, e.Source)
	}
	hash, err := TreeHash(root)
	if err != nil {
		return Result{}, err
	}
	if got.Expect != "" && got.Expect != hash {
		return Result{}, fmt.Errorf("sha256 %s does not match %s from registry %s; not updated", hash, got.Expect, got.Registry)
	}
	dest := filepath.Join(m.Dir, name)
	e.Version = got.Version
	if got.Registry != "" {
		e.Resolved = got.Origin.String()
	}
	if _, err := os.Stat(dest); err == nil && hash == e.SHA256 {
		return Result{Name: name, Dir: dest, Entry: *e, Status: "unchanged"}, nil
	}
	if m.Check != nil {
		if err := m.Check(root); err != nil {
			return Result{}, err
		}
	}
	findings, err := Scan(root)
	if err != nil {
		return Result{}, err
	}
	changes, err := Diff(dest, root)
	if err != nil {
		return Result{}, err
	}
	files, err := Files(root)
	if err != nil {
		return Result{}, err
	}
	preview := Result{Name: name, Dir: dest, Entry: *e, Findings: findings, Files: files, Scripts: Scripts(root), Changes: changes}
	preview.Entry.SHA256 = hash
	if high := anyHigh(findings); (high && !opt.Force) || (len(findings) > 0 && !opt.Yes && !opt.Force) {
		*e = before // nothing changes, the recorded version included
		preview.Status = "flagged"
		if high && !opt.Force {
			return preview, ErrHighRisk
		}
		return preview, ErrFlagged
	}
	if opt.Confirm != nil && !opt.Confirm(preview) {
		*e = before
		preview.Status = "declined"
		return preview, nil
	}
	// A changed script needs the owner's approval again, like a new package.
	oldScripts := Scripts(dest)
	scriptChanged := slices.ContainsFunc(changes, func(c Change) bool {
		return slices.Contains(preview.Scripts, c.Path) || slices.Contains(oldScripts, c.Path)
	})
	r, err := m.commit(name, root, "replaced by update", old)
	if err != nil {
		return Result{}, err
	}
	e.SHA256, e.Updated, e.Flags = hash, m.now().UTC(), Summaries(findings)
	e.Trusted = e.Trusted && len(findings) == 0 && !scriptChanged
	r.Entry, r.Findings, r.Status = *e, findings, "updated"
	r.Files, r.Scripts, r.Changes = files, preview.Scripts, changes
	return r, nil
}

// Remove moves a package to the backups (so it can be restored) and drops
// it from the lock file.
func (m *Manager) Remove(name string) (Backup, error) {
	if err := m.ValidName(name); err != nil {
		return Backup{}, err
	}
	var bk Backup
	err := m.locked(func(lock *Lock) error {
		dest := filepath.Join(m.Dir, name)
		_, inLock := lock.Packages[name]
		if _, err := os.Stat(dest); err == nil {
			var err error
			if bk, err = m.Backups().save(name, dest, "removed", true, clone(lock.Packages[name])); err != nil {
				return err
			}
		} else if !inLock {
			return fmt.Errorf("no %s %q is installed", m.noun(), name)
		}
		delete(lock.Packages, name)
		return nil
	})
	return bk, err
}

// Restore puts a backup back (the newest when id is ""), with the lock
// entry it had: a package restored after a removal or an update is again
// what it was, approved or not. What is there now is backed up first.
func (m *Manager) Restore(name, id string) (Backup, error) {
	var bk Backup
	err := m.locked(func(lock *Lock) error {
		var err error
		if bk, err = m.Backups().restore(name, id, clone(lock.Packages[name])); err != nil {
			return err
		}
		if filepath.Dir(bk.Origin) != m.Dir || filepath.Base(bk.Origin) != name {
			return nil // not an installed package (an agent's own skill, say)
		}
		if bk.Entry != nil {
			lock.Packages[name] = bk.Entry
		} else {
			delete(lock.Packages, name)
		}
		return nil
	})
	return bk, err
}

func clone(e *Entry) *Entry {
	if e == nil {
		return nil
	}
	c := *e
	c.Flags = slices.Clone(e.Flags)
	return &c
}

// Set changes a locked package's entry (trust, enabled).
func (m *Manager) Set(name string, f func(*Entry)) error {
	return m.locked(func(lock *Lock) error {
		e := lock.Packages[name]
		if e == nil {
			return fmt.Errorf("no %s %q was installed from a source", m.noun(), name)
		}
		f(e)
		return nil
	})
}

// Search looks name and description words up in the registries.
func (m *Manager) Search(ctx context.Context, query string) ([]IndexEntry, []error) {
	return m.Fetcher.Search(ctx, m.Kind, query)
}
