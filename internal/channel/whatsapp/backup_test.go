package whatsapp

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestReconnectsDoNotPileUpBackups(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	backups := filepath.Join(dir, "backups")
	a := New("main", filepath.Join(dir, "whatsapp-main.db"), backups, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := a.open(ctx); err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i := 0; i < 4; i++ { // the connection flaps
		a.backup(ctx, reconnectBackupGap)
	}
	entries, err := os.ReadDir(backups)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d backups after four reconnects within an hour, want 1", len(entries))
	}
}
