package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// Backups are a tar.gz of $HABAKKUK_HOME's state: config.yaml, agents/
// (personas, memory, skills; workspaces only with -workspaces), skills/,
// personalities/, and a consistent snapshot of each database taken with
// VACUUM INTO while Habakkuk may be running. Secrets (.env and WhatsApp
// device credentials) go in only with -env. The search index memory.db,
// logs and the blob store are left out: the index rebuilds from the notes,
// and blobs hold attachment copies and large tool outputs.

// backupDBs are the databases a backup snapshots, relative to the home.
var backupDBs = []string{"control.db", "transcripts.db", "state.db", "web.db"}

// upgradeDBs are snapshotted too by the backup an update takes: a new
// release may migrate them, and an older binary refuses a database migrated
// past what it knows, so a rollback must restore them as well.
var upgradeDBs = []string{"memory.db", "usage.db", "nodes.db"}

// backupDBNames is every database name a backup may hold (besides
// channels/*.db).
func backupDBNames() []string { return slices.Concat(backupDBs, upgradeDBs) }

// backupOptions picks what goes into a backup beyond config and agents.
type backupOptions struct {
	env        bool // .env and WhatsApp device credentials
	workspaces bool // agents/<name>/workspace
	allDBs     bool // memory, usage and nodes databases too
}

// backupDirs are the folders a backup copies, relative to the home.
var backupDirs = []string{"agents", "skills", "personalities"}

func backupCmd(ctx context.Context, home string, args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	out := fs.String("o", "", "archive to write (default habakkuk-backup-<time>.tar.gz in the current directory)")
	env := fs.Bool("env", false, "include secrets: .env and WhatsApp device credentials")
	workspaces := fs.Bool("workspaces", false, "include agent workspaces kept under agents/<name>/workspace")
	fs.Parse(args)
	path := *out
	if path == "" {
		path = "habakkuk-backup-" + time.Now().UTC().Format("20060102-150405") + ".tar.gz"
	}
	n, err := writeBackup(ctx, home, path, *env, *workspaces)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d files)\n", path, n)
	if *env {
		fmt.Println("it holds your API keys: keep it private")
	}
	return nil
}

func writeBackup(ctx context.Context, home, path string, env, workspaces bool) (int, error) {
	return writeBackupOpts(ctx, home, path, backupOptions{env: env, workspaces: workspaces})
}

func writeBackupOpts(ctx context.Context, home, path string, o backupOptions) (int, error) {
	env, workspaces := o.env, o.workspaces
	if _, err := os.Stat(filepath.Join(home, "config.yaml")); err != nil {
		return 0, fmt.Errorf("nothing to back up at %s: %w", home, err)
	}
	tmpDir, err := os.MkdirTemp("", "habakkuk-backup-")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer os.Remove(path + ".tmp")
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	n := 0
	add := func(name, src string) error {
		if err := addFile(tw, name, src); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		n++
		return nil
	}
	err = func() error {
		if err := add("config.yaml", filepath.Join(home, "config.yaml")); err != nil {
			return err
		}
		if env {
			if _, err := os.Stat(filepath.Join(home, ".env")); err == nil {
				if err := add(".env", filepath.Join(home, ".env")); err != nil {
					return err
				}
			}
		}
		dbs := slices.Clone(backupDBs)
		if o.allDBs {
			dbs = backupDBNames()
		}
		if env {
			creds, _ := filepath.Glob(filepath.Join(home, "channels", "*.db"))
			for _, c := range creds {
				dbs = append(dbs, filepath.Join("channels", filepath.Base(c)))
			}
		}
		for _, rel := range dbs {
			src := filepath.Join(home, rel)
			if _, err := os.Stat(src); err != nil {
				continue
			}
			snap := filepath.Join(tmpDir, strings.ReplaceAll(rel, string(filepath.Separator), "_"))
			if err := snapshotDB(ctx, src, snap); err != nil {
				return fmt.Errorf("snapshot %s: %w", rel, err)
			}
			if err := add(filepath.ToSlash(rel), snap); err != nil {
				return err
			}
		}
		for _, dir := range backupDirs {
			root := filepath.Join(home, dir)
			err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						return nil
					}
					return err
				}
				rel, _ := filepath.Rel(home, p)
				parts := strings.Split(filepath.ToSlash(rel), "/")
				if d.IsDir() {
					// agents/<name>/workspace is the default workspace.
					if !workspaces && len(parts) == 3 && parts[0] == "agents" && parts[2] == "workspace" {
						return filepath.SkipDir
					}
					return nil
				}
				if !d.Type().IsRegular() {
					return nil // symlinks and devices are not state
				}
				return add(filepath.ToSlash(rel), p)
			})
			if err != nil {
				return err
			}
		}
		return nil
	}()
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = zw.Close()
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	return n, os.Rename(path+".tmp", path)
}

// snapshotDB writes a consistent, owner-only copy of a live database to dst.
func snapshotDB(ctx context.Context, src, dst string) error {
	db, err := store.Open(ctx, src, nil)
	if err != nil {
		return err
	}
	defer db.Close()
	return store.VacuumInto(ctx, db, dst)
}

func addFile(tw *tar.Writer, name, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: int64(info.Mode().Perm()), Size: info.Size(),
		ModTime: info.ModTime(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.Copy(tw, io.LimitReader(f, info.Size()))
	return err
}

func restoreCmd(home string, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	force := fs.Bool("force", false, "overwrite an existing config and databases")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: habakkuk restore [-force] FILE")
	}
	n, err := restoreBackup(fs.Arg(0), home, *force)
	if err != nil {
		return err
	}
	fmt.Printf("restored %d files into %s; run `habakkuk doctor` to check them\n", n, home)
	return nil
}

// restoreBackup unpacks a backup into home. Only regular files under the
// names a backup writes are accepted, so an archive cannot write outside
// home. A database replaces the old one together with its WAL, which would
// otherwise be replayed into the restored file.
func restoreBackup(archive, home string, force bool) (int, error) {
	if _, err := os.Stat(filepath.Join(home, "config.yaml")); err == nil && !force {
		return 0, fmt.Errorf("%s already has a config; stop Habakkuk and pass -force to overwrite it", home)
	}
	f, err := os.Open(archive)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return 0, fmt.Errorf("%s is not a Habakkuk backup: %w", archive, err)
	}
	tr := tar.NewReader(zr)
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return n, err
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return n, fmt.Errorf("refusing %s: only regular files are restored", h.Name)
		}
		rel, err := restorable(h.Name)
		if err != nil {
			return n, err
		}
		dst := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return n, err
		}
		mode := os.FileMode(h.Mode).Perm() & 0o755
		if strings.HasSuffix(rel, ".db") || rel == ".env" || rel == "config.yaml" {
			mode = 0o600
		}
		if strings.HasSuffix(rel, ".db") {
			for _, side := range []string{dst + "-wal", dst + "-shm"} {
				if err := os.Remove(side); err != nil && !errors.Is(err, os.ErrNotExist) {
					return n, err
				}
			}
		}
		if err := writeAtomic(dst, tr, mode); err != nil {
			return n, err
		}
		n++
	}
	if n == 0 {
		return 0, errors.New("the archive is empty")
	}
	return n, nil
}

// backupNames lists the files in a backup archive (slash-separated).
func backupNames(archive string) (map[string]bool, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("%s is not a Habakkuk backup: %w", archive, err)
	}
	tr := tar.NewReader(zr)
	names := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		names[filepath.ToSlash(filepath.Clean(h.Name))] = true
	}
}

// restorable checks an archive name and returns it as a relative path.
func restorable(name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return "", fmt.Errorf("refusing %s: outside the Habakkuk home", name)
	}
	top, _, _ := strings.Cut(filepath.ToSlash(clean), "/")
	switch {
	case clean == "config.yaml", clean == ".env", slices.Contains(backupDBNames(), clean):
	case top == "channels" && strings.HasSuffix(clean, ".db"):
	case slices.Contains(backupDirs, top) && top != clean:
	default:
		return "", fmt.Errorf("refusing %s: not part of a Habakkuk backup", name)
	}
	return clean, nil
}
