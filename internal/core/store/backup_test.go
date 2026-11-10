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
		if _, err := Backup(ctx, db, backups, "wa", Retention{Recent: 2}); err != nil {
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

func TestPruneKeepsRecentAndOnePerDay(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 9, 18, 0, 0, 0, time.UTC)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	good := "SQLite format 3\x00 rest of the page"
	// Eight reconnects today, then one or two copies on earlier days.
	for i := 0; i < 8; i++ {
		write("wa-20261109-1"+string(rune('0'+i))+"0000.db", good)
	}
	for _, name := range []string{
		"wa-20261108-230000.db", "wa-20261108-080000.db", // the 23:00 one is that day's copy
		"wa-20261105-120000.db",
		"wa-20261101-120000.db",   // older than a week
		"wa-2-20261101-120000.db", // another account whose name starts the same way
		"wa-notes.db",
	} {
		write(name, good)
	}
	write("wa-20261107-120000.db", "") // not a good copy: left alone, not counted
	if err := prune(dir, "wa", Retention{Recent: 5, Days: 7}, now); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"wa-20261109-170000.db": true, "wa-20261109-160000.db": true, "wa-20261109-150000.db": true,
		"wa-20261109-140000.db": true, "wa-20261109-130000.db": true,
		"wa-20261108-230000.db": true, "wa-20261105-120000.db": true,
		"wa-2-20261101-120000.db": true, "wa-notes.db": true, "wa-20261107-120000.db": true,
	}
	entries, _ := os.ReadDir(dir)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
		if !want[e.Name()] {
			t.Errorf("kept %s", e.Name())
		}
	}
	if len(got) != len(want) {
		t.Fatalf("kept %v", got)
	}

	// The only good copy survives even a retention that keeps nothing.
	only := t.TempDir()
	if err := os.WriteFile(filepath.Join(only, "wa-20260101-000000.db"), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prune(only, "wa", Retention{}, now); err != nil {
		t.Fatal(err)
	}
	if LatestBackup(only, "wa") == "" {
		t.Fatal("deleted the only good copy")
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
