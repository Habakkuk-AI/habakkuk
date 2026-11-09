package session

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

func TestCompactionsAreAppendOnly(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "transcripts.db"), Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewStore(db)
	sess, _ := s.Create(ctx, "habakkuk", "system")
	for i := range 6 {
		if err := s.Append(ctx, sess.ID, "j", i, llm.UserText("m"), "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if c, err := s.LatestCompaction(ctx, sess.ID); err != nil || c != nil {
		t.Fatalf("new session: %+v, %v", c, err)
	}
	u := &llm.Usage{InputTokens: 7}
	if err := s.AddCompaction(ctx, sess.ID, Compaction{KeepFrom: 2, Through: 6, Summary: "one", Usage: u}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]Compaction{
		"stale transcript":  {KeepFrom: 4, Through: 5, Summary: "x"},
		"does not move on":  {KeepFrom: 2, Through: 6, Summary: "x"},
		"keeps nothing":     {KeepFrom: 6, Through: 6, Summary: "x"},
		"moves backward":    {KeepFrom: 1, Through: 6, Summary: "x"},
		"folds no messages": {KeepFrom: 0, Through: 6, Summary: "x"},
	} {
		if err := s.AddCompaction(ctx, sess.ID, c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := s.AddCompaction(ctx, sess.ID, Compaction{KeepFrom: 4, Through: 6, Summary: "two"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.LatestCompaction(ctx, sess.ID)
	if err != nil || c.KeepFrom != 4 || c.Summary != "two" || c.Through != 6 {
		t.Fatalf("latest = %+v, %v", c, err)
	}
	if msgs, _ := s.Messages(ctx, sess.ID); len(msgs) != 6 {
		t.Fatalf("compaction touched the transcript: %d messages", len(msgs))
	}
}
