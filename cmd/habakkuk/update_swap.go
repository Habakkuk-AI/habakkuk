package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	plat "github.com/habakkuk-ai/habakkuk/internal/core/platform"
)

// installBinary puts data at exe in one step, so exe is always either the
// old binary or the new one:
//
//   - Unix: write a temporary file beside exe and rename it over exe. A
//     running process keeps the old inode.
//   - Windows: a running .exe cannot be replaced or deleted, but it can be
//     renamed. Move it aside to exe.old, then rename the new file into
//     place; if that fails, the old one is moved back. The .old file is
//     removed the next time Habakkuk starts (removeReplaced).
func installBinary(exe string, data []byte) error {
	mode := os.FileMode(0o755)
	if info, err := os.Stat(exe); err == nil {
		mode = info.Mode().Perm() | 0o100
	}
	if plat.IsWindows {
		return swapAside(exe, data, mode)
	}
	return writeAtomic(exe, bytes.NewReader(data), mode)
}

// swapAside is the Windows replacement strategy (it works anywhere; tests
// run it on every OS).
func swapAside(exe string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(exe)
	f, err := os.CreateTemp(dir, "."+filepath.Base(exe)+".new-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once renamed into place
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	aside := exe + ".old"
	if err := os.Remove(aside); err != nil && !errors.Is(err, os.ErrNotExist) {
		// An earlier .old is still running (or locked): pick a fresh name.
		aside = exe + ".old-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	hadOld := true
	if err := os.Rename(exe, aside); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("move the running binary aside: %w", err)
		}
		hadOld = false
	}
	if err := os.Rename(tmp, exe); err != nil {
		if hadOld {
			if rerr := os.Rename(aside, exe); rerr != nil {
				return fmt.Errorf("install the new binary: %w; and restoring the old one failed too (%v): it is at %s", err, rerr, aside)
			}
		}
		return fmt.Errorf("install the new binary: %w", err)
	}
	return nil
}

// removeReplaced deletes binaries an update moved aside (Windows), once
// they are no longer running. Errors are ignored: a still-running one is
// removed next time.
func removeReplaced(exe string) {
	matches, _ := filepath.Glob(exe + ".old*")
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

// checkWritable reports whether a file can be created in dir, before an
// update spends time on backups it cannot finish.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".habakkuk-write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// copyFile copies src to dst (replacing it atomically) with mode.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return writeAtomic(dst, in, mode)
}

// writeAtomic streams r into a temporary file beside path, syncs it and
// renames it over path, so readers see the old file or the new one.
func writeAtomic(path string, r io.Reader, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after the rename
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
