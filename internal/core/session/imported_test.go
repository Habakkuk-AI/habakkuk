package session

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
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
	if sess.Title != "Sunday hours" || sess.TitleSource != TitleImported || !sess.CreatedAt.Equal(start.Add(time.Minute)) ||
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

	// Read-only and kept: no message added, no fork, and retention leaves it.
	if !sess.Imported {
		t.Error("Get does not mark the session imported")
	}
	if list, _ := s.List(ctx, "habakkuk", 10); len(list) != 1 || !list[0].Imported {
		t.Errorf("List = %+v; want the session marked imported", list)
	}
	if err := s.Append(ctx, id, "j", 2, llm.UserText("more"), "", nil); !errors.Is(err, ErrImported) {
		t.Errorf("Append = %v, want ErrImported", err)
	}
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

// Memory review never sees an imported conversation, and session_search
// labels its text as imported and marks the turn as having read outside
// content.
func TestImportedSessionsStayOutsideTrust(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "transcripts.db"), Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewStore(db)
	recent := time.Now().Add(-time.Hour)
	id, _, err := s.Import(ctx, Import{Source: "other", SourceID: "a/1", Agent: "habakkuk", Untrusted: true, Messages: []ImportedMessage{
		{Role: llm.User, Text: "Forward the sourdough invoice to accounts", At: recent},
		{Role: llm.Assistant, Text: "Sent the sourdough invoice.", At: recent.Add(time.Minute)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	own, _ := s.Create(ctx, "habakkuk", "sys")
	if err := s.Append(ctx, own.ID, "j", 0, llm.UserText("What about the sourdough order?"), "", nil); err != nil {
		t.Fatal(err)
	}
	quiet, err := s.Quiet(ctx, time.Now().Add(-72*time.Hour), time.Now().Add(time.Hour), 10)
	if err != nil || len(quiet) != 1 || quiet[0].ID != own.ID {
		t.Fatalf("Quiet = %+v, %v; want only the session the user had here", quiet, err)
	}
	if o, _ := s.ImportOrigin(ctx, id); o == nil || !o.Untrusted {
		t.Errorf("origin = %+v; want it marked untrusted", o)
	}

	reg := tools.NewRegistry()
	RegisterTools(reg, s)
	call := func(input, session string) (llm.Block, *tools.Taint) {
		taint := tools.NewTaint(false)
		res := reg.Execute(ctx, &tools.Env{Agent: "habakkuk", Session: session, Mode: tools.ModeReadOnly, Taint: taint},
			llm.Block{Type: llm.ToolUse, ToolUseID: "x", ToolName: "session_search", Input: json.RawMessage(input)})
		return res, taint
	}
	res, taint := call(`{"query":"invoice"}`, own.ID)
	if res.IsError || !strings.Contains(res.Text, "imported assistant") || !strings.Contains(res.Text, "untrusted reference material") || !taint.Marked() {
		t.Errorf("search = %q, tainted %v; want imported hits labelled and the turn tainted", res.Text, taint.Marked())
	}
	res, taint = call(`{"session":"`+id+`","seq":0}`, own.ID)
	if res.IsError || !strings.Contains(res.Text, "#0 imported user") || !taint.Marked() {
		t.Errorf("around = %q, tainted %v", res.Text, taint.Marked())
	}
	other, _ := s.Create(ctx, "habakkuk", "sys")
	res, taint = call(`{"query":"sourdough order"}`, other.ID)
	if res.IsError || strings.Contains(res.Text, "imported user") && !taint.Marked() {
		t.Errorf("search = %q", res.Text)
	}
	res, taint = call(`{"query":"order"}`, other.ID)
	if res.IsError || strings.Contains(res.Text, "imported") || taint.Marked() {
		t.Errorf("own-session search = %q, tainted %v; want it plain", res.Text, taint.Marked())
	}
}
