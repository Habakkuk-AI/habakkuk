package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
)

func TestMemoryHistoryWiring(t *testing.T) {
	ctx := context.Background()
	a := openWithMemory(t, "memory:\n  history_days: 30")
	h := a.Memory.History()
	if h == nil || a.Skills.History != h {
		t.Fatal("memory history not wired")
	}
	if a.Cfg.Memory.HistoryRetention() != 30*24*time.Hour {
		t.Fatalf("retention %v", a.Cfg.Memory.HistoryRetention())
	}
	m := memoryCommands{a.Memory}
	if out, _ := m.MemoryHistory(ctx, "habakkuk"); out != "No memory changes recorded yet." {
		t.Fatalf("empty history: %q", out)
	}
	if _, err := m.Remember(ctx, "habakkuk", "ses_1", "My bike is blue."); err != nil {
		t.Fatal(err)
	}
	// A hand edit of IDENTITY.md is picked up by the loop's scan.
	os.WriteFile(filepath.Join(a.Cfg.AgentDir("habakkuk"), "IDENTITY.md"), []byte("name: Ada\n"), 0o600)
	a.scanMemoryHistory(ctx)

	out, err := m.MemoryHistory(ctx, "habakkuk")
	if err != nil || !strings.Contains(out, "IDENTITY.md created by hand; created on disk") || !strings.Contains(out, "note my-bike-is-blue created by user via /remember") {
		t.Fatalf("history: %q %v", out, err)
	}
	cs, _ := h.List(ctx, history.Filter{Agent: "habakkuk", File: "my-bike-is-blue"})
	if len(cs) != 1 || cs[0].Session != "ses_1" {
		t.Fatalf("%+v", cs)
	}
	out, err = m.UndoMemory(ctx, "habakkuk", "ses_2", cs[0].ID)
	if err != nil || !strings.Contains(out, "Removed note my-bike-is-blue") {
		t.Fatalf("undo: %q %v", out, err)
	}
	if _, err := a.Memory.Read(ctx, "habakkuk", "my-bike-is-blue"); err == nil {
		t.Fatal("note still there")
	}
	undo, _ := h.List(ctx, history.Filter{Agent: "habakkuk", Limit: 1})
	if undo[0].Tool != "/undo-memory" || undo[0].Session != "ses_2" || undo[0].Restores != cs[0].ID {
		t.Fatalf("undo change %+v", undo[0])
	}
	if _, err := m.UndoMemory(ctx, "other", "ses_2", cs[0].ID); err == nil {
		t.Fatal("undid another agent's change")
	}
	// Maintenance prunes by memory.history_days and keeps live files' latest.
	if err := a.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(must(h.List(ctx, history.Filter{Agent: "habakkuk"}))); n != 3 {
		t.Fatalf("%d changes after maintenance", n)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
