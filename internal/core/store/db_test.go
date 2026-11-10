package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

var testMigrations = []Migration{
	{Version: 1, SQL: `CREATE TABLE a (id INTEGER PRIMARY KEY);`},
	{Version: 2, SQL: `CREATE TABLE b (id INTEGER PRIMARY KEY); INSERT INTO a (id) VALUES (1);`},
}

func TestMigrationsForwardOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "x.db")
	db, err := Open(ctx, path, testMigrations[:1])
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if v, _ := SchemaVersion(ctx, path); v != 1 {
		t.Fatalf("version %d after the first migration", v)
	}
	// A newer binary adds a step: it runs once.
	for range 2 {
		db, err = Open(ctx, path, testMigrations)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err := db.QueryRow("SELECT count(*) FROM a").Scan(&n); err != nil || n != 1 {
			t.Fatalf("rows %d %v: step 2 ran %d times", n, err, n)
		}
		db.Close()
	}
	if err := CheckSchema(ctx, path, testMigrations); err != nil {
		t.Fatal(err)
	}
}

// A failing step leaves the database at the previous version, with none of
// the step's changes.
func TestMigrationRollsBackOnFailure(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "x.db")
	bad := append(testMigrations[:1:1], Migration{Version: 2, SQL: `CREATE TABLE b (id INTEGER); INSERT INTO nope VALUES (1);`})
	if _, err := Open(ctx, path, bad); err == nil || !strings.Contains(err.Error(), "migration 2") {
		t.Fatalf("err = %v", err)
	}
	if v, _ := SchemaVersion(ctx, path); v != 1 {
		t.Fatalf("version %d after a failed step, want 1", v)
	}
	db, err := Open(ctx, path, testMigrations)
	if err != nil {
		t.Fatalf("the fixed step should apply cleanly: %v", err)
	}
	db.Close()
}

// An older binary refuses a database a newer one migrated, with a message
// that says what to do.
func TestOlderBinaryRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "control.db")
	db, err := Open(ctx, path, testMigrations)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err = Open(ctx, path, testMigrations[:1])
	var tooNew *SchemaTooNewError
	if !errors.As(err, &tooNew) || tooNew.Have != 2 || tooNew.Know != 1 {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"control.db", "schema version 2", "up to version 1", "hk update", "--rollback"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err, want)
		}
	}
	if err := CheckSchema(ctx, path, testMigrations[:1]); !errors.As(err, &tooNew) {
		t.Fatalf("CheckSchema: %v", err)
	}
	// Snapshots and foreign schemas open with no migrations and are not checked.
	db, err = Open(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if v, err := SchemaVersion(ctx, filepath.Join(t.TempDir(), "missing.db")); v != 0 || err != nil {
		t.Fatalf("missing file: %d %v", v, err)
	}
}
