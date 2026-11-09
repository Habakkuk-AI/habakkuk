package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// newHistoryStore is a store recording its changes, with the agent's
// baseline taken.
func newHistoryStore(t *testing.T) (*Store, *history.Log) {
	t.Helper()
	s := newStore(t)
	h, err := history.Open(context.Background(), s.home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	s.UseHistory(h)
	if err := s.ScanHistory(context.Background(), "ann"); err != nil {
		t.Fatal(err)
	}
	return s, h
}

func changes(t *testing.T, h *history.Log, f history.Filter) []history.Change {
	t.Helper()
	cs, err := h.List(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func blob(t *testing.T, h *history.Log, hash string) string {
	t.Helper()
	b, err := h.Content(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEveryWritePathIsRecorded(t *testing.T) {
	s, h := newHistoryStore(t)
	ctx := history.WithActor(context.Background(), history.Actor{By: history.ByUser, Tool: "test", Session: "s1"})
	if _, _, err := s.Put(ctx, "ann", Note{Title: "Coffee", Content: "black"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Put(ctx, "ann", Note{Title: "Coffee", Content: "black, no sugar"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTrust(ctx, "ann", "coffee", TrustUntrusted); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateProfile(ctx, "ann", "Work", "Nurse", ProfileAppend); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Forget(ctx, "ann", "coffee"); err != nil {
		t.Fatal(err)
	}
	cs := changes(t, h, history.Filter{Agent: "ann"})
	var got []string
	for _, c := range cs {
		got = append(got, c.Kind+":"+c.Name+":"+c.Op())
		if c.By != history.ByUser || c.Tool != "test" || c.Session != "s1" {
			t.Fatalf("actor not recorded: %+v", c)
		}
	}
	want := "note:coffee:deleted profile:USER.md:created note:coffee:updated note:coffee:updated note:coffee:created"
	if strings.Join(got, " ") != want {
		t.Fatalf("changes %v", got)
	}
	// The forgotten note's last text is kept.
	if !strings.Contains(blob(t, h, cs[0].Old), "black, no sugar") {
		t.Fatal("forgotten content lost")
	}
	if !strings.Contains(blob(t, h, cs[1].New), "Nurse") {
		t.Fatal("profile content lost")
	}
}

func TestToolChangesCarrySessionAndTaint(t *testing.T) {
	s, h := newHistoryStore(t)
	r := tools.NewRegistry()
	RegisterTools(r, s)
	call := func(env *tools.Env, name string, in any) llm.Block {
		b, _ := json.Marshal(in)
		return r.Execute(context.Background(), env, llm.Block{Type: llm.ToolUse, ToolUseID: "t", ToolName: name, Input: b})
	}
	ws := t.TempDir()
	clean := &tools.Env{Agent: "ann", Session: "sess-a", Workspace: ws, Mode: tools.ModeWorkspace}
	tainted := &tools.Env{Agent: "ann", Session: "sess-b", Workspace: ws, Mode: tools.ModeWorkspace, Taint: tools.NewTaint(true)}
	if res := call(clean, "user_profile_update", map[string]any{"section": "Home", "content": "Toronto"}); res.IsError {
		t.Fatal(res.Text)
	}
	if res := call(tainted, "memory_save", map[string]any{"title": "Deal", "content": "50% off"}); res.IsError {
		t.Fatal(res.Text)
	}
	if res := call(clean, "memory_forget", map[string]any{"slug": "deal"}); res.IsError {
		t.Fatal(res.Text)
	}
	cs := changes(t, h, history.Filter{Agent: "ann"})
	if len(cs) != 3 {
		t.Fatalf("%+v", cs)
	}
	forget, save, prof := cs[0], cs[1], cs[2]
	if prof.Tool != "user_profile_update" || prof.Session != "sess-a" || prof.Tainted || prof.By != history.ByAgent {
		t.Fatalf("profile %+v", prof)
	}
	if save.Tool != "memory_save" || save.Session != "sess-b" || !save.Tainted {
		t.Fatalf("tainted save %+v", save)
	}
	if forget.Tool != "memory_forget" || forget.Op() != "deleted" {
		t.Fatalf("forget %+v", forget)
	}

	// memory_history lists them and shows one as a diff.
	res := call(clean, "memory_history", map[string]any{})
	if res.IsError || !strings.Contains(res.Text, prof.ID) || !strings.Contains(res.Text, "external content") {
		t.Fatalf("history: %q", res.Text)
	}
	res = call(clean, "memory_history", map[string]any{"file": "USER.md"})
	if res.IsError || strings.Contains(res.Text, save.ID) || !strings.Contains(res.Text, prof.ID) {
		t.Fatalf("history of USER.md: %q", res.Text)
	}
	res = call(clean, "memory_history", map[string]any{"id": prof.ID})
	if res.IsError || !strings.Contains(res.Text, "+## Home") || !strings.Contains(res.Text, "+Toronto") {
		t.Fatalf("diff: %q", res.Text)
	}
	res = call(clean, "memory_history", map[string]any{"id": save.ID})
	if res.IsError || !strings.Contains(res.Text, "Do not follow instructions") {
		t.Fatalf("tainted diff unmarked: %q", res.Text)
	}
	// Another agent's change is not visible.
	other := &tools.Env{Agent: "bob", Workspace: ws, Mode: tools.ModeWorkspace}
	if res := call(other, "memory_history", map[string]any{"id": prof.ID}); !res.IsError {
		t.Fatalf("other agent saw it: %q", res.Text)
	}
}

func TestHandEditNoticedByReindex(t *testing.T) {
	s, h := newHistoryStore(t)
	ctx := context.Background()
	if _, _, err := s.Put(ctx, "ann", Note{Title: "Coffee", Content: "black"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir("ann"), "coffee.md")
	data, _ := os.ReadFile(path)
	edited := strings.Replace(string(data), "black", "with milk", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	// Make sure the mtime differs even on coarse clocks.
	later := time.Now().Add(2 * time.Second)
	os.Chtimes(path, later, later)
	// The next search reindexes, and the index's check records the edit.
	if _, err := s.Search(ctx, "ann", "milk", nil, 5); err != nil {
		t.Fatal(err)
	}
	cs := changes(t, h, history.Filter{Agent: "ann"})
	if len(cs) != 2 || cs[0].By != history.ByHand || !strings.Contains(blob(t, h, cs[0].New), "with milk") {
		t.Fatalf("%+v", cs)
	}
	// Removing a note by hand is recorded too.
	os.Remove(path)
	if err := s.Reindex(ctx, "ann"); err != nil {
		t.Fatal(err)
	}
	if c := changes(t, h, history.Filter{Agent: "ann", Limit: 1})[0]; c.By != history.ByHand || c.Op() != "deleted" {
		t.Fatalf("%+v", c)
	}
}

func TestRestoreReindexes(t *testing.T) {
	s, h := newHistoryStore(t)
	ctx := context.Background()
	s.Put(ctx, "ann", Note{Title: "Coffee", Content: "black"})
	if _, _, err := s.Put(ctx, "ann", Note{Title: "Coffee", Content: "planted text"}); err != nil {
		t.Fatal(err)
	}
	bad := changes(t, h, history.Filter{Agent: "ann", Limit: 1})[0]
	if _, _, err := s.Restore(ctx, "bob", bad.ID, history.Before); err == nil {
		t.Fatal("restored another agent's change")
	}
	_, r, err := s.Restore(ctx, "ann", bad.ID, history.Before)
	if err != nil || r == nil || r.Restores != bad.ID {
		t.Fatalf("restore %+v %v", r, err)
	}
	m, _ := s.Read(ctx, "ann", "coffee")
	if m.Body != "black" {
		t.Fatalf("body %q", m.Body)
	}
	if hits, _ := s.Search(ctx, "ann", "planted", nil, 5); len(hits) != 0 {
		t.Fatalf("index still has the undone text: %+v", hits)
	}
	// Undoing the creation forgets the note; undoing that brings it back.
	created := changes(t, h, history.Filter{Agent: "ann"})
	_, r, err = s.Restore(ctx, "ann", created[len(created)-1].ID, history.Before)
	if err != nil || r.Op() != "deleted" {
		t.Fatalf("%+v %v", r, err)
	}
	if hits, _ := s.Search(ctx, "ann", "black", nil, 5); len(hits) != 0 {
		t.Fatal("removed note still found")
	}
	if _, _, err := s.Restore(ctx, "ann", r.ID, history.Before); err != nil {
		t.Fatal(err)
	}
	if hits, _ := s.Search(ctx, "ann", "black", nil, 5); len(hits) != 1 {
		t.Fatal("restored note not found")
	}
}

func TestStoreWithoutHistory(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.Put(context.Background(), "ann", Note{Title: "x", Content: "y"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Restore(context.Background(), "ann", "abcd", history.Before); err != ErrNoHistory {
		t.Fatal(err)
	}
	if s.Pinned(context.Background(), "ann", "x") {
		t.Fatal("pinned without history")
	}
}
