package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// script is a fake model driven by a function of the chain and request.
type script struct {
	mu   sync.Mutex
	reqs []llm.Request
	fn   func(chain []llm.Target, req llm.Request) *llm.Response
}

func (s *script) Send(ctx context.Context, chain []llm.Target, req llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	r := s.fn(chain, req)
	if r == nil {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return r, nil
}

func say(text string) *llm.Response {
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: text}}}, StopReason: llm.StopEnd}
}

func call(id, name string, input any) *llm.Response {
	raw, _ := json.Marshal(input)
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{
		{Type: llm.ToolUse, ToolUseID: id, ToolName: name, Input: raw}}}, StopReason: llm.StopToolUse}
}

func lastResult(m llm.Message) (string, bool) {
	for _, b := range m.Blocks {
		if b.Type == llm.ToolResult {
			return b.Text, true
		}
	}
	return "", false
}

func isChild(req llm.Request) bool {
	return strings.Contains(req.Messages[0].Text(), "Task from another agent")
}

// pools starts turn and sub-turn workers for the harness.
func (h *harness) pools(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pool := func(kind string) *jobs.Pool {
		return &jobs.Pool{Queue: h.queue, Handlers: map[string]jobs.Handler{kind: h.runner.HandleTurn},
			Concurrency: 2, Lease: 5 * time.Second, Poll: 10 * time.Millisecond, Log: h.runner.Log}
	}
	main, sub := pool(KindTurn), pool(KindSubturn)
	h.runner.Wake = func() { main.Wake(); sub.Wake() }
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); main.Run(ctx) }()
	go func() { defer wg.Done(); sub.Run(ctx) }()
	t.Cleanup(func() { cancel(); wg.Wait() })
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestDelegateOutputSchemaRetriesOnce(t *testing.T) {
	ctx := context.Background()
	schema := `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false}`
	m := &script{fn: func(_ []llm.Target, req llm.Request) *llm.Response {
		last := req.Messages[len(req.Messages)-1]
		if isChild(req) {
			if strings.HasPrefix(last.Text(), schemaRetryPrefix) {
				return say("Sorry:\n```json\n{\"n\": 3}\n```")
			}
			return say(`I counted. {"n": "three"}`)
		}
		if res, ok := lastResult(last); ok {
			return say("child said " + res)
		}
		return call("d1", "delegate", map[string]any{"task": "count", "output_schema": json.RawMessage(schema)})
	}}
	h := newHarness(t, m)
	h.runner.RegisterDelegate(h.runner.Tools)
	h.runner.Agents["habakkuk"].Toolsets = []string{"core", "delegate"}
	h.pools(t)
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	id, _ := h.runner.Submit(ctx, s.ID, "go")
	final, err := h.runner.wait(ctx, id)
	if err != nil || final != `child said {"n": 3}` {
		t.Fatalf("final = %q, %v", final, err)
	}
	// The child's prompt carried the schema.
	m.mu.Lock()
	defer m.mu.Unlock()
	if !slices.ContainsFunc(m.reqs, func(r llm.Request) bool {
		return isChild(r) && strings.Contains(r.Messages[0].Text(), `"additionalProperties":false`)
	}) {
		t.Fatal("the child was not told the schema")
	}
}

func TestDelegateOutputSchemaFailsAfterOneRetry(t *testing.T) {
	ctx := context.Background()
	m := &script{fn: func(_ []llm.Target, req llm.Request) *llm.Response {
		last := req.Messages[len(req.Messages)-1]
		if isChild(req) {
			return say("no json here")
		}
		if res, ok := lastResult(last); ok {
			return say("result: " + res)
		}
		return call("d1", "delegate", map[string]any{"task": "count", "output_schema": map[string]any{"type": "array"}})
	}}
	h := newHarness(t, m)
	h.runner.RegisterDelegate(h.runner.Tools)
	h.runner.Agents["habakkuk"].Toolsets = []string{"core", "delegate"}
	h.pools(t)
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	id, _ := h.runner.Submit(ctx, s.ID, "go")
	final, _ := h.runner.wait(ctx, id)
	if !strings.Contains(final, "does not match output_schema after one retry") {
		t.Fatalf("final = %q", final)
	}
	children := 0
	m.mu.Lock()
	for _, r := range m.reqs {
		if isChild(r) {
			children++
		}
	}
	m.mu.Unlock()
	if children != 2 {
		t.Fatalf("child model calls = %d, want 2 (one retry)", children)
	}
}

type noteTool struct{ name string }

func (n noteTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: n.name, Description: "test tool " + n.name, Schema: json.RawMessage(`{"type":"object"}`)}
}
func (noteTool) Risk() tools.Risk { return tools.RiskRead }
func (n noteTool) Run(context.Context, *tools.Env, json.RawMessage) (string, error) {
	return "ran " + n.name, nil
}

func TestDelegateModelAndToolsets(t *testing.T) {
	ctx := context.Background()
	var childChain []llm.Target
	var childTools []string
	m := &script{fn: func(chain []llm.Target, req llm.Request) *llm.Response {
		last := req.Messages[len(req.Messages)-1]
		if isChild(req) {
			childChain = chain
			for _, s := range req.Tools {
				childTools = append(childTools, s.Name)
			}
			return say("child done")
		}
		if res, ok := lastResult(last); ok {
			return say(res)
		}
		if strings.Contains(last.Text(), "bad") {
			return call("d0", "delegate", map[string]any{"task": "t", "toolsets": []string{"board"}})
		}
		return call("d1", "delegate", map[string]any{"task": "t", "model": "other/small", "toolsets": []string{"core"}})
	}}
	h := newHarness(t, m)
	h.runner.Tools.Register("extra", noteTool{"extra_tool"})
	h.runner.RegisterDelegate(h.runner.Tools)
	h.runner.Agents["habakkuk"].Toolsets = []string{"core", "delegate", "extra"}
	h.runner.CheckModel = func(ref string) error {
		if ref != "other/small" {
			return fmt.Errorf("unknown")
		}
		return nil
	}
	h.pools(t)
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	id, _ := h.runner.Submit(ctx, s.ID, "go")
	if final, err := h.runner.wait(ctx, id); err != nil || final != "child done" {
		t.Fatalf("final = %q, %v", final, err)
	}
	if len(childChain) != 2 || childChain[0] != (llm.Target{Endpoint: "other", Model: "small"}) || childChain[1].Endpoint != "fake" {
		t.Fatalf("child chain = %v", childChain)
	}
	if slices.Contains(childTools, "extra_tool") || slices.Contains(childTools, "delegate") || !slices.Contains(childTools, "read_file") {
		t.Fatalf("child tools = %v", childTools)
	}
	// A toolset the parent does not have is refused.
	s2, _ := h.runner.NewSession(ctx, "habakkuk")
	id2, _ := h.runner.Submit(ctx, s2.ID, "bad")
	if final, _ := h.runner.wait(ctx, id2); !strings.Contains(final, `toolset "board" is not one of yours`) {
		t.Fatalf("final = %q", final)
	}
}

func TestBackgroundDelegateReportsToTheParent(t *testing.T) {
	ctx := context.Background()
	m := &script{fn: func(_ []llm.Target, req llm.Request) *llm.Response {
		last := req.Messages[len(req.Messages)-1]
		if isChild(req) {
			return say("found 3 bugs")
		}
		if strings.Contains(last.Text(), BackgroundPrefix) {
			return say("noted: " + last.Text())
		}
		if res, ok := lastResult(last); ok {
			return say("started: " + res)
		}
		return call("d1", "delegate", map[string]any{"task": "audit", "background": true})
	}}
	h := newHarness(t, m)
	h.runner.RegisterDelegate(h.runner.Tools)
	h.runner.Agents["habakkuk"].Toolsets = []string{"core", "delegate"}
	h.pools(t)
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	j, _ := h.queue.Enqueue(ctx, jobs.NewJob{ID: "turn_x", Kind: KindTurn, Agent: "habakkuk", SerialKey: s.ID,
		Payload: TurnPayload{Session: s.ID, Text: "audit it", Origin: json.RawMessage(`{"chat":"c1"}`)}})
	h.runner.Wake()
	final, err := h.runner.wait(ctx, j)
	if err != nil || !strings.Contains(final, "in the background") {
		t.Fatalf("final = %q, %v", final, err)
	}
	waitUntil(t, "background result turn", func() bool {
		for _, msg := range h.transcript(t, s.ID) {
			if strings.HasPrefix(msg.Text(), "noted: ") {
				return true
			}
		}
		return false
	})
	msgs := h.transcript(t, s.ID)
	got := msgs[len(msgs)-1].Text()
	if !strings.Contains(got, "found 3 bugs") || !strings.Contains(got, "Task: audit") {
		t.Fatalf("parent got %q", got)
	}
	// The result turn answers in the chat the delegation came from.
	sub := subturnID(s.ID, "habakkuk", delegateInput{Task: "audit", Background: true})
	bg, err := h.queue.Get(ctx, "bg_"+strings.TrimPrefix(sub, "sub_"))
	if err != nil {
		t.Fatal(err)
	}
	var p TurnPayload
	if err := bg.Decode(&p); err != nil || string(p.Origin) != `{"chat":"c1"}` {
		t.Fatalf("result turn origin = %s, %v", p.Origin, err)
	}
}

func TestSubagentsListAndStop(t *testing.T) {
	ctx := context.Background()
	started := make(chan struct{}, 1)
	m := &script{fn: func(_ []llm.Target, req llm.Request) *llm.Response {
		last := req.Messages[len(req.Messages)-1]
		if isChild(req) {
			select {
			case started <- struct{}{}:
			default:
			}
			return nil // works until canceled
		}
		if strings.Contains(last.Text(), BackgroundPrefix) {
			return say("ack: " + last.Text())
		}
		if _, ok := lastResult(last); ok {
			return say("ok")
		}
		return call("d1", "delegate", map[string]any{"task": "long research", "background": true, "model": "fake/m"})
	}}
	h := newHarness(t, m)
	h.runner.RegisterDelegate(h.runner.Tools)
	h.runner.Agents["habakkuk"].Toolsets = []string{"core", "delegate"}
	h.pools(t)
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	id, _ := h.runner.Submit(ctx, s.ID, "research")
	if _, err := h.runner.wait(ctx, id); err != nil {
		t.Fatal(err)
	}
	<-started
	subs, err := h.runner.Subturns(ctx, s.ID)
	if err != nil || len(subs) != 1 || subs[0].Task != "long research" || !subs[0].Background {
		t.Fatalf("subturns = %+v, %v", subs, err)
	}
	out := FormatSubturns(subs, time.Now())
	if !strings.Contains(out, subs[0].ID) || !strings.Contains(out, "background") || !strings.Contains(out, "fake/m") {
		t.Fatalf("listing = %q", out)
	}
	if other, _ := h.runner.Subturns(ctx, "ses_other"); len(other) != 0 {
		t.Fatal("another session sees the task")
	}
	if _, err := h.runner.StopSubturn(ctx, s.ID, "sub_zzz"); err == nil {
		t.Fatal("stopped an unknown task")
	}
	prefix := strings.TrimPrefix(subs[0].ID, "sub_")[:6]
	if _, err := h.runner.StopSubturn(ctx, s.ID, prefix); err != nil {
		t.Fatal(err)
	}
	if j, _ := h.queue.Get(ctx, subs[0].ID); j.Status != jobs.Canceled {
		t.Fatalf("status = %s", j.Status)
	}
	waitUntil(t, "stopped notice", func() bool {
		msgs := h.transcript(t, s.ID)
		return strings.Contains(msgs[len(msgs)-1].Text(), "was stopped")
	})
	if subs, _ := h.runner.Subturns(ctx, s.ID); len(subs) != 0 {
		t.Fatalf("still listed: %+v", subs)
	}
}

func gitOK(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestDelegateWorktree(t *testing.T) {
	gitOK(t)
	ctx := context.Background()
	m := &script{fn: func(_ []llm.Target, req llm.Request) *llm.Response {
		last := req.Messages[len(req.Messages)-1]
		if isChild(req) {
			if _, ok := lastResult(last); ok {
				return say("added feature")
			}
			return call("w1", "write_file", map[string]string{"path": "feature.txt", "content": "new\n"})
		}
		if res, ok := lastResult(last); ok {
			return say(res)
		}
		return call("d1", "delegate", map[string]any{"task": "add the feature", "worktree": true})
	}}
	h := newHarness(t, m)
	ws := h.runner.Agents["habakkuk"].Workspace
	run(t, ws, "init", "-q")
	run(t, ws, "add", "-A")
	run(t, ws, "commit", "-q", "-m", "init")
	h.runner.RegisterDelegate(h.runner.Tools)
	h.runner.Agents["habakkuk"].Toolsets = []string{"core", "delegate"}
	h.pools(t)
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	id, _ := h.runner.Submit(ctx, s.ID, "go")
	final, err := h.runner.wait(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(final, "added feature") || !strings.Contains(final, "branch habakkuk/sub_") || !strings.Contains(final, "feature.txt") {
		t.Fatalf("final = %q", final)
	}
	if _, err := os.Stat(filepath.Join(ws, "feature.txt")); err == nil {
		t.Fatal("the change landed in the main workspace")
	}
	matches, _ := filepath.Glob(filepath.Join(ws, WorktreeDir, "sub_*", "feature.txt"))
	if len(matches) != 1 {
		t.Fatalf("worktree files: %v", matches)
	}
	exclude, _ := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	if !strings.Contains(string(exclude), "/"+WorktreeDir+"/") {
		t.Fatalf("exclude = %q", exclude)
	}
	out, _ := exec.Command("git", "-C", ws, "status", "--porcelain").Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("main tree is dirty: %s", out)
	}
}

func TestDelegateWorktreeNeedsGitRepo(t *testing.T) {
	gitOK(t)
	ctx := context.Background()
	m := &script{fn: func(_ []llm.Target, req llm.Request) *llm.Response {
		if res, ok := lastResult(req.Messages[len(req.Messages)-1]); ok {
			return say(res)
		}
		return call("d1", "delegate", map[string]any{"task": "x", "worktree": true})
	}}
	h := newHarness(t, m)
	h.runner.RegisterDelegate(h.runner.Tools)
	h.runner.Agents["habakkuk"].Toolsets = []string{"core", "delegate"}
	h.pools(t)
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	id, _ := h.runner.Submit(ctx, s.ID, "go")
	if final, _ := h.runner.wait(ctx, id); !strings.Contains(final, "git repository") {
		t.Fatalf("final = %q", final)
	}
}
