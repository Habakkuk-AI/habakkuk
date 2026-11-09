package store

import (
	"context"
	"database/sql"
	"fmt"
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

// Backup writes a consistent copy of a live SQLite database to
// dir/<prefix>-<YYYYMMDD-HHMMSS>.db (VACUUM INTO, fsync, rename) and keeps
// the newest keep copies. The live database is never modified.
func Backup(ctx context.Context, db *sql.DB, dir, prefix string, keep int) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
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
	final := filepath.Join(dir, fmt.Sprintf("%s-%s.db", prefix, time.Now().UTC().Format("20060102-150405")))
	tmp := final + ".tmp"
	_ = os.Remove(tmp)
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
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
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := platform.SyncDir(dir); err != nil {
		return "", err
	}
	return final, rotate(dir, prefix, keep)
}

func rotate(dir, prefix string, keep int) error {
	matches, err := filepath.Glob(filepath.Join(dir, prefix+"-*.db"))
	if err != nil {
		return err
	}
	sort.Strings(matches) // timestamps sort chronologically
	for len(matches) > keep {
		if err := os.Remove(matches[0]); err != nil {
			return err
		}
		matches = matches[1:]
	}
	return nil
}

// LatestBackup returns the newest backup for prefix, or "".
func LatestBackup(dir, prefix string) string {
	matches, _ := filepath.Glob(filepath.Join(dir, prefix+"-*.db"))
	sort.Strings(matches)
	for i := len(matches) - 1; i >= 0; i-- {
		if !strings.HasSuffix(matches[i], ".tmp") {
			return matches[i]
		}
	}
	return ""
}

// FreeBytes reports the space available to unprivileged users at path.
func FreeBytes(path string) (uint64, error) {
	free, _, err := platform.DiskUsage(path)
	return free, err
}
