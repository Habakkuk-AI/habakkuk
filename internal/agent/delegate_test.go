package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// boss delegates two tasks in one response; workers answer their task.
type boss struct {
	mu        sync.Mutex
	sawTools  map[string][]string // first user text -> tool names offered
	subCalled int
}

func (b *boss) Send(_ context.Context, _ []llm.Target, req llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	first := req.Messages[0].Text()
	var names []string
	for _, t := range req.Tools {
		names = append(names, t.Name)
	}
	b.mu.Lock()
	b.sawTools[first] = names
	b.mu.Unlock()
	last := req.Messages[len(req.Messages)-1]
	if strings.Contains(first, "Task from another agent") {
		b.mu.Lock()
		b.subCalled++
		b.mu.Unlock()
		task := strings.SplitN(first, "\n", 3)[2]
		return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "report on " + task}}}, StopReason: llm.StopEnd}, nil
	}
	var results []string
	for _, blk := range last.Blocks {
		if blk.Type == llm.ToolResult {
			results = append(results, blk.Text)
		}
	}
	if len(results) > 0 {
		return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: strings.Join(results, " | ")}}}, StopReason: llm.StopEnd}, nil
	}
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{
		{Type: llm.ToolUse, ToolUseID: "a", ToolName: "delegate", Input: json.RawMessage(`{"task":"apples"}`)},
		{Type: llm.ToolUse, ToolUseID: "b", ToolName: "delegate", Input: json.RawMessage(`{"task":"pears"}`)},
	}}, StopReason: llm.StopToolUse}, nil
}

func TestDelegateRunsSubAgentsInParallel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &boss{sawTools: map[string][]string{}}
	h := newHarness(t, b)
	h.runner.RegisterDelegate(h.runner.Tools)
	h.runner.Agents["habakkuk"].Toolsets = []string{"core", "delegate"}
	pool := func(kind string) *jobs.Pool {
		return &jobs.Pool{Queue: h.queue, Handlers: map[string]jobs.Handler{kind: h.runner.HandleTurn},
			Concurrency: 1, Lease: 5 * time.Second, Poll: 20 * time.Millisecond, Log: h.runner.Log}
	}
	// One worker each: the parent holds the only turn worker while it waits.
	main, sub := pool(KindTurn), pool(KindSubturn)
	h.runner.Wake = func() { main.Wake(); sub.Wake() }
	go main.Run(ctx)
	go sub.Run(ctx)

	s, _ := h.runner.NewSession(ctx, "habakkuk")
	id, _ := h.runner.Submit(ctx, s.ID, "split this")
	final, err := h.runner.wait(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if final != "report on apples | report on pears" {
		t.Fatalf("final = %q", final)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subCalled != 2 {
		t.Fatalf("sub-agents ran %d times", b.subCalled)
	}
	for first, tools := range b.sawTools {
		if strings.Contains(first, "Task from another agent") && strings.Contains(strings.Join(tools, ","), "delegate") {
			t.Fatal("a sub-agent was offered the delegate tool")
		}
	}
	// Repeating the same call (as after a crash) reattaches to the finished job.
	env := h.runner.Env(h.runner.Agents["habakkuk"], s.ID)
	env.Session, env.Agent = s.ID, "habakkuk"
	again, err := delegateTool{h.runner}.Run(ctx, env, json.RawMessage(`{"task":"apples"}`))
	if err != nil || again != "report on apples" || b.subCalled != 2 {
		t.Fatalf("reattach: %q %v calls=%d", again, err, b.subCalled)
	}
}
