package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

func TestSessionsSubcommands(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "transcripts.db"), session.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := session.NewStore(db)
	a, _ := s.Create(ctx, "habakkuk", "sys")
	b, _ := s.Create(ctx, "helper", "sys")
	s.Append(ctx, a.ID, "", 0, llm.UserText("where did I park the bicycle"), "", nil)
	s.Append(ctx, b.ID, "", 0, llm.UserText("bicycle repair shops"), "", nil)

	do := func(stdin string, args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := runSessionVerb(ctx, s, args, &out, strings.NewReader(stdin)); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.String()
	}
	if got := do("", "search", "bicycle"); !strings.Contains(got, a.ID) || !strings.Contains(got, b.ID) {
		t.Fatalf("search all:\n%s", got)
	}
	if got := do("", "search", "-agent", "helper", "bicycle"); strings.Contains(got, a.ID) {
		t.Fatalf("search one agent:\n%s", got)
	}
	if got := do("", "export", a.ID, "-format", "json"); !strings.Contains(got, `"id": "`+a.ID+`"`) {
		t.Fatalf("export json:\n%s", got)
	}
	path := filepath.Join(dir, "out.html")
	do("", "export", a.ID, "-format=html", "-o", path)
	if page, _ := os.ReadFile(path); !strings.Contains(string(page), "<!doctype html>") {
		t.Fatalf("export html: %s", page)
	}
	do("", "archive", a.ID)
	if got := do("", "archived"); !strings.Contains(got, a.ID) {
		t.Fatalf("archived:\n%s", got)
	}
	do("", "unarchive", a.ID)
	if got := do("n\n", "rm", b.ID); !strings.Contains(got, "Kept.") {
		t.Fatalf("rm declined:\n%s", got)
	}
	do("", "rm", "-y", b.ID)
	if _, err := s.Get(ctx, b.ID); err == nil {
		t.Fatal("session not deleted")
	}
	var out bytes.Buffer
	if err := runSessionVerb(ctx, s, []string{"export", a.ID, "-format", "pdf"}, &out, nil); err == nil {
		t.Fatal("unknown format accepted")
	}
}
