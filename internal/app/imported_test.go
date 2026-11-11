package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// An imported conversation is untrusted wherever trust is decided: memory
// review and a turn's starting taint both ask untrustedSession.
func TestImportedSessionIsUntrusted(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "transcripts.db"), session.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &App{Sessions: session.NewStore(db)}
	id, _, err := a.Sessions.Import(ctx, session.Import{Source: "other", SourceID: "a/1", Agent: "habakkuk",
		Messages: []session.ImportedMessage{{Role: llm.User, Text: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	own, _ := a.Sessions.Create(ctx, "habakkuk", "sys")
	if !a.untrustedSession(ctx, id) {
		t.Error("an imported session counts as trusted")
	}
	if a.untrustedSession(ctx, own.ID) {
		t.Error("an ordinary session counts as untrusted")
	}
}
