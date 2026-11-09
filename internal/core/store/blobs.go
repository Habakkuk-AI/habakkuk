package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Blobs is a content-addressed file store for payloads too large to keep
// inline in a database row (big tool outputs, media).
type Blobs struct {
	dir string
}

func NewBlobs(dir string) (*Blobs, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Blobs{dir: dir}, nil
}

// Put stores data and returns its reference ("sha256:<hex>"). Writing the same
// content twice is a no-op. The write is atomic: temp file, fsync, rename.
func (b *Blobs) Put(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	id := hex.EncodeToString(sum[:])
	path := b.path(id)
	if _, err := os.Stat(path); err == nil {
		return "sha256:" + id, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := WriteFileAtomic(path, data, 0o600); err != nil {
		return "", err
	}
	return "sha256:" + id, nil
}

func (b *Blobs) Get(ref string) ([]byte, error) {
	id, ok := strings.CutPrefix(ref, "sha256:")
	if !ok || len(id) != 64 {
		return nil, errors.New("invalid blob ref")
	}
	return os.ReadFile(b.path(id))
}

func (b *Blobs) path(id string) string {
	return filepath.Join(b.dir, id[:2], id)
}

// WriteFileAtomic writes data so that a crash or full disk leaves either the
// old file or the complete new file, never a truncated one.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		return err
	}
	return os.Rename(name, path)
}
