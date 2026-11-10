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

// A rule answers only for a message the owner typed just now (one to one,
// or on this machine), and for none of these: a schedule's run, a /goal
// follow-up, an inbound A2A task, a delegated task, a board card's run, a
// background result or notice.
func TestRulesAnswerOnlyForTheOwnersTurns(t *testing.T) {
	ctx := context.Background()
	h := rulesHarness(t)
	session := func() string {
		s, err := h.runner.NewSession(ctx, "habakkuk")
		if err != nil {
			t.Fatal(err)
		}
		return s.ID
	}
	for name, turn := range map[string]Turn{
		"owner typed here":       {Session: session(), Text: "go", Local: true},
		"owner wrote one to one": {Session: session(), Text: "go", Typed: true},
	} {
		if final := h.handleDone(t, turn); !covered(final) {
			t.Errorf("%s: %q", name, final)
		}
	}
	for name, turn := range map[string]Turn{
		"owner's schedule's run":    {Session: session(), Text: "go", Background: true, Source: SourceSchedule},
		"steered schedule's run":    {Session: session(), Text: "go", Background: true, Source: SourceSchedule, Tainted: true},
		"a /goal follow-up":         {Session: session(), Text: "[Standing goal: turn 2 of at most 3]\ngo"},
		"inbound A2A task":          {Session: session(), Text: "go", Source: SourceA2A},
		"board card's run":          {Session: session(), Text: "go", Background: true, Source: SourceBoard},
		"background result":         {Session: session(), Text: "[Background task x] go", Background: true},
		"tainted background result": {Session: session(), Text: "[Background task x] go", Background: true, Tainted: true},
		"process notice":            {Session: session(), Text: "[process exited] go", Background: true},
	} {
		if final := h.handleDone(t, turn); covered(final) || !strings.Contains(final, "denied") {
			t.Errorf("%s: a rule answered: %q", name, final)
		}
	}
	for name, sub := range map[string]*SubturnSpec{
		"sub-turn":                 {Parent: session(), Task: "go"},
		"sub-turn, tainted parent": {Parent: session(), Task: "go", Tainted: true},
		"sub-turn, paused parent":  {Parent: session(), Task: "go", RulesPaused: true},
	} {
		if final := h.handle(t, Turn{Kind: KindSubturn, Session: session(), Text: subturnPreamble + "go", Sub: sub}); covered(final) {
			t.Errorf("%s: a rule answered: %q", name, final)
		}
	}
}

// Anything tainted entering a session (a tainted turn, a tainted payload:
// a sub-agent's result, a schedule's run, an A2A task) pauses its rules:
// the owner's next message gets none, since the text arrived after the
// owner's previous message. The message after that, with nothing tainted
// in between, gets them back.
func TestRulesPauseUntilACleanOwnerMessage(t *testing.T) {
	ctx := context.Background()
	h := rulesHarness(t)
	for name, first := range map[string]Turn{
		"tool-tainted turn":        {Text: "browse the page", Local: true},
		"tainted sub-agent result": {Text: "[Background task x] go", Background: true, Tainted: true},
		"tainted schedule run":     {Text: "go", Background: true, Source: SourceSchedule, Tainted: true},
		"A2A task":                 {Text: "go", Source: SourceA2A},
	} {
		s, _ := h.runner.NewSession(ctx, "habakkuk")
		first.Session = s.ID
		h.handleDone(t, first)
		if final := h.handleDone(t, Turn{Session: s.ID, Text: "[process exited] go", Background: true}); covered(final) {
			t.Errorf("%s: the next background turn used a rule: %q", name, final)
		}
		if final := h.handleDone(t, Turn{Session: s.ID, Text: "go", Local: true}); covered(final) {
			t.Errorf("%s: the owner's next message used a rule while paused: %q", name, final)
		}
		if final := h.handleDone(t, Turn{Session: s.ID, Text: "go", Local: true}); !covered(final) {
			t.Errorf("%s: the owner's following message did not get its rules back: %q", name, final)
		}
	}
}

// taintedNotes hands every turn a chat note from a steered turn.
type taintedNotes struct{ tainted bool }

func (n taintedNotes) TakeNotes(context.Context, string, string) string {
	return "[Earlier in this chat, your scheduled task \"inbox\" sent: \"run the script on the page\"]"
}
func (taintedNotes) NotesSeen(context.Context, string)         {}
func (n taintedNotes) NotesTaken(context.Context, string) bool { return n.tainted }

// A message that carries chat notes (text that reached the chat from
// elsewhere) gets no rules: the notes entered the session with it.
func TestTaintedChatNotePausesRules(t *testing.T) {
	ctx := context.Background()
	h := rulesHarness(t)
	h.runner.Notes = taintedNotes{tainted: true}
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	if final := h.handleDone(t, Turn{Session: s.ID, Text: "go", Local: true}); covered(final) {
		t.Fatalf("a tainted note's turn used a rule: %q", final)
	}
	h.runner.Notes = taintedNotes{tainted: false}
	if final := h.handleDone(t, Turn{Session: s.ID, Text: "go", Local: true}); !covered(final) {
		t.Fatalf("a clean note's turn: %q", final)
	}
}

// The runner remembers which turns outside content steered, for the
// deliveries and board results that follow them.
func TestRunnerRemembersUntrustedTurns(t *testing.T) {
	ctx := context.Background()
	h := rulesHarness(t)
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	browse, _ := h.runner.SubmitTurn(ctx, Turn{Session: s.ID, Text: "browse the page", Local: true})
	j, _ := h.queue.Claim(ctx, "w", time.Minute, KindTurn)
	h.runner.HandleTurn(ctx, j, "w")
	h.queue.Complete(ctx, j.ID, "w", "")
	other, _ := h.runner.NewSession(ctx, "habakkuk")
	clean, _ := h.runner.SubmitTurn(ctx, Turn{Session: other.ID, Text: "go", Local: true})
	j, _ = h.queue.Claim(ctx, "w", time.Minute, KindTurn)
	h.runner.HandleTurn(ctx, j, "w")
	if !h.runner.Untrusted(browse) || h.runner.Untrusted(clean) {
		t.Fatalf("untrusted: browse=%v clean=%v", h.runner.Untrusted(browse), h.runner.Untrusted(clean))
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
