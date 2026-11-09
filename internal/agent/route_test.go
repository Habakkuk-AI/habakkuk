package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/judge"
)

func TestSmartReason(t *testing.T) {
	img := []Attachment{{MediaType: "image/png", Name: "x.png"}}
	for _, c := range []struct {
		text  string
		media []Attachment
		smart bool
	}{
		{"hi", nil, false},
		{"thanks!", nil, false},
		{"what time is it in Lagos?", nil, false},
		{"remind me what we said about the venue", nil, false},
		{"Build me a landing page for the bakery", nil, true},
		{"can you debug this error", nil, true},
		{"research the best CRMs for a cleaning company", nil, true},
		{"write a proposal for the church conference", nil, true},
		{"look at this", img, true},
		{"```go\nfunc main() {}\n```", nil, true},
		{strings.Repeat("words ", 120), nil, true},
	} {
		got := smartReason(c.text, c.media, nil) != ""
		if got != c.smart {
			t.Errorf("smartReason(%q) smart=%v, want %v", c.text[:min(len(c.text), 40)], got, c.smart)
		}
	}
	history := []llm.Message{{Role: llm.User, Blocks: []llm.Block{{Type: llm.Image, Media: "sha256:1"}}}}
	if smartReason("ok", nil, history) == "" {
		t.Error("a conversation with images must not go to a fast model that may be text-only")
	}
}

// chainRecorder answers each call with a tool call until tools is used up,
// then with text, and records the first model of every call's chain.
type chainRecorder struct {
	mu    sync.Mutex
	first []string
	tools int // tool rounds to ask for in each turn
	left  int
}

func (c *chainRecorder) Send(_ context.Context, chain []llm.Target, req llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.first = append(c.first, chain[0].String())
	last := req.Messages[len(req.Messages)-1]
	if last.Role == llm.User && last.Blocks[0].Type != llm.ToolResult {
		c.left = c.tools // a new turn
	}
	if c.left > 0 {
		c.left--
		return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{
			{Type: llm.ToolUse, ToolUseID: fmt.Sprint("c", len(c.first)), ToolName: "list_dir", Input: json.RawMessage(`{}`)},
		}}, StopReason: llm.StopToolUse, Model: chain[0].Model}, nil
	}
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "ok"}}},
		StopReason: llm.StopEnd, Model: chain[0].Model}, nil
}

func (c *chainRecorder) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.first
	c.first = nil
	return out
}

func routedHarness(t *testing.T, routing string, tools int) (*harness, *chainRecorder) {
	rec := &chainRecorder{tools: tools}
	h := newHarness(t, rec)
	spec := h.runner.Agents["habakkuk"]
	spec.Chain = []llm.Target{{Endpoint: "anthropic", Model: "big"}}
	spec.FastChain = FastChain(llm.Target{Endpoint: "deepseek", Model: "flash"}, spec.Chain)
	spec.Routing, spec.EscalateAfter = routing, 2
	return h, rec
}

func runTurn(t *testing.T, h *harness, sid, text string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.runner.Submit(ctx, sid, text); err != nil {
		t.Fatal(err)
	}
	j, _ := h.queue.Claim(ctx, "w1", time.Minute, KindTurn)
	if j == nil {
		t.Fatal("turn not claimable")
	}
	if _, err := h.runner.HandleTurn(ctx, j, "w1"); err != nil {
		t.Fatal(err)
	}
	h.queue.Complete(ctx, j.ID, "w1", "")
}

func TestRoutingAuto(t *testing.T) {
	ctx := context.Background()
	h, rec := routedHarness(t, RouteAuto, 0)
	s, _ := h.runner.NewSession(ctx, "habakkuk")

	runTurn(t, h, s.ID, "hey, how are you?")
	if got := rec.take(); len(got) != 1 || got[0] != "deepseek/flash" {
		t.Fatalf("light turn: %v, want the fast model", got)
	}
	runTurn(t, h, s.ID, "research the best CRMs for a cleaning company and compare prices")
	if got := rec.take(); got[0] != "anthropic/big" {
		t.Fatalf("real work: %v, want the main model", got)
	}
	// A short follow-up right after real work stays on the main model.
	runTurn(t, h, s.ID, "yes, go ahead")
	if got := rec.take(); got[0] != "anthropic/big" {
		t.Fatalf("follow-up: %v, want the main model", got)
	}
}

func TestRoutingEscalatesLongToolLoops(t *testing.T) {
	ctx := context.Background()
	h, rec := routedHarness(t, RouteAuto, 3)
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	runTurn(t, h, s.ID, "what's in my folder?")
	got := rec.take()
	want := []string{"deepseek/flash", "deepseek/flash", "anthropic/big", "anthropic/big"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("calls %v, want %v (escalate after 2 tool rounds)", got, want)
	}
}

func TestRoutingModesAndOverride(t *testing.T) {
	ctx := context.Background()
	h, rec := routedHarness(t, RouteSmart, 0)
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	runTurn(t, h, s.ID, "hi")
	if got := rec.take(); got[0] != "anthropic/big" {
		t.Fatalf("smart: %v", got)
	}

	h, rec = routedHarness(t, RouteFast, 3)
	s, _ = h.runner.NewSession(ctx, "habakkuk")
	runTurn(t, h, s.ID, "build me a website")
	for _, m := range rec.take() {
		if m != "deepseek/flash" {
			t.Fatalf("fast: a call went to %s; fast mode never escalates", m)
		}
	}

	// A /model choice wins over routing.
	h, rec = routedHarness(t, RouteAuto, 0)
	h.runner.Choices = &memChoices{m: map[string]string{}}
	s, _ = h.runner.NewSession(ctx, "habakkuk")
	h.runner.Choices.SetSessionModel(ctx, s.ID, "openai/chosen")
	runTurn(t, h, s.ID, "hi")
	if got := rec.take(); got[0] != "openai/chosen" {
		t.Fatalf("override: %v", got)
	}
	if note := h.runner.ModelCommand(ctx, s.ID, ""); strings.Contains(note, "Routing") {
		t.Fatalf("/model shows routing while the session is switched: %q", note)
	}
	h.runner.Choices.SetSessionModel(ctx, s.ID, "")
	if note := h.runner.ModelCommand(ctx, s.ID, ""); !strings.Contains(note, "Routing: auto") || !strings.Contains(note, "deepseek/flash") {
		t.Fatalf("/model: %q", note)
	}
}

func TestRoutingWithJudge(t *testing.T) {
	ctx := context.Background()
	var p float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{
			"real_work": map[string]any{"type": "noul", "noul": p}}, "usage": map[string]int{}})
	}))
	defer srv.Close()
	h, rec := routedHarness(t, RouteAuto, 0)
	h.runner.Judge = &judge.Client{BaseURL: srv.URL, Key: func() string { return "k" }}
	s, _ := h.runner.NewSession(ctx, "habakkuk")

	p = 0.1 // the judge overrides the word rules both ways
	runTurn(t, h, s.ID, "can you fix my mood, I'm bored")
	if got := rec.take(); got[0] != "deepseek/flash" {
		t.Fatalf("judge said light: %v", got)
	}
	p = 0.9
	runTurn(t, h, s.ID, "what about the thing we discussed")
	if got := rec.take(); got[0] != "anthropic/big" {
		t.Fatalf("judge said real work: %v", got)
	}
	// Without an answer the word rules decide.
	h.runner.Judge = &judge.Client{BaseURL: "http://127.0.0.1:1", Key: func() string { return "k" }}
	h.runner.routes.Delete(s.ID)
	runTurn(t, h, s.ID, "hello")
	if got := rec.take(); got[0] != "deepseek/flash" {
		t.Fatalf("judge down, light: %v", got)
	}
}
