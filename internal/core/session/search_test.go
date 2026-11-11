package session

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

func TestSearchFindsPastMessages(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "transcripts.db"), Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewStore(db)
	old, _ := s.Create(ctx, "habakkuk", "sys")
	other, _ := s.Create(ctx, "scout", "sys")
	cur, _ := s.Create(ctx, "habakkuk", "sys")
	s.SetTitle(ctx, old.ID, "Trip planning")
	msgs := []llm.Message{
		llm.UserText("Book the flights to Lisbon for the conference"),
		{Role: llm.Assistant, Blocks: []llm.Block{
			{Type: llm.Text, Text: "Booked TAP flight TP1234 to Lisbon."},
			{Type: llm.ToolUse, ToolUseID: "c1", ToolName: "exec", Input: json.RawMessage(`{"command":"secret"}`)},
		}},
		{Role: llm.User, Blocks: []llm.Block{{Type: llm.ToolResult, ToolUseID: "c1", Text: "tool output"}}},
	}
	for i, m := range msgs {
		if err := s.Append(ctx, old.ID, "j", i, m, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	s.Append(ctx, other.ID, "j", 0, llm.UserText("Lisbon for scout"), "", nil)
	s.Append(ctx, cur.ID, "j", 0, llm.UserText("when do I fly to Lisbon?"), "", nil)

	hits, err := s.Search(ctx, "habakkuk", "lisbon flights", cur.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %+v", hits)
	}
	for _, h := range hits {
		if h.Session != old.ID || h.Title != "Trip planning" || !strings.Contains(h.Snippet, "[Lisbon]") {
			t.Fatalf("bad hit %+v", h)
		}
	}
	if hits, _ := s.Search(ctx, "habakkuk", "secret output", cur.ID, 10); len(hits) != 0 {
		t.Fatalf("tool calls and results are indexed: %+v", hits)
	}
	if _, err := s.Search(ctx, "habakkuk", "  !! ", "", 10); err == nil {
		t.Fatal("empty query accepted")
	}
	if _, err := s.Search(ctx, "habakkuk", `"unbalanced AND (`, "", 10); err != nil {
		t.Fatalf("query syntax leaked into FTS: %v", err)
	}
	ex, err := s.Around(ctx, "habakkuk", old.ID, 1, 3)
	if err != nil || len(ex) != 2 || ex[1].Role != "assistant" {
		t.Fatalf("around = %+v, %v", ex, err)
	}
	if _, err := s.Around(ctx, "scout", old.ID, 1, 3); err == nil {
		t.Fatal("read another agent's session")
	}

	// The tool searches the calling agent's other sessions.
	reg := tools.NewRegistry()
	RegisterTools(reg, s)
	call := func(input string) llm.Block {
		return reg.Execute(ctx, &tools.Env{Agent: "habakkuk", Session: cur.ID, Mode: tools.ModeReadOnly},
			llm.Block{Type: llm.ToolUse, ToolUseID: "x", ToolName: "session_search", Input: json.RawMessage(input)})
	}
	if res := call(`{"query":"fly"}`); res.IsError || res.Text != "no matches" {
		t.Fatalf("tool = %+v", res)
	}
	if res := call(`{"query":"TAP"}`); res.IsError || !strings.Contains(res.Text, old.ID+" #1") {
		t.Fatalf("tool = %+v", res)
	}
	if res := call(`{"session":"` + old.ID + `","seq":1}`); res.IsError || !strings.Contains(res.Text, "#0 user") {
		t.Fatalf("tool around = %+v", res)
	}

	// Pruning a session drops its index rows too.
	if _, err := db.ExecContext(ctx, `UPDATE sessions SET updated_at = 0 WHERE id = ?`, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prune(ctx, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if hits, _ := s.Search(ctx, "habakkuk", "lisbon", cur.ID, 10); len(hits) != 0 {
		t.Fatalf("pruned session still found: %+v", hits)
	}
	var n int
	db.QueryRowContext(ctx, `SELECT count(*) FROM message_text WHERE session_id = ?`, old.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d index rows left behind", n)
	}
}

// A database written before the search index existed gets its messages
// indexed by the migration.
func TestSearchMigrationIndexesExistingMessages(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "transcripts.db")
	db, err := store.Open(ctx, path, Migrations[:2])
	if err != nil {
		t.Fatal(err)
	}
	// Written as that older binary wrote them: the Store of today expects
	// the latest schema.
	if _, err := db.ExecContext(ctx, `INSERT INTO sessions (id, agent, system_prompt, created_at, updated_at) VALUES ('s1', 'habakkuk', 'sys', 1, 1);
		INSERT INTO messages (session_id, seq, role, blocks, created_at) VALUES
			('s1', 0, 'user', '[{"type":"text","text":"remember the walrus"}]', 1),
			('s1', 1, 'assistant', '[{"type":"text","text":"noted"},{"type":"text","text":"the walrus again"}]', 1);`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = store.Open(ctx, path, Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hits, err := NewStore(db).Search(ctx, "habakkuk", "walrus", "", 5)
	if err != nil || len(hits) != 2 {
		t.Fatalf("hits = %+v, %v", hits, err)
	}
	var v int
	db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	if v != len(Migrations) {
		t.Fatalf("user_version = %d", v)
	}
}
