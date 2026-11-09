package hub

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Limits on what one package may unpack to.
const (
	MaxDownload = 64 << 20  // bytes fetched over HTTP
	maxTotal    = 128 << 20 // bytes unpacked
	maxFiles    = 10000
)

var errTooBig = errors.New("package is too large")

// writer unpacks entries under a root, refusing anything that would land
// outside it. Only folders and regular files are written; symlinks, hard
// links and devices are skipped. Files are written 0600 (folders 0700) whatever their mode
// in the archive, so nothing installed is executable until someone makes it
// so.
type writer struct {
	root  string
	total int64
	files int
}

func (w *writer) target(name string, strip int) (string, bool, error) {
	name = strings.TrimPrefix(strings.ReplaceAll(name, "\\", "/"), "./")
	parts := strings.Split(strings.Trim(name, "/"), "/")
	if len(parts) <= strip {
		return "", false, nil
	}
	rel := strings.Join(parts[strip:], "/")
	if rel == "" || rel == "." {
		return "", false, nil
	}
	if path.IsAbs(name) || strings.Contains(rel, ":") || rel != path.Clean(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false, fmt.Errorf("archive entry %q leaves the package folder", name)
	}
	for _, p := range strings.Split(rel, "/") {
		if p == ".git" {
			return "", false, nil
		}
	}
	return filepath.Join(w.root, filepath.FromSlash(rel)), true, nil
}

func (w *writer) file(dst string, r io.Reader) error {
	w.files++
	if w.files > maxFiles {
		return fmt.Errorf("%w: more than %d files", errTooBig, maxFiles)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, maxTotal-w.total+1))
	w.total += n
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && w.total > maxTotal {
		err = fmt.Errorf("%w: more than %d MB unpacked", errTooBig, maxTotal>>20)
	}
	return err
}

// extractTarGz unpacks a gzip-compressed tar, dropping the first strip
// path components of every entry.
func extractTarGz(r io.Reader, root string, strip int) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("not a gzip archive: %w", err)
	}
	defer func() { _ = gz.Close() }()
	return extractTar(gz, root, strip)
}

func extractTar(r io.Reader, root string, strip int) error {
	w := &writer{root: root}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		dst, ok, err := w.target(h.Name, strip)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := w.file(dst, tr); err != nil {
				return err
			}
		}
	}
}

func extractZip(data []byte, root string, strip int) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("not a zip archive: %w", err)
	}
	w := &writer{root: root}
	for _, f := range zr.File {
		dst, ok, err := w.target(f.Name, strip)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return err
			}
		case mode.IsRegular():
			rc, err := f.Open()
			if err != nil {
				return err
			}
			err = w.file(dst, rc)
			rc.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// archiveType names the archive format of data, or "" for a plain file.
func archiveType(name string, data []byte) string {
	lower := strings.ToLower(name)
	switch {
	case bytes.HasPrefix(data, []byte("PK\x03\x04")) || strings.HasSuffix(lower, ".zip"):
		return "zip"
	case bytes.HasPrefix(data, []byte{0x1f, 0x8b}) || strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"):
		return "tar.gz"
	case strings.HasSuffix(lower, ".tar"):
		return "tar"
	}
	return ""
}

// unpack writes data under root: an archive is extracted, anything else is
// saved as the file single. It refuses binary single files.
func unpack(name string, data []byte, root, single string) error {
	switch archiveType(name, data) {
	case "zip":
		return extractZip(data, root, 0)
	case "tar.gz":
		return extractTarGz(bytes.NewReader(data), root, 0)
	case "tar":
		return extractTar(bytes.NewReader(data), root, 0)
	}
	if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		return fmt.Errorf("%s is neither an archive (.zip, .tar.gz) nor a text file", name)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, single), data, 0o600)
}

// unwrap descends through folders that are the only entry of their parent
// (the usual top folder of an archive), unless the folder holds marker.
func unwrap(root, marker string) string {
	for range 4 {
		if _, err := os.Stat(filepath.Join(root, marker)); err == nil {
			return root
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 1 || !entries[0].IsDir() {
			return root
		}
		root = filepath.Join(root, entries[0].Name())
	}
	return root
}

// copyTree copies the folders and regular files under src to dst with the
// same limits as an archive. Symlinks and .git are skipped.
func copyTree(src, dst string) error {
	w := &writer{root: dst}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case d.Type().IsRegular():
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			return w.file(target, f)
		}
		return nil
	})
}
