//go:build unix

package store

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A service started without UMask= runs with a loose umask; backups hold
// credentials and must stay owner-only anyway.
func TestBackupIsOwnerOnlyWhateverTheUmask(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(ctx, filepath.Join(dir, "live.db"), []Migration{{Version: 1, SQL: "CREATE TABLE creds (k TEXT)"}})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backups := filepath.Join(dir, "backups")
	if err := os.Mkdir(backups, 0o755); err != nil { // made loose by an older release
		t.Fatal(err)
	}
	path, err := Backup(ctx, db, backups, "wa", DefaultRetention)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v, want 0600", st.Mode().Perm())
	}
	if st, _ := os.Stat(backups); st.Mode().Perm() != 0o700 {
		t.Fatalf("backup folder mode %v, want 0700", st.Mode().Perm())
	}
	snap := filepath.Join(dir, "snap.db")
	if err := VacuumInto(ctx, db, snap); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(snap); st.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode %v, want 0600", st.Mode().Perm())
	}
}
