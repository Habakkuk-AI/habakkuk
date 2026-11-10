package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

type riskTool struct {
	name string
	risk Risk
}

func (t riskTool) Spec() llm.ToolSpec { return llm.ToolSpec{Name: t.name} }
func (t riskTool) Risk() Risk         { return t.risk }
func (t riskTool) Run(context.Context, *Env, json.RawMessage) (string, error) {
	return "ok", nil
}

func TestNetworkToolsTaintTheTurn(t *testing.T) {
	r := NewRegistry()
	r.Register("t", riskTool{"local_read", RiskRead})
	r.Register("t", riskTool{"fetch", RiskNetwork})
	env := &Env{Agent: "a", Workspace: t.TempDir(), Mode: ModeFull, Taint: NewTaint(false)}
	call := func(name string) {
		r.Execute(context.Background(), env, llm.Block{Type: llm.ToolUse, ToolUseID: "1", ToolName: name, Input: json.RawMessage(`{}`)})
	}
	call("local_read")
	if env.Taint.Marked() {
		t.Fatal("a local read tainted the turn")
	}
	call("fetch")
	if !env.Taint.Marked() {
		t.Fatal("network output did not taint the turn")
	}
	// A nil taint is harmless.
	var none *Taint
	none.Mark()
	if none.Marked() || !NewTaint(true).Marked() {
		t.Fatal("nil or preset taint")
	}
}

// MayTaint is what a resumed turn uses to set its taint again, so anything
// it cannot vouch for counts as outside content.
func TestMayTaintErrsOnTheSideOfYes(t *testing.T) {
	r := NewRegistry()
	r.Register("t", riskTool{"local_read", RiskRead})
	r.Register("t", riskTool{"local_exec", RiskExec})
	r.Register("t", riskTool{"fetch", RiskNetwork})
	r.Register("computer", riskTool{"computer_screenshot", RiskRead})
	for name, want := range map[string]bool{
		"local_read": false, "local_exec": false, "fetch": true,
		"computer_screenshot": true, "mcp__gone__search": true,
	} {
		if got := r.MayTaint(name); got != want {
			t.Errorf("MayTaint(%s) = %v, want %v", name, got, want)
		}
	}
}
