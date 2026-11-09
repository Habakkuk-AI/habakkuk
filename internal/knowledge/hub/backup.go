package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// KeepBackups is how many backups of one name are kept.
const KeepBackups = 10

// Backups keeps copies of package folders before they are replaced,
// removed, merged or archived, under Dir/<name>/<time>/: the folder itself
// in files/ and a backup.json saying where it came from and why.
type Backups struct {
	Dir string
	Now func() time.Time
}

// Backup describes one saved copy.
type Backup struct {
	Name   string    `json:"name"`
	Origin string    `json:"origin"` // the folder it was taken from, and is restored to
	Reason string    `json:"reason"`
	Time   time.Time `json:"time"`
	// Entry is the package's lock entry when it was saved, so a restore
	// puts back where it came from and whether it was approved.
	Entry *Entry `json:"entry,omitempty"`
	ID    string `json:"-"` // the time folder's name
	Dir   string `json:"-"` // the saved files
}

func (b Backups) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Save copies origin (or moves it, when move is set) into a new backup.
func (b Backups) Save(name, origin, reason string, move bool) (Backup, error) {
	return b.save(name, origin, reason, move, nil)
}

func (b Backups) save(name, origin, reason string, move bool, e *Entry) (Backup, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return Backup{}, fmt.Errorf("invalid backup name %q", name)
	}
	t := b.now().UTC()
	id := t.Format("20060102T150405.000000000Z")
	slot := filepath.Join(b.Dir, name, id)
	if err := os.MkdirAll(slot, 0o700); err != nil {
		return Backup{}, err
	}
	files := filepath.Join(slot, "files")
	var err error
	if move {
		err = os.Rename(origin, files)
	} else {
		err = copyTree(origin, files)
	}
	if err != nil {
		_ = os.RemoveAll(slot)
		return Backup{}, err
	}
	bk := Backup{Name: name, Origin: origin, Reason: reason, Time: t, Entry: e, ID: id, Dir: files}
	data, _ := json.MarshalIndent(bk, "", "  ")
	if err := store.WriteFileAtomic(filepath.Join(slot, "backup.json"), data, 0o600); err != nil {
		return Backup{}, err
	}
	b.rotate(name)
	return bk, nil
}

// List returns the backups of name, newest first.
func (b Backups) List(name string) ([]Backup, error) {
	root := filepath.Join(b.Dir, name)
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Backup
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(root, e.Name(), "backup.json"))
		if err != nil {
			continue
		}
		var bk Backup
		if json.Unmarshal(data, &bk) != nil {
			continue
		}
		bk.ID, bk.Dir = e.Name(), filepath.Join(root, e.Name(), "files")
		out = append(out, bk)
	}
	slices.SortFunc(out, func(x, y Backup) int { return strings.Compare(y.ID, x.ID) })
	return out, nil
}

// Names lists every name with at least one backup.
func (b Backups) Names() ([]string, error) {
	entries, err := os.ReadDir(b.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// Restore puts a backup (the newest when id is "") back where it came from.
// Whatever is there now is backed up first, so a restore can be undone.
func (b Backups) Restore(name, id string) (Backup, error) {
	return b.restore(name, id, nil)
}

// restore is Restore; cur is the lock entry of what is there now, kept
// with its backup.
func (b Backups) restore(name, id string, cur *Entry) (Backup, error) {
	list, err := b.List(name)
	if err != nil {
		return Backup{}, err
	}
	i := slices.IndexFunc(list, func(bk Backup) bool { return id == "" || bk.ID == id })
	if i < 0 {
		if id == "" {
			return Backup{}, fmt.Errorf("no backups of %q", name)
		}
		return Backup{}, fmt.Errorf("no backup %s of %q", id, name)
	}
	bk := list[i]
	if err := os.MkdirAll(filepath.Dir(bk.Origin), 0o700); err != nil {
		return Backup{}, err
	}
	// Take the backup out first, so saving the current folder (which may
	// rotate old backups away) cannot lose it.
	tmp := bk.Origin + ".restoring"
	_ = os.RemoveAll(tmp)
	if err := os.Rename(bk.Dir, tmp); err != nil {
		return Backup{}, err
	}
	if _, err := os.Stat(bk.Origin); err == nil {
		if _, err := b.save(name, bk.Origin, "replaced by restore of "+bk.ID, true, cur); err != nil {
			_ = os.Rename(tmp, bk.Dir)
			return Backup{}, err
		}
	}
	if err := os.Rename(tmp, bk.Origin); err != nil {
		return Backup{}, err
	}
	_ = os.RemoveAll(filepath.Dir(bk.Dir))
	return bk, nil
}

func (b Backups) rotate(name string) {
	list, err := b.List(name)
	if err != nil {
		return
	}
	for _, old := range list[min(len(list), KeepBackups):] {
		_ = os.RemoveAll(filepath.Dir(old.Dir))
	}
}
