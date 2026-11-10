package migrate

import (
	"database/sql"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
)

// openSourceDB opens a SQLite database in a source home for reading
// without writing anything next to it. Opened read-only, a database in WAL
// mode still gets -wal and -shm files beside it, so it is opened as
// immutable instead. When a -wal file is there already, it holds changes
// the main file lacks, and immutable would miss them: the database and its
// -wal file are copied to a temporary folder and the copy is read. close
// closes the database and removes any copy.
func openSourceDB(path string) (db *sql.DB, closeFn func() error, err error) {
	if !exists(path + "-wal") {
		q := url.Values{}
		q.Add("mode", "ro")
		q.Add("immutable", "1")
		if db, err = sql.Open("sqlite", "file:"+path+"?"+q.Encode()); err != nil {
			return nil, nil, err
		}
		db.SetMaxOpenConns(1)
		return db, db.Close, nil
	}
	dir, err := os.MkdirTemp("", "hk-migrate-db-")
	if err != nil {
		return nil, nil, err
	}
	copy := filepath.Join(dir, filepath.Base(path))
	for _, suffix := range []string{"", "-wal"} {
		if err := copyFile(path+suffix, copy+suffix); err != nil {
			os.RemoveAll(dir)
			return nil, nil, err
		}
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	if db, err = sql.Open("sqlite", "file:"+copy+"?"+q.Encode()); err != nil {
		os.RemoveAll(dir)
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	return db, func() error { return errors.Join(db.Close(), os.RemoveAll(dir)) }, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
