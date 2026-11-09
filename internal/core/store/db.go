// Package store opens Habakkuk's SQLite databases and its blob directory.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Migration is one forward-only schema step. Version numbers start at 1 and
// must be contiguous within a database.
type Migration struct {
	Version int
	SQL     string
}

// Open opens (creating if needed) a SQLite database in WAL mode and applies
// any pending migrations. Writes use BEGIN IMMEDIATE so concurrent writers
// queue on the busy timeout instead of failing with SQLITE_BUSY mid-transaction.
func Open(ctx context.Context, path string, migrations []Migration) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer; a small pool keeps reads concurrent under WAL.
	db.SetMaxOpenConns(8)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// Databases hold transcripts and channel credentials: owner-only. SQLite
	// gives the WAL and shared-memory files the main file's mode.
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(f, 0o600); err != nil && !os.IsNotExist(err) {
			db.Close()
			return nil, err
		}
	}
	if err := migrate(ctx, db, path, migrations); err != nil {
		db.Close()
		if tooNew := (*SchemaTooNewError)(nil); errors.As(err, &tooNew) {
			return nil, err
		}
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return db, nil
}

// SchemaTooNewError means a database was migrated by a newer Habakkuk than
// the one opening it. Opening it anyway could corrupt data the newer schema
// relies on, so the older binary refuses to start.
type SchemaTooNewError struct {
	Path       string
	Have, Know int // the database's schema version and the newest this binary knows
}

func (e *SchemaTooNewError) Error() string {
	return fmt.Sprintf("%s has schema version %d, but this habakkuk only knows up to version %d: "+
		"a newer Habakkuk has already upgraded it. Roll forward to that release (habakkuk update), "+
		"or restore the backup taken before the upgrade (habakkuk update --rollback, or habakkuk restore -force FILE)",
		filepath.Base(e.Path), e.Have, e.Know)
}

// LatestVersion is the newest schema version in migrations (0 for none).
func LatestVersion(migrations []Migration) int {
	v := 0
	for _, m := range migrations {
		v = max(v, m.Version)
	}
	return v
}

// SchemaVersion reads a database's schema version (PRAGMA user_version)
// without creating, migrating or writing to it. A missing file is version 0.
func SchemaVersion(ctx context.Context, path string) (int, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	q := url.Values{}
	q.Add("mode", "ro")
	q.Add("_pragma", "busy_timeout(10000)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return v, nil
}

// CheckSchema returns a *SchemaTooNewError when the database at path is
// newer than migrations know; a missing file is fine.
func CheckSchema(ctx context.Context, path string, migrations []Migration) error {
	have, err := SchemaVersion(ctx, path)
	if err != nil {
		return err
	}
	if know := LatestVersion(migrations); have > know {
		return &SchemaTooNewError{Path: path, Have: have, Know: know}
	}
	return nil
}

// migrate applies pending migrations, forward only, each in its own
// BEGIN IMMEDIATE transaction together with its user_version bump, so a
// failed step leaves the database at the previous version and two processes
// starting at once never run the same step twice. Opening with nil
// migrations (a snapshot, a database whose schema another library owns)
// skips the checks.
func migrate(ctx context.Context, db *sql.DB, path string, migrations []Migration) error {
	if len(migrations) == 0 {
		return nil
	}
	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return err
	}
	if know := LatestVersion(migrations); current > know {
		return &SchemaTooNewError{Path: path, Have: current, Know: know}
	}
	for _, m := range migrations {
		if m.Version <= current {
			continue
		}
		if m.Version != current+1 {
			return fmt.Errorf("migration %d follows %d: versions must be contiguous", m.Version, current)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		// Re-read inside the write lock: another process may have just
		// applied this step.
		if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
			tx.Rollback()
			return err
		}
		if current >= m.Version {
			tx.Rollback()
			continue
		}
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", m.Version, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.Version)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		current = m.Version
	}
	return nil
}
