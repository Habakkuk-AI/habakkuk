package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
)

// MinFreeForBackup is the free space a backup leaves untouched. A backup that
// would fill the disk is skipped: a full disk is how state gets corrupted.
const MinFreeForBackup = 256 << 20

// Retention says which backups Backup keeps: the newest Recent copies, plus
// the newest copy of each of the last Days days. The newest good copy is
// always kept.
type Retention struct {
	Recent int
	Days   int
}

// DefaultRetention keeps five recent copies and one a day for a week.
var DefaultRetention = Retention{Recent: 5, Days: 7}

const backupStamp = "20060102-150405"

// Backup writes a consistent copy of a live SQLite database to
// dir/<prefix>-<YYYYMMDD-HHMMSS>.db (VACUUM INTO, fsync, rename) and prunes
// older copies by keep. Copies hold credentials, so they are owner-only
// (0600, in a 0700 folder) whatever the process umask. The live database is
// never modified.
func Backup(ctx context.Context, db *sql.DB, dir, prefix string, keep Retention) (string, error) {
	if err := PrivateDir(dir); err != nil {
		return "", err
	}
	var pages, pageSize int64
	if err := db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return "", err
	}
	if err := db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return "", err
	}
	free, err := FreeBytes(dir)
	if err != nil {
		return "", err
	}
	if need := pages*pageSize + MinFreeForBackup; free < uint64(need) {
		return "", fmt.Errorf("backup skipped: %d MB free, need %d MB", free>>20, need>>20)
	}
	final := filepath.Join(dir, fmt.Sprintf("%s-%s.db", prefix, time.Now().UTC().Format(backupStamp)))
	// A random temp name: SQLite opens it itself, and a fixed name could
	// have a link put there between a remove and the open.
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	tmp := final + "." + hex.EncodeToString(rnd[:]) + ".tmp"
	if err := VacuumInto(ctx, db, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("backup: %w", err)
	}
	// Read-write: Windows only flushes handles opened for writing.
	f, err := os.OpenFile(tmp, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	err = f.Sync()
	f.Close()
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if !goodBackup(tmp) {
		_ = os.Remove(tmp)
		return "", errors.New("backup: the copy is not a SQLite database")
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := platform.SyncDir(dir); err != nil {
		return "", err
	}
	return final, prune(dir, prefix, keep, time.Now())
}

// VacuumInto writes a consistent copy of db to path, which must not exist.
// The file is created owner-only first: SQLite would otherwise create it
// with the process umask, which may let others read it.
func VacuumInto(ctx context.Context, db *sql.DB, path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// PrivateDir creates dir as owner-only (0700), and tightens it if it
// already exists with looser permissions.
func PrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if !platform.HasUnixPermissions {
		return nil
	}
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, st.Mode().Perm()&0o700)
	}
	return nil
}

type backupFile struct {
	path string
	at   time.Time
}

// backups lists dir's backups for prefix, newest first. Only names Backup
// writes count, so another prefix that starts the same way (an account
// "main-2" next to "main") is never touched.
func backups(dir, prefix string) []backupFile {
	entries, _ := os.ReadDir(dir)
	var out []backupFile
	for _, e := range entries {
		stamp, ok := strings.CutPrefix(e.Name(), prefix+"-")
		if !ok || e.IsDir() {
			continue
		}
		stamp, ok = strings.CutSuffix(stamp, ".db")
		if !ok {
			continue
		}
		at, err := time.Parse(backupStamp, stamp)
		if err != nil {
			continue
		}
		out = append(out, backupFile{path: filepath.Join(dir, e.Name()), at: at})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.After(out[j].at) })
	return out
}

// goodBackup reports whether path holds a SQLite database (a non-empty
// file with SQLite's header).
func goodBackup(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 16)
	if _, err := io.ReadFull(f, head); err != nil {
		return false
	}
	return bytes.Equal(head, []byte("SQLite format 3\x00"))
}

// prune deletes the good backups keep does not cover. Files that are not
// good copies are left alone, and the newest good copy always stays.
func prune(dir, prefix string, keep Retention, now time.Time) error {
	cutoff := now.UTC().AddDate(0, 0, -keep.Days)
	days := map[string]bool{}
	good := 0
	for _, b := range backups(dir, prefix) {
		if !goodBackup(b.path) {
			continue
		}
		good++
		day := b.at.Format("20060102")
		switch {
		case good == 1, good <= keep.Recent:
			days[day] = true
			continue
		case keep.Days > 0 && b.at.After(cutoff) && !days[day]:
			days[day] = true // the newest copy of its day
			continue
		}
		if err := os.Remove(b.path); err != nil {
			return err
		}
	}
	return nil
}

// LatestBackup returns the newest backup for prefix, or "".
func LatestBackup(dir, prefix string) string {
	if b := backups(dir, prefix); len(b) > 0 {
		return b[0].path
	}
	return ""
}

// LatestBackupTime returns when the newest backup for prefix was taken, or
// the zero time.
func LatestBackupTime(dir, prefix string) time.Time {
	if b := backups(dir, prefix); len(b) > 0 {
		return b[0].at
	}
	return time.Time{}
}

// FreeBytes reports the space available to unprivileged users at path.
func FreeBytes(path string) (uint64, error) {
	free, _, err := platform.DiskUsage(path)
	return free, err
}
