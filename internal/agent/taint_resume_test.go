package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// probeTool is a tool of a given risk that counts its runs.
type probeTool struct {
	name string
	risk tools.Risk
	runs *atomic.Int32
}

func (p probeTool) Spec() llm.ToolSpec { return llm.ToolSpec{Name: p.name} }
func (p probeTool) Risk() tools.Risk   { return p.risk }
func (p probeTool) Run(context.Context, *tools.Env, json.RawMessage) (string, error) {
	p.runs.Add(1)
	return p.name + " ran", nil
}

// twoStep calls first, then (once it has first's result) second, then
// answers. With hold set, the call that would ask for second waits for the
// turn to be stopped instead: the process is shut down right there.
type twoStep struct {
	first, second string
	secondInput   json.RawMessage // default {}
	hosted        bool            // the first step is the provider's hosted search, not a tool
	hold          bool
	mu            sync.Mutex
	held          chan struct{}
}

func (m *twoStep) Send(ctx context.Context, _ []llm.Target, req llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	asst := func(blocks ...llm.Block) (*llm.Response, error) {
		stop := llm.StopEnd
		for _, b := range blocks {
			if b.Type == llm.ToolUse {
				stop = llm.StopToolUse
			}
		}
		return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: blocks}, StopReason: stop, Model: "fake/m"}, nil
	}
	use := func(id, name string) llm.Block {
		return llm.Block{Type: llm.ToolUse, ToolUseID: id, ToolName: name, Input: json.RawMessage(`{}`)}
	}
	// What has this turn done so far?
	didFirst, secondResult := false, ""
	for _, msg := range req.Messages {
		for _, b := range msg.Blocks {
			switch {
			case b.Type == llm.Opaque, b.Type == llm.ToolUse && b.ToolUseID == "first":
				didFirst = true
			case b.Type == llm.ToolResult && b.ToolUseID == "second":
				secondResult = b.Text
			}
		}
	}
	switch {
	case secondResult != "":
		return asst(llm.Block{Type: llm.Text, Text: "done: " + secondResult})
	case !didFirst && m.hosted:
		// Hosted search comes back inside the assistant message, with a
		// local tool call to keep the turn going.
		return asst(llm.Block{Type: llm.Opaque, Raw: json.RawMessage(`{"type":"web_search_tool_result"}`), Provider: "anthropic"},
			use("pad", "list_dir"))
	case !didFirst:
		return asst(use("first", m.first))
	}
	if m.hold {
		m.mu.Lock()
		if m.held != nil {
			close(m.held)
			m.held = nil
		}
		m.mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	second := use("second", m.second)
	if len(m.secondInput) > 0 {
		second.Input = m.secondInput
	}
	return asst(second)
}

// resumeOpts changes interruptAndResume's second step and the turn's Env.
type resumeOpts struct {
	second string
	input  json.RawMessage
	env    func(*tools.Env)
}

// interruptAndResume runs a turn until the model is about to ask for its
// second step, stops it the way a planned shutdown does, and resumes it as
// a new process would: a fresh Env, nothing kept in memory. It returns the
// result of the second tool call.
func interruptAndResume(t *testing.T, first string, hosted bool, opts ...resumeOpts) (result llm.Block, shellRuns int32) {
	t.Helper()
	ctx := context.Background()
	held := make(chan struct{})
	o := resumeOpts{second: "shell"}
	if len(opts) > 0 {
		o = opts[0]
	}
	h := newHarness(t, &twoStep{first: first, second: o.second, secondInput: o.input, hosted: hosted, hold: true, held: held})
	var fetched, shelled atomic.Int32
	h.runner.Tools.Register("core", probeTool{"fetch_page", tools.RiskNetwork, &fetched})
	h.runner.Tools.Register("core", probeTool{"shell", tools.RiskExec, &shelled})
	h.runner.Agents["habakkuk"].Mode = tools.ModeFull
	h.runner.Env = func(s *Spec, _ string) *tools.Env {
		env := &tools.Env{Workspace: s.Workspace, Mode: s.Mode, Taint: tools.NewTaint(false)}
		if o.env != nil {
			o.env(env)
		}
		return env
	}
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	jobID, _ := h.runner.SubmitTyped(ctx, s.ID, "read the page, then tidy up")

	j, _ := h.queue.Claim(ctx, "before", time.Minute, KindTurn)
	runCtx, shutdown := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { _, err := h.runner.HandleTurn(runCtx, j, "before"); errc <- err }()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never reached its second step")
	}
	shutdown()
	if err := <-errc; err == nil {
		t.Fatal("the interrupted turn reported success")
	}
	if err := h.queue.Release(ctx, jobID, "before"); err != nil {
		t.Fatal(err)
	}

	// The next start: same databases, a model that carries on.
	h.runner.Models = &twoStep{first: first, second: o.second, secondInput: o.input, hosted: hosted}
	j2, _ := h.queue.Claim(ctx, "after", time.Minute, KindTurn)
	if j2 == nil || j2.ID != jobID {
		t.Fatal("the interrupted turn was not resumed")
	}
	if _, err := h.runner.HandleTurn(ctx, j2, "after"); err != nil {
		t.Fatal(err)
	}
	for _, m := range h.transcript(t, s.ID) {
		for _, b := range m.Blocks {
			if b.Type == llm.ToolResult && b.ToolUseID == "second" {
				return b, shelled.Load()
			}
		}
	}
	t.Fatal("the resumed turn never made its second call")
	return llm.Block{}, 0
}

// A turn that read a web page before a restart is still tainted after it
// resumes: the shell needs a human, as it did before the restart.
func TestTaintSurvivesAResumedTurn(t *testing.T) {
	res, runs := interruptAndResume(t, "fetch_page", false)
	if runs != 0 || !res.IsError || !strings.Contains(res.Text, "outside content") {
		t.Fatalf("after the restart the shell ran %d times without a human; result: %+v", runs, res)
	}
}

// The same for the provider's hosted web search, whose results arrive in
// opaque blocks instead of through a tool.
func TestHostedSearchTaintSurvivesAResumedTurn(t *testing.T) {
	res, runs := interruptAndResume(t, "", true)
	if runs != 0 || !res.IsError || !strings.Contains(res.Text, "outside content") {
		t.Fatalf("after the restart the shell ran %d times without a human; result: %+v", runs, res)
	}
}

// A turn that had only read local files is not tainted by resuming.
func TestResumeDoesNotTaintACleanTurn(t *testing.T) {
	res, runs := interruptAndResume(t, "list_dir", false)
	if runs != 1 || res.IsError {
		t.Fatalf("a clean resumed turn could not use the shell (ran %d times): %+v", runs, res)
	}
}
