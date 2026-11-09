package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// summarizer answers summary requests with "SUMMARY n" and runs one
// list_dir round per user message otherwise. Each response reports the
// estimated size of what it was sent as its prompt usage. If block is set,
// summary calls wait on it.
type summarizer struct {
	mu        sync.Mutex
	summaries []string // inputs of summary calls
	requests  [][]llm.Message
	block     chan struct{}
	overflow  int // reject plain requests with more messages than this
}

func (f *summarizer) Send(ctx context.Context, _ []llm.Target, req llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	if req.System == summarySystem {
		if f.block != nil {
			select {
			case <-f.block:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		f.mu.Lock()
		f.summaries = append(f.summaries, req.Messages[0].Text())
		n := len(f.summaries)
		f.mu.Unlock()
		return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: fmt.Sprintf("SUMMARY %d", n)}}},
			StopReason: llm.StopEnd, Model: "fake/m"}, nil
	}
	f.mu.Lock()
	f.requests = append(f.requests, req.Messages)
	f.mu.Unlock()
	if f.overflow > 0 && len(req.Messages) > f.overflow {
		return nil, &llm.APIError{Status: 400, Message: "prompt is too long: 1234567 tokens > 1000000 maximum"}
	}
	prompt := 0
	for _, m := range req.Messages {
		prompt += estimate(m)
	}
	usage := llm.Usage{InputTokens: int64(prompt)}
	last := req.Messages[len(req.Messages)-1]
	if startsTurn(last) {
		return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{
			{Type: llm.ToolUse, ToolUseID: fmt.Sprintf("c%d", len(req.Messages)), ToolName: "list_dir", Input: json.RawMessage(`{}`)},
		}}, StopReason: llm.StopToolUse, Usage: usage, Model: "fake/m"}, nil
	}
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "ok"}}},
		StopReason: llm.StopEnd, Usage: usage, Model: "fake/m"}, nil
}

func (f *summarizer) counts() (summaries, requests int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.summaries), len(f.requests)
}

func (f *summarizer) lastRequest() []llm.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

// runTurn submits text and runs the turn to completion on worker w.
func (h *harness) runTurn(t *testing.T, sid, text string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.runner.Submit(ctx, sid, text); err != nil {
		t.Fatal(err)
	}
	j, _ := h.queue.Claim(ctx, "w", time.Minute, KindTurn)
	if j == nil {
		t.Fatal("turn not claimable")
	}
	final, err := h.runner.HandleTurn(ctx, j, "w")
	if err != nil {
		t.Fatal(err)
	}
	h.queue.Complete(ctx, j.ID, "w", final)
}

// wantContext checks that the model was sent the summary followed by the
// transcript from keepFrom on, and nothing older.
func wantContext(t *testing.T, sent, transcript []llm.Message, summary string, keepFrom int) {
	t.Helper()
	tail := transcript[keepFrom:]
	if len(sent) < 1 || len(sent) > len(tail)+1 {
		t.Fatalf("sent %d messages for a tail of %d", len(sent), len(tail))
	}
	first := sent[0]
	if first.Role != llm.User || first.Blocks[0].Text != summaryPreamble+summary {
		t.Fatalf("context does not open with the summary: %+v", first)
	}
	var got []llm.Message
	if len(first.Blocks) > 1 {
		got = append(got, llm.Message{Role: llm.User, Blocks: first.Blocks[1:]})
	}
	got = append(got, sent[1:]...)
	if !reflect.DeepEqual(got, tail[:len(got)]) {
		t.Fatalf("context after the summary is not the transcript tail\n got %+v\nwant %+v", got, tail[:len(got)])
	}
}

func TestCompactionTriggersOnceAndKeepsTail(t *testing.T) {
	ctx := context.Background()
	f := &summarizer{}
	h := newHarness(t, f)
	h.runner.Agents["habakkuk"].CompactAt = 1000 // tail budget 200: one turn of ~150
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	question := strings.Repeat("words ", 66) // ~100 tokens

	turns := 0
	for ; turns < 20; turns++ {
		if n, _ := f.counts(); n > 0 {
			break
		}
		h.runTurn(t, s.ID, question)
	}
	if n, _ := f.counts(); n != 1 {
		t.Fatalf("no compaction after %d turns", turns)
	}
	c, err := h.sess.LatestCompaction(ctx, s.ID)
	if err != nil || c == nil {
		t.Fatalf("no compaction recorded: %v", err)
	}
	transcript := h.transcript(t, s.ID)
	if c.Summary != "SUMMARY 1" || c.KeepFrom <= 0 || !startsTurn(transcript[c.KeepFrom]) {
		t.Fatalf("compaction %+v: want a cut at a turn start", c)
	}
	if !strings.Contains(f.summaries[0], "[result of list_dir]") || !strings.Contains(f.summaries[0], "words words") {
		t.Fatalf("summary input misses the transcript: %q", f.summaries[0])
	}
	wantContext(t, f.lastRequest(), transcript, c.Summary, c.KeepFrom)

	// The context is small again: more turns do not compact again, and
	// every request starts from the summary.
	for range 2 {
		h.runTurn(t, s.ID, question)
		wantContext(t, f.lastRequest(), h.transcript(t, s.ID), c.Summary, c.KeepFrom)
	}
	if n, _ := f.counts(); n != 1 {
		t.Fatalf("compacted %d times, want once", n)
	}
	// Nothing was deleted or rewritten.
	if got := h.transcript(t, s.ID); !reflect.DeepEqual(got[:len(transcript)], transcript) {
		t.Fatal("transcript changed")
	}
}

// seedLong writes a finished conversation whose last response reported a
// prompt over the agent's threshold.
func seedLong(t *testing.T, h *harness, sid string, turns int) {
	t.Helper()
	ctx := context.Background()
	seq := 0
	add := func(m llm.Message, usage *llm.Usage) {
		if err := h.sess.Append(ctx, sid, "old", seq, m, "fake/m", usage); err != nil {
			t.Fatal(err)
		}
		seq++
	}
	for i := range turns {
		id := fmt.Sprintf("old%d", i)
		add(llm.UserText(fmt.Sprintf("question %d %s", i, strings.Repeat("x", 400))), nil)
		add(llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.ToolUse, ToolUseID: id, ToolName: "list_dir", Input: json.RawMessage(`{}`)}}}, &llm.Usage{InputTokens: 10})
		add(llm.Message{Role: llm.User, Blocks: []llm.Block{{Type: llm.ToolResult, ToolUseID: id, Text: "hello.txt"}}}, nil)
		add(llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: fmt.Sprintf("answer %d", i)}}}, &llm.Usage{InputTokens: 900, CacheReadTokens: 200})
	}
}

// The worker dies after the summary came back but before it was recorded.
// The session stays exactly as it was, and the worker that takes over
// compacts and finishes the turn.
func TestCrashDuringCompactionLeavesSessionValid(t *testing.T) {
	ctx := context.Background()
	block := make(chan struct{})
	f := &summarizer{block: block}
	h := newHarness(t, f)
	h.runner.Agents["habakkuk"].CompactAt = 1000
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	seedLong(t, h, s.ID, 6)
	h.runner.Submit(ctx, s.ID, "next question")

	j, _ := h.queue.Claim(ctx, "dying", 50*time.Millisecond, KindTurn)
	errc := make(chan error, 1)
	go func() { _, err := h.runner.HandleTurn(ctx, j, "dying"); errc <- err }()
	time.Sleep(100 * time.Millisecond) // stuck in the summary call; lease expires
	j2, _ := h.queue.Claim(ctx, "w2", time.Minute, KindTurn)
	if j2 == nil {
		t.Fatal("expired turn not reclaimable")
	}
	before := h.transcript(t, s.ID)
	close(block)
	if err := <-errc; !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("dying worker err = %v, want lease lost", err)
	}
	if c, _ := h.sess.LatestCompaction(ctx, s.ID); c != nil {
		t.Fatalf("a worker without the lease recorded a compaction: %+v", c)
	}
	if after := h.transcript(t, s.ID); !reflect.DeepEqual(before, after) {
		t.Fatal("transcript changed")
	}

	f.block = nil
	if _, err := h.runner.HandleTurn(ctx, j2, "w2"); err != nil {
		t.Fatal(err)
	}
	c, _ := h.sess.LatestCompaction(ctx, s.ID)
	if c == nil {
		t.Fatal("the new worker did not compact")
	}
	transcript := h.transcript(t, s.ID)
	wantContext(t, f.lastRequest(), transcript, c.Summary, c.KeepFrom)
	users := 0
	for _, m := range transcript {
		if strings.Contains(m.Text(), "next question") {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("user message written %d times", users)
	}
	if last := transcript[len(transcript)-1]; last.Role != llm.Assistant || last.Text() != "ok" {
		t.Fatalf("turn did not finish: %+v", last)
	}
}

// After a restart a new runner reads the compaction and never sends the
// summarized messages again.
func TestResumeAfterRestartUsesCompaction(t *testing.T) {
	ctx := context.Background()
	f := &summarizer{}
	h := newHarness(t, f)
	h.runner.Agents["habakkuk"].CompactAt = 1000
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	seedLong(t, h, s.ID, 6)
	h.runTurn(t, s.ID, "first")
	c, _ := h.sess.LatestCompaction(ctx, s.ID)
	if c == nil {
		t.Fatal("no compaction")
	}

	old := h.runner
	h.runner = &Runner{
		Agents: old.Agents, Sessions: old.Sessions, Queue: old.Queue, Models: old.Models,
		Tools: old.Tools, Bus: old.Bus, Log: old.Log, Env: old.Env,
	}
	h.runTurn(t, s.ID, "after restart")
	wantContext(t, f.lastRequest(), h.transcript(t, s.ID), c.Summary, c.KeepFrom)
	if n, _ := f.counts(); n != 1 {
		t.Fatalf("compacted %d times, want once", n)
	}
}

// A context overflow error below the threshold compacts once and retries.
func TestOverflowCompactsAndRetries(t *testing.T) {
	ctx := context.Background()
	f := &summarizer{overflow: 20}
	h := newHarness(t, f)
	h.runner.Agents["habakkuk"].CompactAt = 2000 // the seeded 1100 tokens stay under it
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	seedLong(t, h, s.ID, 6)
	h.runTurn(t, s.ID, "go on")
	c, _ := h.sess.LatestCompaction(ctx, s.ID)
	if c == nil {
		t.Fatal("overflow did not compact")
	}
	wantContext(t, f.lastRequest(), h.transcript(t, s.ID), c.Summary, c.KeepFrom)
}

func TestManualCompact(t *testing.T) {
	ctx := context.Background()
	f := &summarizer{}
	h := newHarness(t, f)
	h.runner.Agents["habakkuk"].CompactAt = 1000 // tail budget 200: one turn of ~125
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	run := func() string {
		jobID, err := h.runner.SubmitTurn(ctx, Turn{Session: s.ID, Compact: true})
		if err != nil {
			t.Fatal(err)
		}
		j, _ := h.queue.Claim(ctx, "w", time.Minute, KindTurn)
		if j == nil || j.ID != jobID {
			t.Fatal("compaction not claimable")
		}
		final, err := h.runner.HandleTurn(ctx, j, "w")
		if err != nil {
			t.Fatal(err)
		}
		h.queue.Complete(ctx, j.ID, "w", final)
		return final
	}
	if got := run(); got != "Nothing to compact yet." {
		t.Fatalf("empty session: %q", got)
	}
	seedLong(t, h, s.ID, 3)
	if got := run(); !strings.HasPrefix(got, "Compacted: ") {
		t.Fatalf("got %q", got)
	}
	c, _ := h.sess.LatestCompaction(ctx, s.ID)
	if c == nil || c.KeepFrom != 8 || c.Through != 12 {
		t.Fatalf("compaction %+v: want the last turn kept", c)
	}
	if got := run(); got != "Nothing to compact yet." {
		t.Fatalf("second run: %q", got)
	}
}

// Every cut, at every budget, gives a context in which roles alternate and
// each tool result sits right after the call it answers.
func TestCutPointNeverSplitsToolPairs(t *testing.T) {
	var msgs []llm.Message
	call := 0
	for turn := range 12 {
		msgs = append(msgs, llm.UserText(strings.Repeat("q", 40*(turn%4+1))))
		for range turn % 4 { // 0 to 3 tool rounds, some with parallel calls
			a := llm.Message{Role: llm.Assistant}
			r := llm.Message{Role: llm.User}
			for range 1 + call%2 {
				call++
				id := fmt.Sprintf("t%d", call)
				a.Blocks = append(a.Blocks, llm.Block{Type: llm.ToolUse, ToolUseID: id, ToolName: "x", Input: json.RawMessage(`{}`)})
				r.Blocks = append(r.Blocks, llm.Block{Type: llm.ToolResult, ToolUseID: id, Text: strings.Repeat("r", 30*call)})
			}
			msgs = append(msgs, a, r)
		}
		msgs = append(msgs, llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "a"}}})
	}
	// A turn still in progress: it ends with tool results.
	msgs = append(msgs, llm.UserText("last"),
		llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.ToolUse, ToolUseID: "z", ToolName: "x"}}},
		llm.Message{Role: llm.User, Blocks: []llm.Block{{Type: llm.ToolResult, ToolUseID: "z", Text: strings.Repeat("r", 4000)}}})

	sawMidTurn := false
	for base := 0; base < len(msgs)-1; base++ {
		if base > 0 && !startsTurn(msgs[base]) && msgs[base].Role != llm.Assistant {
			continue // never a base
		}
		for budget := 0; budget < 3000; budget += 25 {
			keep := cutPoint(msgs, base, budget)
			if keep == base {
				continue
			}
			if keep < base || keep >= len(msgs) {
				t.Fatalf("base %d budget %d: keep %d out of range", base, budget, keep)
			}
			if !startsTurn(msgs[keep]) {
				sawMidTurn = true
			}
			tr := &turn{msgs: msgs, base: keep, summary: "S"}
			checkContext(t, tr.context())
		}
	}
	if !sawMidTurn {
		t.Fatal("the oversized last turn was never cut inside")
	}
	// A roomy budget keeps whole turns.
	if keep := cutPoint(msgs, 0, 2500); !startsTurn(msgs[keep]) {
		t.Fatalf("keep %d is not a turn start", keep)
	}
}

func checkContext(t *testing.T, ctx []llm.Message) {
	t.Helper()
	if ctx[0].Role != llm.User {
		t.Fatal("context does not start with a user message")
	}
	for i, m := range ctx {
		if i > 0 && m.Role == ctx[i-1].Role {
			t.Fatalf("two %s messages in a row at %d", m.Role, i)
		}
		for _, b := range m.Blocks {
			if b.Type != llm.ToolResult {
				continue
			}
			found := false
			if i > 0 {
				for _, u := range ctx[i-1].ToolUses() {
					found = found || u.ToolUseID == b.ToolUseID
				}
			}
			if !found {
				t.Fatalf("tool result %s at %d has no call before it", b.ToolUseID, i)
			}
		}
	}
}

func TestSummaryInputPlaceholders(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.User, Blocks: []llm.Block{{Type: llm.Text, Text: "see photo"}, {Type: llm.Image, Media: "sha256:1", MediaType: "image/png", Name: "p.png"}}},
		{Role: llm.Assistant, Blocks: []llm.Block{
			{Type: llm.Thinking, Text: "secret reasoning"},
			{Type: llm.Opaque, Raw: json.RawMessage(`{"type":"web_search_tool_result","content":"huge"}`), Provider: "anthropic"},
			{Type: llm.Text, Text: "nice " + strings.Repeat("é", maxRenderedText)},
		}},
	}
	in := summaryInput("old summary", msgs, 1<<20)
	for _, want := range []string{"<previous_summary>\nold summary", `[user image "p.png" omitted]`, "[hosted web search or fetch activity omitted]", "more characters]"} {
		if !strings.Contains(in, want) {
			t.Errorf("summary input lacks %q:\n%s", want, in)
		}
	}
	for _, bad := range []string{"secret reasoning", "huge", "sha256"} {
		if strings.Contains(in, bad) {
			t.Errorf("summary input contains %q", bad)
		}
	}
	if !strings.Contains(summaryInput("", msgs, 60), "earlier lines omitted") {
		t.Error("over-long input not cut")
	}
}
