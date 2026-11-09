package review

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/judge"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

type countingModel struct{ calls int }

func (m *countingModel) Send(context.Context, []llm.Target, llm.Request, func(llm.Event)) (*llm.Response, error) {
	m.calls++
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text,
		Text: `{"decision":"ask","reason":"model looked"}`}}}, StopReason: llm.StopEnd, Model: "m"}, nil
}

func judgeServer(t *testing.T, routine, risky float64) *judge.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{
			"routine": map[string]any{"type": "noul", "noul": routine},
			"risky":   map[string]any{"type": "noul", "noul": risky}}, "usage": map[string]int{}})
	}))
	t.Cleanup(srv.Close)
	return &judge.Client{BaseURL: srv.URL, Key: func() string { return "k" }}
}

func TestJudgeScreen(t *testing.T) {
	req := tools.ApprovalRequest{Tool: "exec", Input: json.RawMessage(`{"command":"go test ./..."}`), Task: "run the tests", Workspace: "/w"}
	for _, c := range []struct {
		name           string
		routine, risky float64
		tainted        bool
		allow          bool
	}{
		{"confident routine", 0.99, 0.01, false, true},
		{"not sure enough", 0.9, 0.01, false, false},
		{"routine but risky", 0.99, 0.2, false, false},
		{"tainted turn", 0.99, 0.01, true, false},
	} {
		m := &countingModel{}
		r := &Reviewer{Agent: "a", Models: m, Chain: []llm.Target{{Endpoint: "p", Model: "m"}}, Judge: judgeServer(t, c.routine, c.risky)}
		q := req
		q.Tainted = c.tainted
		rv := r.Review(context.Background(), q)
		if c.allow && (rv.Verdict != tools.VerdictAllow || m.calls != 0) {
			t.Errorf("%s: %+v, model calls %d; want allow without the model", c.name, rv, m.calls)
		}
		if !c.allow && (rv.Verdict == tools.VerdictAllow || m.calls != 1) {
			t.Errorf("%s: %+v, model calls %d; want the model to decide", c.name, rv, m.calls)
		}
	}
	// A failing judge falls through to the model.
	m := &countingModel{}
	r := &Reviewer{Models: m, Chain: []llm.Target{{Endpoint: "p", Model: "m"}}, Judge: &judge.Client{BaseURL: "http://127.0.0.1:1", Key: func() string { return "k" }}}
	if rv := r.Review(context.Background(), req); rv.Verdict == tools.VerdictAllow || m.calls != 1 {
		t.Fatalf("unreachable judge: %+v, %d", rv, m.calls)
	}
}
