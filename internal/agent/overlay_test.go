package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

type fakeOverlay struct {
	mu     sync.Mutex
	steers []string
}

func (*fakeOverlay) Effort(context.Context, string) string { return "max" }
func (*fakeOverlay) System(context.Context, string) string { return "# Persona\nBe brief." }
func (*fakeOverlay) Preamble(context.Context, string) string {
	return "\n\n[Your task list: 1. [ ] check]"
}
func (f *fakeOverlay) Steer(context.Context, string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.steers
	f.steers = nil
	return out
}

type recordingModel struct {
	fakeModel
	mu   sync.Mutex
	reqs []llm.Request
}

func (m *recordingModel) Send(ctx context.Context, chain []llm.Target, req llm.Request, on func(llm.Event)) (*llm.Response, error) {
	m.mu.Lock()
	m.reqs = append(m.reqs, req)
	m.mu.Unlock()
	return m.fakeModel.Send(ctx, chain, req, on)
}

type fakeHooks struct {
	mu     sync.Mutex
	events []string
	veto   string
}

func (h *fakeHooks) Fire(_ context.Context, event, subject string, _ any) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event+":"+subject)
	if event == HookToolBefore {
		return h.veto
	}
	return ""
}

func TestOverlayShapesTheTurn(t *testing.T) {
	ctx := context.Background()
	model := &recordingModel{}
	h := newHarness(t, model)
	ov := &fakeOverlay{steers: []string{"also count the files"}}
	hooks := &fakeHooks{}
	h.runner.Overlay, h.runner.Hooks = ov, hooks
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	h.runner.Submit(ctx, s.ID, "what's here?")
	j, _ := h.queue.Claim(ctx, "w1", time.Minute, KindTurn)
	if _, err := h.runner.HandleTurn(ctx, j, "w1"); err != nil {
		t.Fatal(err)
	}
	if len(model.reqs) != 2 {
		t.Fatalf("%d model calls", len(model.reqs))
	}
	for _, r := range model.reqs {
		if r.Effort != "max" || !strings.HasSuffix(r.System, "\n\n# Persona\nBe brief.") {
			t.Fatalf("effort %q, system %q", r.Effort, r.System)
		}
	}
	msgs := h.transcript(t, s.ID)
	if txt := msgs[0].Text(); !strings.Contains(txt, "what's here?\n\n[Your task list: 1. [ ] check]") {
		t.Fatalf("user message = %q", txt)
	}
	sess, _ := h.sess.Get(ctx, s.ID)
	if strings.Contains(sess.SystemPrompt, "Persona") || sess.Title != "what's here?" {
		t.Fatalf("overlay leaked into the session: %q / %q", sess.SystemPrompt, sess.Title)
	}
	results := msgs[2]
	if results.Blocks[0].Type != llm.ToolResult || results.Blocks[1].Type != llm.Text ||
		results.Blocks[1].Text != SteerPrefix+"also count the files" {
		t.Fatalf("steer not after the results: %+v", results.Blocks)
	}
	want := "session_new:habakkuk,turn_start:habakkuk,tool_before:list_dir,tool_after:list_dir,turn_end:habakkuk"
	if got := strings.Join(hooks.events, ","); got != want {
		t.Fatalf("hooks %s\nwant %s", got, want)
	}
}

func TestToolBeforeHookBlocksTheCall(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &fakeModel{})
	h.runner.Hooks = &fakeHooks{veto: "listing is off limits"}
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	h.runner.Submit(ctx, s.ID, "list")
	j, _ := h.queue.Claim(ctx, "w1", time.Minute, KindTurn)
	final, err := h.runner.HandleTurn(ctx, j, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(final, "blocked by a tool_before hook: listing is off limits") {
		t.Fatalf("final = %q", final)
	}
}
