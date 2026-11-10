package migrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Copying an entry of the source home. One walk does both jobs: without
// write it only counts what a copy would do (the plan), with write it does
// it (Apply), so the two never disagree.
//
// The copy keeps modes, modification times and symlinks, never follows a
// link, and leaves sockets and devices out. It can be stopped and run
// again: a file is copied only when it is missing at the destination or
// the source is newer, and nothing at the destination is ever deleted. A
// file that is newer at the destination is kept, because once the import
// is done the new agent works there.

// mtimeWindow is how far two modification times may differ and still be
// the same: file systems round them differently.
const mtimeWindow = 2 * time.Second

// partSuffix marks a file still being written; a finished file is renamed
// into place.
const partSuffix = ".habakkuk-part"

// SyncStats says what an entry holds and what a copy does about it.
type SyncStats struct {
	Files, Dirs, Links int   // what the source holds, within the limits
	Bytes              int64 // the size of those files
	// Copy files (CopyBytes) are missing at the destination or older there;
	// MakeDirs folders and MakeLinks links are missing.
	Copy, MakeDirs, MakeLinks int
	CopyBytes                 int64
	Newer                     int      // kept: changed at the destination since
	Blocked                   []string // something else is in the way
	TooBig                    []string // over the size limit, left out
	Special                   int      // sockets, pipes and devices, left out
	// Unreadable could not be copied: the source would not let them be
	// read, or they are links and this system would not make one.
	Unreadable []string
	// Rewritten copied files had References references pointed at the
	// copies.
	Rewritten, References int
}

// todo reports whether a copy would write anything.
func (s SyncStats) todo() bool { return s.Copy+s.MakeDirs+s.MakeLinks > 0 }

// syncEntry walks one entry. With write it copies what is missing or older
// at the destination; without, it only counts. The error is one that
// stops the copy: the destination cannot be written, or ctx ended.
func (p *Plan) syncEntry(ctx context.Context, e *FileEntry, write bool) (SyncStats, error) {
	var st SyncStats
	limit := p.Files.Max
	type made struct {
		path string
		info fs.FileInfo
	}
	var dirs []made
	err := filepath.WalkDir(e.Source, func(path string, d fs.DirEntry, werr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, _ := filepath.Rel(e.Source, path)
		dest := e.Dest
		if rel != "." {
			dest = filepath.Join(e.Dest, rel)
		}
		if werr != nil {
			st.Unreadable = append(st.Unreadable, filepath.ToSlash(rel))
			return nil
		}
		info, err := d.Info()
		if err != nil {
			st.Unreadable = append(st.Unreadable, filepath.ToSlash(rel))
			return nil
		}
		switch mode := info.Mode(); {
		case mode.IsDir():
			st.Dirs++
			have, err := os.Lstat(dest)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				st.MakeDirs++
				if write {
					if err := os.Mkdir(dest, 0o700); err != nil {
						return err
					}
					dirs = append(dirs, made{dest, info})
				}
			case err != nil:
				return err
			case !have.IsDir():
				st.Blocked = append(st.Blocked, filepath.ToSlash(rel))
				return filepath.SkipDir
			}
		case mode.IsRegular():
			if limit > 0 && info.Size() > limit {
				st.TooBig = append(st.TooBig, filepath.ToSlash(rel))
				return nil
			}
			st.Files++
			st.Bytes += info.Size()
			switch p.compare(path, dest, info, p.rw.movesAny(e.inside[rel])) {
			case destNewer:
				st.Newer++
			case destBlocked:
				st.Blocked = append(st.Blocked, filepath.ToSlash(rel))
			case destStale:
				if write {
					n, err := p.copyFile(path, dest, info)
					var unread *unreadable
					if errors.As(err, &unread) {
						st.Unreadable = append(st.Unreadable, filepath.ToSlash(rel))
						return nil
					}
					if err != nil {
						return err
					}
					if n > 0 {
						st.Rewritten++
						st.References += n
					}
				}
				st.Copy++
				st.CopyBytes += info.Size()
			}
		case mode&fs.ModeSymlink != 0:
			st.Links++
			target, err := os.Readlink(path)
			if err != nil {
				st.Unreadable = append(st.Unreadable, filepath.ToSlash(rel))
				return nil
			}
			// A link to something that moves follows it; any other link is
			// kept as it is and never followed.
			if filepath.IsAbs(target) {
				target = p.rw.str(target)
			}
			have, err := os.Lstat(dest)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				if write {
					// Windows makes links only for some users; go on without.
					if err := os.Symlink(target, dest); err != nil {
						st.Unreadable = append(st.Unreadable, filepath.ToSlash(rel))
						return nil
					}
				}
				st.MakeLinks++
			case err != nil:
				return err
			default:
				if now, _ := os.Readlink(dest); have.Mode()&fs.ModeSymlink == 0 || now != target {
					st.Blocked = append(st.Blocked, filepath.ToSlash(rel))
				}
			}
		default:
			st.Special++
		}
		return nil
	})
	// A new folder gets its mode and time last, after its files are in.
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		_ = os.Chtimes(d.path, d.info.ModTime(), d.info.ModTime())
		_ = os.Chmod(d.path, d.info.Mode().Perm()|0o700)
	}
	return st, err
}

// What compare finds at the destination.
const (
	destSame    = iota // nothing to do
	destStale          // missing, or the source is newer: copy
	destNewer          // changed there since: keep
	destBlocked        // not a file: leave alone
)

// compare decides what a source file needs by size and modification time.
// rewritten says the file names entries that move, so its copy differs
// from it and only the contents can tell whether the copy is current.
func (p *Plan) compare(src, dest string, si fs.FileInfo, rewritten bool) int {
	di, err := os.Lstat(dest)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return destStale
	case err != nil || !di.Mode().IsRegular():
		return destBlocked
	}
	d := di.ModTime().Sub(si.ModTime())
	switch {
	case d > mtimeWindow:
		return destNewer
	case d < -mtimeWindow:
		return destStale
	case di.Size() == si.Size() && !rewritten:
		return destSame
	}
	// About the same time, another size. It may be the copy with its
	// references rewritten.
	if p.rw != nil && si.Size() <= rewriteCap {
		if data, err := os.ReadFile(src); err == nil {
			want, _ := p.rw.bytes(data)
			if have, err := os.ReadFile(dest); err == nil && bytes.Equal(have, want) {
				return destSame
			}
		}
	}
	if d > 0 {
		return destNewer // changed in the copy a moment after
	}
	return destStale
}

// unreadable is a failure to read the source, which the copy passes over.
type unreadable struct{ err error }

func (u *unreadable) Error() string { return u.err.Error() }

// copyFile copies one file through a temporary name, keeping its mode and
// time. A small text file has its references rewritten on the way; the
// count is returned.
func (p *Plan) copyFile(src, dest string, si fs.FileInfo) (int, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, &unreadable{err}
	}
	defer in.Close()
	var head []byte
	refs := 0
	if si.Size() <= rewriteCap {
		if head, err = io.ReadAll(io.LimitReader(in, rewriteCap+1)); err != nil {
			return 0, &unreadable{err}
		}
		if len(head) <= rewriteCap {
			head, refs = p.rw.bytes(head)
		}
	}
	tmp := dest + partSuffix
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	_, err = out.Write(head)
	if err == nil {
		_, err = io.Copy(out, in)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chtimes(tmp, si.ModTime(), si.ModTime())
	}
	if err == nil {
		err = os.Chmod(tmp, si.Mode().Perm())
	}
	if err == nil && runtime.GOOS == "windows" {
		_ = os.Chmod(dest, 0o600) // a read-only file cannot be replaced there
	}
	if err == nil {
		err = os.Rename(tmp, dest)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return refs, nil
}

// FilesResult is what a copy did.
type FilesResult struct {
	Dir     string
	Entries []*FileEntry          // the entries now in place
	Stats   map[string]*SyncStats // by entry name
}

// Totals adds up the entries' stats.
func (r *FilesResult) Totals() SyncStats {
	var t SyncStats
	for _, s := range r.Stats {
		t.Copy += s.Copy
		t.CopyBytes += s.CopyBytes
		t.MakeLinks += s.MakeLinks
		t.Rewritten += s.Rewritten
		t.References += s.References
		t.Unreadable = append(t.Unreadable, s.Unreadable...)
	}
	return t
}

// applyFiles copies the entries and records them. Its error means the
// copy stopped part way; the next run goes on from there.
func (p *Plan) applyFiles(ctx context.Context) (*FilesResult, error) {
	fp := p.Files
	if !fp.Copies() || len(fp.Moves) == 0 {
		return nil, nil
	}
	res := &FilesResult{Dir: fp.Dir, Stats: map[string]*SyncStats{}}
	if under(p.Home, fp.Dir) {
		if err := os.MkdirAll(fp.Dir, 0o700); err != nil {
			return nil, err
		}
	}
	for _, e := range fp.Moves {
		if err := os.MkdirAll(filepath.Dir(e.Dest), 0o700); err != nil {
			return nil, err
		}
		if e.Kind != "missing" {
			st, err := p.syncEntry(ctx, e, true)
			res.Stats[e.Name] = &st
			if err != nil {
				return nil, fmt.Errorf("copying %s to %s stopped: %w", e.Source, e.Dest, err)
			}
		}
		res.Entries = append(res.Entries, e)
	}
	if err := p.recordAdopted(); err != nil {
		return nil, err
	}
	return res, nil
}
