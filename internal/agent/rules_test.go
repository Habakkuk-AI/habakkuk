package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// ruleModel runs `echo covered` once and reports what came back; a message
// that says "browse" reads a page instead.
type ruleModel struct{}

func (ruleModel) Send(_ context.Context, _ []llm.Target, req llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	for _, b := range last.Blocks {
		if b.Type == llm.ToolResult {
			return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "result: " + b.Text}}},
				StopReason: llm.StopEnd, Model: "fake/m"}, nil
		}
	}
	call := llm.Block{Type: llm.ToolUse, ToolUseID: "x", ToolName: "exec", Input: json.RawMessage(`{"command":"echo covered"}`)}
	if strings.Contains(last.Text(), "browse") {
		call = llm.Block{Type: llm.ToolUse, ToolUseID: "x", ToolName: "fetch_page", Input: json.RawMessage(`{}`)}
	}
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{call}}, StopReason: llm.StopToolUse, Model: "fake/m"}, nil
}

// rulesHarness is a harness whose agent has a rule covering `echo covered`
// and nobody to ask.
func rulesHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, ruleModel{})
	var fetched atomic.Int32
	h.runner.Tools.Register("core", probeTool{"fetch_page", tools.RiskNetwork, &fetched})
	rf := &tools.RuleFile{Path: filepath.Join(t.TempDir(), tools.RulesFileName),
		Config: []tools.Rule{{Tool: "exec", Exact: "echo covered"}}}
	h.runner.Env = func(s *Spec, _ string) *tools.Env {
		return &tools.Env{Workspace: s.Workspace, Mode: s.Mode, Rules: rf, Taint: tools.NewTaint(false)}
	}
	return h
}

// handleDone is harness.handle that also completes the job, so the next
// turn of the same session can be claimed.
func (h *harness) handleDone(t *testing.T, turn Turn) string {
	t.Helper()
	ctx := context.Background()
	id, err := h.runner.SubmitTurn(ctx, turn)
	if err != nil {
		t.Fatal(err)
	}
	j, err := h.queue.Claim(ctx, "w1", time.Minute, KindTurn)
	if err != nil || j == nil || j.ID != id {
		t.Fatalf("claim: %v %v", j, err)
	}
	final, err := h.runner.HandleTurn(ctx, j, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.queue.Complete(ctx, j.ID, "w1", final); err != nil {
		t.Fatal(err)
	}
	return final
}

func covered(final string) bool {
	return strings.Contains(final, "saved approval rule") && !strings.Contains(final, "denied")
}

// A rule answers for a plain turn, and for none of these: work set up by a
// tainted turn, an inbound A2A task, a delegated task of a tainted parent
// or of one whose rules were paused.
func TestRulesAndWhereTurnsComeFrom(t *testing.T) {
	ctx := context.Background()
	h := rulesHarness(t)
	session := func() string {
		s, err := h.runner.NewSession(ctx, "habakkuk")
		if err != nil {
			t.Fatal(err)
		}
		return s.ID
	}
	if final := h.handle(t, Turn{Session: session(), Text: "go"}); !covered(final) {
		t.Fatalf("plain turn: %q", final)
	}
	cases := map[string]Turn{
		"tainted schedule or card": {Session: session(), Text: "go", Background: true, Source: SourceSchedule, Tainted: true},
		"inbound A2A task":         {Session: session(), Text: "go", Source: SourceA2A},
		"sub-turn, tainted parent": {Kind: KindSubturn, Session: session(), Text: subturnPreamble + "go",
			Sub: &SubturnSpec{Parent: session(), Task: "go", Tainted: true}},
		"sub-turn, paused parent": {Kind: KindSubturn, Session: session(), Text: subturnPreamble + "go",
			Sub: &SubturnSpec{Parent: session(), Task: "go", RulesPaused: true}},
	}
	for name, turn := range cases {
		if final := h.handle(t, turn); covered(final) || !strings.Contains(final, "denied") {
			t.Errorf("%s: a rule answered: %q", name, final)
		}
	}
}

// After a turn that read outside content, the session's rules stay paused
// for turns nobody sent a fresh message (a notice, a background result),
// and come back with the owner's next message.
func TestRulesPauseAfterATaintedTurnInTheSession(t *testing.T) {
	ctx := context.Background()
	h := rulesHarness(t)
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	h.handleDone(t, Turn{Session: s.ID, Text: "browse the page"})
	if final := h.handleDone(t, Turn{Session: s.ID, Text: "[process exited] go", Background: true}); covered(final) {
		t.Fatalf("a notice after a tainted turn used a rule: %q", final)
	}
	if final := h.handleDone(t, Turn{Session: s.ID, Text: "go"}); !covered(final) {
		t.Fatalf("the owner's next message did not re-arm the rules: %q", final)
	}
	// A clean session's notices use rules as before.
	clean, _ := h.runner.NewSession(ctx, "habakkuk")
	if final := h.handle(t, Turn{Session: clean.ID, Text: "go", Background: true}); !covered(final) {
		t.Fatalf("a clean notice: %q", final)
	}
}

// A turn that read a page before a restart is still tainted after it
// resumes, so a rule does not answer for its command.
func TestRuleDoesNotCoverAResumedTaintedTurn(t *testing.T) {
	rf := &tools.RuleFile{Path: filepath.Join(t.TempDir(), tools.RulesFileName),
		Config: []tools.Rule{{Tool: "exec", Exact: "echo covered"}}}
	withRule := resumeOpts{second: "exec", input: json.RawMessage(`{"command":"echo covered"}`),
		env: func(e *tools.Env) { e.Rules = rf; e.Mode = tools.ModeWorkspace }}
	res, _ := interruptAndResume(t, "fetch_page", false, withRule)
	if !res.IsError || !strings.Contains(res.Text, "outside content") {
		t.Fatalf("resumed tainted turn: %+v", res)
	}
	res, _ = interruptAndResume(t, "list_dir", false, withRule)
	if res.IsError || !strings.Contains(res.Text, "saved approval rule") {
		t.Fatalf("resumed clean turn: %+v", res)
	}
}
