package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
)

func TestBackupRotates(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(ctx, filepath.Join(dir, "live.db"), []Migration{{Version: 1, SQL: "CREATE TABLE creds (k TEXT, v TEXT)"}})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "INSERT INTO creds VALUES ('identity', 'secret')"); err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(dir, "backups")
	for i := 0; i < 4; i++ {
		if _, err := Backup(ctx, db, backups, "wa", 2); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond) // names have one-second resolution
	}
	entries, _ := os.ReadDir(backups)
	if len(entries) != 2 {
		t.Fatalf("kept %d backups, want 2", len(entries))
	}
	copyDB, err := Open(ctx, LatestBackup(backups, "wa"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var v string
	if err := copyDB.QueryRowContext(ctx, "SELECT v FROM creds WHERE k = 'identity'").Scan(&v); err != nil || v != "secret" {
		t.Fatalf("backup content: %q %v", v, err)
	}
}

func TestDatabasesAreOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	db, err := Open(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, f := range []string{path, path + "-wal"} {
		st, err := os.Stat(f)
		if err != nil {
			continue
		}
		if platform.HasUnixPermissions && st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", f, st.Mode().Perm())

		}
	}
}
