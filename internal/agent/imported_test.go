package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// A conversation imported from another agent is history: no turn runs in
// it, so its transcript stays as it came.
func TestNoTurnInImportedSession(t *testing.T) {
	ctx := context.Background()
	m := &fakeModel{}
	h := newHarness(t, m)
	id, _, err := h.sess.Import(ctx, session.Import{Source: "other", SourceID: "a/1", Agent: "habakkuk",
		Messages: []session.ImportedMessage{{Role: llm.User, Text: "hello"}, {Role: llm.Assistant, Text: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.runner.Submit(ctx, id, "go on"); !errors.Is(err, session.ErrImported) {
		t.Fatalf("Submit = %v, want ErrImported", err)
	}
	if n := len(h.transcript(t, id)); n != 2 || m.calls != 0 {
		t.Fatalf("%d messages, %d model calls; want the imported 2 and none", n, m.calls)
	}
}
