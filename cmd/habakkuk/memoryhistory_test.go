package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
)

func TestMemoryHistoryCLI(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	if err := store.WriteFileAtomic(filepath.Join(home, "config.yaml"), []byte(config.Template), 0o600); err != nil {
		t.Fatal(err)
	}
	// Two versions of a note, written as the server would.
	mem, err := memory.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	h, err := history.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	mem.UseHistory(h)
	mem.Save(ctx, "habakkuk", "Coffee", "black", nil)
	mem.Save(ctx, "habakkuk", "Coffee", "planted", nil)
	cs, _ := h.List(ctx, history.Filter{})
	mem.Close()
	h.Close()
	id := cs[0].ID

	for _, args := range [][]string{
		{"history"},
		{"history", "-agent", "habakkuk", "-file", "coffee", "-since", "7d"},
		{"diff", id},
		{"restore", id},
		{"pin", "coffee"},
		{"pin", "profile", "-agent", "habakkuk"},
		{"pins"},
		{"unpin", "profile"},
	} {
		if err := memoryCmd(ctx, home, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"diff"},
		{"restore", "zz"},
		{"pin", "no-such-note"},
		{"pin", "skill:nope"},
		{"history", "-since", "soon"},
	} {
		if err := memoryCmd(ctx, home, args); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}

	mem, _ = memory.Open(ctx, home)
	defer mem.Close()
	h, _ = history.Open(ctx, home)
	defer h.Close()
	if m, _ := mem.Read(ctx, "habakkuk", "coffee"); m.Body != "black" {
		t.Fatalf("restore from the CLI: %q", m.Body)
	}
	latest, _ := h.List(ctx, history.Filter{Limit: 1})
	if latest[0].By != history.ByUser || latest[0].Tool != "cli" || !strings.Contains(latest[0].Note, id) {
		t.Fatalf("%+v", latest[0])
	}
	pins, _ := h.Pins(ctx, "habakkuk")
	if len(pins) != 1 || pins[0].Name != "coffee" {
		t.Fatalf("pins %+v", pins)
	}
}
