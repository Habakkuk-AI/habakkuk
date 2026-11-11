package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"

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
// message typed here, and for nothing else: a schedule's run, an API
// request, a board card's run, a meeting notice, a sub-agent's task, an
// A2A task.
func TestLocalRuleAnswersOnlyForTheOwnersTurns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("command rules need bash")
	}
	a := openOutsideHome(t)
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
		"typed here":                {agent.Turn{Text: "go", Local: true, Typed: true}, true},
		"hk run, local not typed":   {agent.Turn{Text: "go", Local: true}, false},
		"owner's schedule, no chat": {agent.Turn{Text: "go", Background: true, Source: agent.SourceSchedule}, false},
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

// scheduleModel creates a schedule, as any request might ask it to.
type scheduleModel struct{}

func (scheduleModel) Send(_ context.Context, _ []llm.Target, req llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	for _, b := range req.Messages[len(req.Messages)-1].Blocks {
		if b.Type == llm.ToolResult {
			return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "done"}}},
				StopReason: llm.StopEnd, Model: "fake/m"}, nil
		}
	}
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{
		{Type: llm.ToolUse, ToolUseID: "s", ToolName: "schedule_create", Input: json.RawMessage(`{"name":"made","when":"@every 1h","prompt":"run echo covered"}`)}}},
		StopReason: llm.StopToolUse, Model: "fake/m"}, nil
}

// A schedule made by a turn that isn't the owner's own (an API, MCP or
// editor request here) is marked tainted, and so are its runs; one the
// owner made by typing is clean. Neither run uses rules.
func TestScheduleMadeByAnotherTurnIsTainted(t *testing.T) {
	a, _ := openWith(t, "")
	a.Runner.Models = scheduleModel{}
	ctx := context.Background()
	for _, local := range []bool{false, true} {
		sess, _ := a.Runner.NewSession(ctx, "habakkuk")
		id, err := a.Runner.SubmitTurn(ctx, agent.Turn{Session: sess.ID, Text: "make a schedule", Local: local, Typed: local})
		if err != nil {
			t.Fatal(err)
		}
		j, _ := a.Queue.Claim(ctx, "w", time.Minute, agent.KindTurn)
		if j == nil || j.ID != id {
			t.Fatal("claim")
		}
		if _, err := a.Runner.HandleTurn(ctx, j, "w"); err != nil {
			t.Fatal(err)
		}
		a.Queue.Complete(ctx, j.ID, "w", "")
		list, _ := a.Schedules.List(ctx, "")
		if len(list) == 0 {
			t.Fatal("no schedule made")
		}
		last := list[len(list)-1]
		if last.Tainted == local {
			t.Errorf("local=%v: schedule tainted=%v", local, last.Tainted)
		}
		a.Schedules.Delete(ctx, last.ID)
	}
}

// openOutsideHome opens an app whose workspace is outside its home: no
// rule covers anything in the home.
func openOutsideHome(t *testing.T) *App {
	t.Helper()
	home, ws := t.TempDir(), t.TempDir()
	cfg := strings.Replace(config.Template, "workspace: ~/habakkuk-workspace", "workspace: "+ws, 1)
	if err := store.WriteFileAtomic(filepath.Join(home, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Open(context.Background(), home, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

// Nothing in Habakkuk's home is covered by a rule: not a command run there,
// not a file there. Its config.yaml, .env and databases always ask.
func TestNoRuleInTheHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("command rules need bash")
	}
	a, ws := openWith(t, "") // this workspace is inside the home
	a.Runner.Models = echoModel{}
	ctx := context.Background()
	if _, err := a.Rules("habakkuk").Save(tools.Rule{Tool: "exec", Exact: "echo covered", Place: tools.PlaceLocal}); err != nil {
		t.Fatal(err)
	}
	sess, _ := a.Runner.NewSession(ctx, "habakkuk")
	id, _ := a.Runner.SubmitTurn(ctx, agent.Turn{Session: sess.ID, Text: "go", Local: true, Typed: true})
	j, _ := a.Queue.Claim(ctx, "w", time.Minute, agent.KindTurn)
	if j == nil || j.ID != id {
		t.Fatal("claim")
	}
	final, _ := a.Runner.HandleTurn(ctx, j, "w")
	if strings.Contains(final, "saved approval rule") {
		t.Fatalf("a rule answered in a workspace inside the home (%s): %q", ws, final)
	}
	env := a.Runner.Env(a.Runner.Agents["habakkuk"], sess.ID)
	a.applyPolicy(agent.WithTurnInfo(ctx, agent.TurnInfo{Local: true, Typed: true}), a.Runner.Agents["habakkuk"], nil, env)
	env.Mode = tools.ModeFull
	ap := &countApprover{}
	env.Approver = ap
	for _, name := range []string{"config.yaml", "Config.YAML", ".env", "control.db", "state.db-wal"} {
		ap.n = 0
		res := a.Tools.Execute(ctx, env, llm.Block{Type: llm.ToolUse, ToolUseID: "w", ToolName: "write_file",
			Input: json.RawMessage(`{"path":"` + filepath.ToSlash(filepath.Join(a.Cfg.Home, name)) + `","content":"x"}`)})
		if ap.n != 1 || !res.IsError {
			t.Errorf("write %s in full mode: asked %d times: %+v", name, ap.n, res)
		}
	}
}

type countApprover struct{ n int }

func (c *countApprover) Approve(context.Context, tools.ApprovalRequest) (bool, error) {
	c.n++
	return false, nil
}
