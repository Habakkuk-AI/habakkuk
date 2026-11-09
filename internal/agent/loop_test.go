package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// fakeModel asks for one list_dir call, then answers with text. If block is
// set, the call after the tool round waits on it (to simulate a crash there).
type fakeModel struct {
	mu    sync.Mutex
	calls int
	block chan struct{}
}

func (f *fakeModel) Send(ctx context.Context, chain []llm.Target, req llm.Request, on func(llm.Event)) (*llm.Response, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	last := req.Messages[len(req.Messages)-1]
	for _, b := range last.Blocks {
		if b.Type == llm.ToolResult {
			if f.block != nil {
				select {
				case <-f.block:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			text := "done: " + b.Text
			on(llm.Event{Type: llm.EventText, Text: text})
			return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: text}}}, StopReason: llm.StopEnd, Model: "fake/m"}, nil
		}
	}
	return &llm.Response{
		Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{
			{Type: llm.Text, Text: "looking"},
			{Type: llm.ToolUse, ToolUseID: "call1", ToolName: "list_dir", Input: json.RawMessage(`{}`)},
		}},
		StopReason: llm.StopToolUse, Model: "fake/m",
	}, nil
}

type harness struct {
	runner *Runner
	queue  *jobs.Queue
	sess   *session.Store
}

func newHarness(t *testing.T, model Sender) *harness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	control, err := store.Open(ctx, filepath.Join(dir, "control.db"), jobs.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	transcripts, err := store.Open(ctx, filepath.Join(dir, "transcripts.db"), session.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close(); transcripts.Close() })
	ws := filepath.Join(dir, "ws")
	os.MkdirAll(ws, 0o755)
	os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("hi"), 0o644)
	reg := tools.NewRegistry()
	tools.RegisterBuiltins(reg)
	h := &harness{queue: jobs.NewQueue(control), sess: session.NewStore(transcripts)}
	spec := &Spec{Name: "habakkuk", Chain: []llm.Target{{Endpoint: "fake", Model: "m"}}, MaxTokens: 1000,
		MaxIterations: 10, Toolsets: []string{"core"}, Mode: tools.ModeWorkspace, Workspace: ws, Dir: dir}
	h.runner = &Runner{
		Agents: map[string]*Spec{"habakkuk": spec}, Sessions: h.sess, Queue: h.queue, Models: model,
		Tools: reg, Bus: events.NewBus(), Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Env: func(s *Spec, _ string) *tools.Env { return &tools.Env{Workspace: s.Workspace, Mode: s.Mode} },
	}
	return h
}

func (h *harness) transcript(t *testing.T, sid string) []llm.Message {
	recs, err := h.sess.Messages(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	var out []llm.Message
	for _, r := range recs {
		out = append(out, r.Message)
	}
	return out
}

func TestTurnRunsToolLoop(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &fakeModel{})
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	jobID, _ := h.runner.Submit(ctx, s.ID, "what's in my workspace?")
	j, _ := h.queue.Claim(ctx, "w1", time.Minute, KindTurn)
	if j == nil || j.ID != jobID {
		t.Fatal("turn not claimable")
	}
	final, err := h.runner.HandleTurn(ctx, j, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(final, "hello.txt") {
		t.Fatalf("final = %q", final)
	}
	msgs := h.transcript(t, s.ID)
	roles := ""
	for _, m := range msgs {
		roles += string(m.Role[0])
	}
	if roles != "uaua" {
		t.Fatalf("roles %q, want uaua", roles)
	}
	if !strings.HasPrefix(msgs[0].Text(), "[") || !strings.Contains(msgs[0].Text(), "what's in my workspace?") {
		t.Fatalf("user message not stamped: %q", msgs[0].Text())
	}
	sess, _ := h.sess.Get(ctx, s.ID)
	if strings.Contains(sess.SystemPrompt, time.Now().Format("2006-01-02")) {
		t.Fatal("system prompt contains the date; it would break the prompt cache")
	}
}

// A worker dies after the model asked for a tool but before the result was
// recorded. Another worker reclaims the job after the lease expires, repairs
// the transcript and finishes the turn exactly once.
func TestCrashMidTurnResumes(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &fakeModel{})
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	jobID, _ := h.runner.Submit(ctx, s.ID, "list files")

	// Recreate exactly what a kill -9 leaves behind: a worker claimed the
	// turn, wrote the user message and the model's tool call, then died
	// before the tool result was recorded.
	j, _ := h.queue.Claim(ctx, "dead-worker", 50*time.Millisecond, KindTurn)
	h.sess.Append(ctx, s.ID, j.ID, 0, llm.UserText("[stamp]\nlist files"), "", nil)
	h.sess.Append(ctx, s.ID, j.ID, 1, llm.Message{Role: llm.Assistant, Blocks: []llm.Block{
		{Type: llm.ToolUse, ToolUseID: "c1", ToolName: "list_dir", Input: json.RawMessage(`{}`)},
	}}, "fake/m", nil)

	// The lease expires; a healthy worker picks the turn up.
	time.Sleep(80 * time.Millisecond)
	j2, _ := h.queue.Claim(ctx, "w2", time.Minute, KindTurn)
	if j2 == nil || j2.ID != jobID {
		t.Fatal("crashed turn was not reclaimable")
	}
	if _, err := h.runner.HandleTurn(ctx, j2, "w2"); err != nil {
		t.Fatal(err)
	}
	msgs := h.transcript(t, s.ID)
	users := 0
	for _, m := range msgs {
		if m.Role == llm.User && strings.Contains(m.Text(), "list files") {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("user message written %d times", users)
	}
	repair := msgs[2]
	if repair.Blocks[0].Type != llm.ToolResult || !repair.Blocks[0].IsError || !strings.Contains(repair.Blocks[0].Text, "interrupted") {
		t.Fatalf("dangling tool call not repaired: %+v", repair)
	}
	if last := msgs[len(msgs)-1]; last.Role != llm.Assistant || len(last.ToolUses()) != 0 {
		t.Fatal("turn did not finish")
	}
}

// A worker that lost its lease must not write into the transcript.
func TestLostLeaseCannotWrite(t *testing.T) {
	ctx := context.Background()
	block := make(chan struct{})
	h := newHarness(t, &fakeModel{block: block})
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	h.runner.Submit(ctx, s.ID, "go")
	j, _ := h.queue.Claim(ctx, "slow", 50*time.Millisecond, KindTurn)
	errc := make(chan error, 1)
	go func() { _, err := h.runner.HandleTurn(ctx, j, "slow"); errc <- err }()
	// Let the slow worker get stuck in its second model call, then steal the job.
	time.Sleep(100 * time.Millisecond)
	j2, _ := h.queue.Claim(ctx, "fast", time.Minute, KindTurn)
	if j2 == nil {
		t.Fatal("expired job not reclaimable")
	}
	before := len(h.transcript(t, s.ID))
	close(block)
	if err := <-errc; !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("slow worker err = %v, want lease lost", err)
	}
	if after := len(h.transcript(t, s.ID)); after != before {
		t.Fatalf("worker without a lease wrote %d messages", after-before)
	}
}

// pauser pauses its server-side loop once, then finishes.
type pauser struct{ calls int }

func (p *pauser) Send(_ context.Context, _ []llm.Target, req llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	p.calls++
	if p.calls == 1 {
		return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{
			{Type: llm.Opaque, Raw: json.RawMessage(`{"type":"server_tool_use"}`), Provider: "anthropic"}}}, StopReason: llm.StopPause}, nil
	}
	if last := req.Messages[len(req.Messages)-1]; last.Role != llm.Assistant {
		return nil, errors.New("resume must resend the paused assistant turn last, with no new user message")
	}
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "found it"}}}, StopReason: llm.StopEnd}, nil
}

func TestPauseTurnResumes(t *testing.T) {
	ctx := context.Background()
	p := &pauser{}
	h := newHarness(t, p)
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	h.runner.Submit(ctx, s.ID, "search")
	j, _ := h.queue.Claim(ctx, "w", time.Minute, KindTurn)
	final, err := h.runner.HandleTurn(ctx, j, "w")
	if err != nil || final != "found it" || p.calls != 2 {
		t.Fatalf("final=%q err=%v calls=%d", final, err, p.calls)
	}
}

// picky rejects any request that carries loadable media.
type picky struct{ withMedia, without int }

func (p *picky) Send(_ context.Context, _ []llm.Target, req llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	if req.Media != nil {
		p.withMedia++
		return nil, &llm.APIError{Status: 400, Message: "messages.0.content.1.image.source.base64: The image was specified using the image/jpeg media type, but the image appears to be a image/png image"}
	}
	p.without++
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "ok"}}}, StopReason: llm.StopEnd}, nil
}

func TestBadMediaDoesNotWedgeSession(t *testing.T) {
	ctx := context.Background()
	p := &picky{}
	h := newHarness(t, p)
	h.runner.Media = func(string) ([]byte, error) { return []byte("x"), nil }
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	run := func(turn Turn) string {
		turn.Session = s.ID
		h.runner.SubmitTurn(ctx, turn)
		j, _ := h.queue.Claim(ctx, "w", time.Minute, KindTurn)
		final, err := h.runner.HandleTurn(ctx, j, "w")
		if err != nil {
			t.Fatal(err)
		}
		h.queue.Complete(ctx, j.ID, "w", final)
		return final
	}
	run(Turn{Text: "look", Media: []Attachment{{Ref: "sha256:1", MediaType: "image/jpeg", Name: "p.jpg", Size: 10}}})
	run(Turn{Text: "and now?"})
	if p.withMedia != 1 || p.without != 2 {
		t.Fatalf("with media %d, without %d", p.withMedia, p.without)
	}
}
