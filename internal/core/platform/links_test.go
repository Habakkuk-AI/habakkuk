package platform

import (
	"os"
	"path/filepath"
	"testing"
)

// Links counts a file's names on every system (GetFileInformationByHandle
// on Windows), follows symlinks, and fails for a file it can't read.
func TestLinks(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "SOUL.md")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := Links(f); err != nil || n != 1 {
		t.Fatalf("one name: %d, %v", n, err)
	}
	if err := os.Link(f, filepath.Join(dir, "notes.md")); err != nil {
		t.Skip("no hard links here:", err)
	}
	if n, err := Links(f); err != nil || n != 2 {
		t.Fatalf("two names: %d, %v", n, err)
	}
	if err := os.Symlink(f, filepath.Join(dir, "link.md")); err == nil {
		if n, err := Links(filepath.Join(dir, "link.md")); err != nil || n != 2 {
			t.Fatalf("through a symlink: %d, %v", n, err)
		}
	}
	if _, err := Links(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("no error for a missing file")
	}
}
