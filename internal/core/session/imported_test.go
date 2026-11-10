package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

func TestImportedSessions(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "transcripts.db"), Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewStore(db)
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	im := Import{
		Source: "other", SourceID: "main/abc", Agent: "habakkuk", Title: "Sunday   hours", Started: start,
		Account: "telegram", Conversation: "12025550142",
		Messages: []ImportedMessage{
			{Role: llm.User, Text: "When does Corner Bakery open on Sundays?", At: start.Add(time.Minute)},
			{Role: "tool", Text: "left out"},
			{Role: llm.Assistant, Text: "  "},
			{Role: llm.Assistant, Text: "At nine.", At: start.Add(2 * time.Minute)},
		},
	}
	id, added, err := s.Import(ctx, im)
	if err != nil || !added || id != ImportID("other", "main/abc") {
		t.Fatalf("Import = %q, %v, %v", id, added, err)
	}
	if again, added, err := s.Import(ctx, im); err != nil || added || again != id {
		t.Fatalf("second Import = %q, %v, %v; want the same session, not added", again, added, err)
	}
	sess, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Title != "Sunday hours" || sess.TitleSource != TitleImported || !sess.CreatedAt.Equal(start) ||
		!sess.UpdatedAt.Equal(start.Add(2*time.Minute)) {
		t.Errorf("session = %+v", sess)
	}
	msgs, err := s.Messages(ctx, id)
	if err != nil || len(msgs) != 2 || msgs[0].Message.Role != llm.User || msgs[1].Message.Text() != "At nine." {
		t.Fatalf("messages = %+v, %v", msgs, err)
	}
	hits, err := s.Search(ctx, "habakkuk", "bakery sundays", "", 5)
	if err != nil || len(hits) != 1 || hits[0].Session != id {
		t.Errorf("search = %+v, %v; imported messages must be searchable", hits, err)
	}
	o, err := s.ImportOrigin(ctx, id)
	if err != nil || o == nil || o.Source != "other" || o.Account != "telegram" || o.Conversation != "12025550142" {
		t.Errorf("origin = %+v, %v", o, err)
	}
	if ok, err := s.Imported(ctx, "other", "main/abc"); err != nil || !ok {
		t.Errorf("Imported = %v, %v", ok, err)
	}

	// Read-only and kept: no fork, and retention leaves it.
	if _, err := s.Fork(ctx, id, 1, ""); !errors.Is(err, ErrImported) {
		t.Errorf("Fork = %v, want ErrImported", err)
	}
	own, _ := s.Create(ctx, "habakkuk", "sys")
	n, err := s.Prune(ctx, time.Now().Add(time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("Prune = %d, %v; want only the session the user had here", n, err)
	}
	if _, err := s.Get(ctx, own.ID); err == nil {
		t.Error("the ordinary session was not pruned")
	}
	if _, err := s.Get(ctx, id); err != nil {
		t.Errorf("the imported session was pruned: %v", err)
	}
	if o, _ := s.ImportOrigin(ctx, own.ID); o != nil {
		t.Errorf("an ordinary session has an origin: %+v", o)
	}
	if err := s.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Imported(ctx, "other", "main/abc"); ok {
		t.Error("deleting the session left its import record")
	}
}
