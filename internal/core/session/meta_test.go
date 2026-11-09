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
)

func openStore(t *testing.T) *Store {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "transcripts.db"), Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(db)
}

func assistant(text string) llm.Message {
	return llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: text}}}
}

func TestTitlesArchiveAndDelete(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	a, _ := s.Create(ctx, "habakkuk", "sys")
	b, _ := s.Create(ctx, "habakkuk", "sys")
	if err := s.SetTitle(ctx, a.ID, "first message"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetTitleFrom(ctx, a.ID, "Auto title", TitleAuto); err != nil || !ok {
		t.Fatalf("auto title: %v %v", ok, err)
	}
	if ok, _ := s.SetTitleFrom(ctx, a.ID, "Again", TitleAuto); ok {
		t.Fatal("a second automatic title replaced the first")
	}
	if ok, _ := s.SetTitleFrom(ctx, a.ID, "Mine", TitleUser); !ok {
		t.Fatal("user title refused")
	}
	if ok, _ := s.SetTitleFrom(ctx, a.ID, "Auto", TitleAuto); ok {
		t.Fatal("automatic title replaced the user's")
	}
	got, _ := s.Get(ctx, a.ID)
	if got.Title != "Mine" || got.TitleSource != TitleUser {
		t.Fatalf("got %+v", got)
	}
	if err := s.SetArchived(ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List(ctx, "", 10)
	arch, _ := s.ListArchived(ctx, "", 10)
	if len(list) != 1 || list[0].ID != b.ID || len(arch) != 1 || arch[0].ID != a.ID || !arch[0].Archived {
		t.Fatalf("list %+v archived %+v", list, arch)
	}
	if err := s.Delete(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, b.ID); err == nil {
		t.Fatal("deleted session still there")
	}
	if err := s.Delete(ctx, b.ID); err == nil {
		t.Fatal("deleting twice succeeded")
	}
}

func TestForkCopiesMessagesAndCompactions(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	src, _ := s.Create(ctx, "habakkuk", "frozen prompt")
	for i := range 6 {
		m := llm.UserText("question about kiwis")
		if i%2 == 1 {
			m = assistant("answer")
		}
		if err := s.Append(ctx, src.ID, "job1", i, m, "fake/m", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddCompaction(ctx, src.ID, Compaction{KeepFrom: 2, Through: 6, Summary: "sum"}); err != nil {
		t.Fatal(err)
	}
	f, err := s.Fork(ctx, src.ID, 4, "copy")
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := s.Messages(ctx, f.ID)
	if len(recs) != 4 || recs[0].JobID != "" || recs[1].Model != "fake/m" {
		t.Fatalf("copied %+v", recs)
	}
	c, err := s.LatestCompaction(ctx, f.ID)
	if err != nil || c == nil || c.KeepFrom != 2 || c.Through != 4 || c.Summary != "sum" {
		t.Fatalf("compaction %+v %v", c, err)
	}
	got, _ := s.Get(ctx, f.ID)
	if got.SystemPrompt != "frozen prompt" || got.Parent != src.ID || got.Title != "copy" {
		t.Fatalf("fork %+v", got)
	}
	// A fork before the compaction's cut leaves it behind.
	f2, _ := s.Fork(ctx, src.ID, 2, "")
	if c, _ := s.LatestCompaction(ctx, f2.ID); c != nil {
		t.Fatalf("compaction copied past the cut: %+v", c)
	}
	// It can go on like any session.
	if err := s.Append(ctx, f.ID, "", 4, llm.UserText("more"), "", nil); err != nil {
		t.Fatal(err)
	}
}

func TestExportFormats(t *testing.T) {
	sess := &Session{ID: "ses_1", Agent: "habakkuk", Title: "Tags <b>", CreatedAt: time.Unix(0, 0)}
	recs := []Record{
		{Seq: 0, Message: llm.UserText("hi <script>alert(1)</script>")},
		{Seq: 1, Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{
			{Type: llm.Text, Text: "running"},
			{Type: llm.ToolUse, ToolUseID: "c1", ToolName: "exec", Input: json.RawMessage(`{"command":"ls"}`)}}}},
		{Seq: 2, Message: llm.Message{Role: llm.User, Blocks: []llm.Block{{Type: llm.ToolResult, ToolUseID: "c1", Text: "a.txt"}}}},
		{Seq: 3, Message: assistant("done")},
	}
	now := time.Unix(100, 0)
	md, _ := Export(sess, recs, FormatMarkdown, now)
	if !strings.Contains(string(md), "**Result of `exec`**") || !strings.Contains(string(md), "## habakkuk\n\ndone") {
		t.Fatalf("markdown:\n%s", md)
	}
	js, err := Export(sess, recs, FormatJSON, now)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ID       string `json:"id"`
		Messages []struct {
			Role   string      `json:"role"`
			Blocks []llm.Block `json:"blocks"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(js, &doc); err != nil || doc.ID != "ses_1" || len(doc.Messages) != 4 || doc.Messages[1].Blocks[1].ToolName != "exec" {
		t.Fatalf("json %s: %v", js, err)
	}
	page, _ := Export(sess, recs, FormatHTML, now)
	h := string(page)
	if strings.Contains(h, "<script>") || !strings.Contains(h, "&lt;script&gt;") || !strings.Contains(h, "<title>Tags &lt;b&gt;</title>") {
		t.Fatalf("html not escaped:\n%s", h)
	}
	if !strings.Contains(h, `class="msg tools"`) || !strings.Contains(h, "Result of <code>exec</code>") {
		t.Fatalf("html:\n%s", h)
	}
	if _, err := Export(sess, recs, "pdf", now); err == nil {
		t.Fatal("unknown format accepted")
	}
}
