package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// echoModel runs `echo covered` and reports what came back.
type echoModel struct{}

func (echoModel) Send(_ context.Context, _ []llm.Target, req llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	for _, b := range req.Messages[len(req.Messages)-1].Blocks {
		if b.Type == llm.ToolResult {
			return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "result: " + b.Text}}},
				StopReason: llm.StopEnd, Model: "fake/m"}, nil
		}
	}
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{
		{Type: llm.ToolUse, ToolUseID: "x", ToolName: "exec", Input: json.RawMessage(`{"command":"echo covered"}`)}}},
		StopReason: llm.StopToolUse, Model: "fake/m"}, nil
}

// A rule saved on this machine (from the dashboard's chat) answers for a
// message typed here and for a schedule the owner made, and for nothing
// else: an API request, a board card's run, a meeting notice, a
// sub-agent's task, an A2A task, a schedule a steered turn made.
func TestLocalRuleAnswersOnlyForTheOwnersTurns(t *testing.T) {
	a, _ := openWith(t, "")
	a.Runner.Models = echoModel{}
	ctx := context.Background()
	if _, err := a.Rules("habakkuk").Save(tools.Rule{Tool: "exec", Exact: "echo covered", Place: tools.PlaceLocal, Source: "local"}); err != nil {
		t.Fatal(err)
	}
	run := func(turn agent.Turn) string {
		t.Helper()
		sess, err := a.Runner.NewSession(ctx, "habakkuk")
		if err != nil {
			t.Fatal(err)
		}
		turn.Session = sess.ID
		if turn.Kind == agent.KindSubturn {
			parent, _ := a.Runner.NewSession(ctx, "habakkuk")
			turn.Sub = &agent.SubturnSpec{Parent: parent.ID, Task: "go"}
		}
		id, err := a.Runner.SubmitTurn(ctx, turn)
		if err != nil {
			t.Fatal(err)
		}
		kind := turn.Kind
		if kind == "" {
			kind = agent.KindTurn
		}
		j, err := a.Queue.Claim(ctx, "w", time.Minute, kind)
		if err != nil || j == nil || j.ID != id {
			t.Fatalf("claim: %v %v", j, err)
		}
		final, err := a.Runner.HandleTurn(ctx, j, "w")
		if err != nil {
			t.Fatal(err)
		}
		a.Queue.Complete(ctx, j.ID, "w", final)
		return final
	}
	for name, c := range map[string]struct {
		turn agent.Turn
		want bool
	}{
		"typed here":                {agent.Turn{Text: "go", Local: true}, true},
		"owner's schedule, no chat": {agent.Turn{Text: "go", Background: true, Source: agent.SourceSchedule}, true},
		"API or MCP request":        {agent.Turn{Text: "go"}, false},
		"board card run":            {agent.Turn{Text: "go", Background: true, Source: agent.SourceBoard}, false},
		"meeting notice":            {agent.Turn{Text: "[meeting ended] go", Background: true}, false},
		"sub-agent task":            {agent.Turn{Kind: agent.KindSubturn, Text: "go"}, false},
		"A2A task":                  {agent.Turn{Text: "go", Source: agent.SourceA2A}, false},
		"steered schedule":          {agent.Turn{Text: "go", Background: true, Source: agent.SourceSchedule, Tainted: true}, false},
	} {
		final := run(c.turn)
		if got := strings.Contains(final, "saved approval rule"); got != c.want {
			t.Errorf("%s: rule answered=%v, want %v (%q)", name, got, c.want, final)
		}
	}
}
