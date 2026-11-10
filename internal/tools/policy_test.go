package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// countingApprover answers yes or no and counts the questions.
type countingApprover struct {
	yes  bool
	asks int
	last ApprovalRequest
}

func (a *countingApprover) Approve(_ context.Context, req ApprovalRequest) (bool, error) {
	a.asks++
	a.last = req
	return a.yes, nil
}

// allowAll is a reviewer that approves everything.
type allowAll struct{ reviews int }

func (r *allowAll) Review(context.Context, ApprovalRequest) Review {
	r.reviews++
	return Review{Verdict: VerdictAllow, Reason: "looks fine"}
}

func TestTaintedTurnNeedsHumanForExecEvenInFullMode(t *testing.T) {
	exec := map[string]any{"command": "true"}
	cases := []struct {
		name     string
		approver *countingApprover
		policy   *Policy
		allowed  bool
		asks     int
	}{
		{"nobody attending", nil, nil, false, 0},
		{"human declines", &countingApprover{yes: false}, nil, false, 1},
		{"human approves", &countingApprover{yes: true}, nil, true, 1},
		{"opted in", nil, &Policy{AllowTaintedExec: true}, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var ap Approver
			if c.approver != nil {
				ap = c.approver
			}
			r, env := setup(t, ModeFull, ap)
			env.Taint, env.Policy = NewTaint(true), c.policy
			res := r.Execute(context.Background(), env, call("exec", exec))
			if res.IsError == c.allowed {
				t.Fatalf("allowed=%v, got %+v", c.allowed, res)
			}
			if c.approver != nil && c.approver.asks != c.asks {
				t.Fatalf("asked %d times, want %d", c.approver.asks, c.asks)
			}
		})
	}
}

func TestTaintedTurnReviewerCannotApproveAlone(t *testing.T) {
	r, env := setup(t, ModeWorkspace, nil)
	rev := &allowAll{}
	env.Taint, env.Approvals, env.Reviewer = NewTaint(true), ApproveReviewer, rev
	res := r.Execute(context.Background(), env, call("exec", map[string]any{"command": "true"}))
	if !res.IsError || !strings.Contains(res.Text, "outside content") {
		t.Fatalf("tainted exec ran on the reviewer's word: %+v", res)
	}
	if rev.reviews != 0 {
		t.Fatal("the reviewer was consulted for a tainted exec")
	}
	// Untainted, the reviewer decides as before.
	env.Taint = NewTaint(false)
	if res := r.Execute(context.Background(), env, call("exec", map[string]any{"command": "true"})); res.IsError {
		t.Fatalf("untainted exec with an approving reviewer: %+v", res)
	}
}

func TestTaintedTurnWrites(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.txt")
	r, env := setup(t, ModeFull, nil)
	env.Taint = NewTaint(true)
	// Inside the workspace a tainted turn writes as the mode says.
	if res := r.Execute(context.Background(), env, call("write_file", map[string]any{"path": "a.txt", "content": "x"})); res.IsError {
		t.Fatalf("write inside the workspace: %+v", res)
	}
	res := r.Execute(context.Background(), env, call("write_file", map[string]any{"path": outside, "content": "x"}))
	if !res.IsError {
		t.Fatal("tainted full-mode write outside the workspace ran unattended")
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("the file was written")
	}
	// Untainted full mode writes anywhere, as before.
	env.Taint = NewTaint(false)
	if res := r.Execute(context.Background(), env, call("write_file", map[string]any{"path": outside, "content": "x"})); res.IsError {
		t.Fatalf("untainted full-mode write: %+v", res)
	}
}

func TestNetworkReadTaintsThenExecNeedsHuman(t *testing.T) {
	r, env := setup(t, ModeFull, nil)
	r.Register("t", riskTool{"fetch_page", RiskNetwork})
	env.Taint = NewTaint(false)
	if res := r.Execute(context.Background(), env, call("exec", map[string]any{"command": "true"})); res.IsError {
		t.Fatalf("exec before any outside content: %+v", res)
	}
	r.Execute(context.Background(), env, call("fetch_page", map[string]any{}))
	if res := r.Execute(context.Background(), env, call("exec", map[string]any{"command": "true"})); !res.IsError {
		t.Fatal("exec after reading a page ran without a human")
	}
}

func TestChannelTurnNeedsApproval(t *testing.T) {
	exec := map[string]any{"command": "true"}
	ap := &countingApprover{yes: true}
	r, env := setup(t, ModeFull, ap)
	env.Policy = &Policy{Channel: "tg"}
	if res := r.Execute(context.Background(), env, call("exec", exec)); res.IsError || ap.asks != 1 {
		t.Fatalf("channel exec in full mode: asks=%d %+v", ap.asks, res)
	}
	if !strings.Contains(ap.last.Summary, "via channel tg") {
		t.Fatalf("approval summary does not name the channel: %q", ap.last.Summary)
	}
	// Unattended, it is denied.
	env.Approver = nil
	if res := r.Execute(context.Background(), env, call("exec", exec)); !res.IsError {
		t.Fatal("unattended channel exec ran")
	}
	// The owner opted in: exec runs as the mode says.
	env.Policy = &Policy{Channel: "tg", Allowed: map[Capability]bool{CapExec: true}}
	if res := r.Execute(context.Background(), env, call("exec", exec)); res.IsError {
		t.Fatalf("opted-in channel exec: %+v", res)
	}
	// Writes inside the workspace need nothing.
	env.Policy = &Policy{Channel: "tg"}
	if res := r.Execute(context.Background(), env, call("write_file", map[string]any{"path": "a.txt", "content": "x"})); res.IsError {
		t.Fatalf("channel write inside the workspace: %+v", res)
	}
}

func TestGuestNeverGetsRiskyTools(t *testing.T) {
	ap := &countingApprover{yes: true}
	r, env := setup(t, ModeFull, ap)
	r.Register("computer", riskTool{"computer_screenshot", RiskRead})
	env.Policy = &Policy{Channel: "wa", Guest: true, Allowed: map[Capability]bool{CapExec: true, CapComputer: true, CapWrite: true}, AllowTaintedExec: true}
	outside := filepath.Join(t.TempDir(), "x")
	for _, c := range []struct {
		tool  string
		input map[string]any
	}{
		{"exec", map[string]any{"command": "true"}},
		{"computer_screenshot", map[string]any{}},
		{"write_file", map[string]any{"path": outside, "content": "x"}},
	} {
		res := r.Execute(context.Background(), env, call(c.tool, c.input))
		if !res.IsError || !strings.Contains(res.Text, "other than the owner") {
			t.Fatalf("guest %s: %+v", c.tool, res)
		}
	}
	if ap.asks != 0 {
		t.Fatal("a guest's call was put to the approver")
	}
	// Reading stays open to guests.
	if res := r.Execute(context.Background(), env, call("list_dir", map[string]any{})); res.IsError {
		t.Fatalf("guest list_dir: %+v", res)
	}
}

func TestChannelComputerReadAsksEvenInReadOnly(t *testing.T) {
	ap := &countingApprover{yes: false}
	r, env := setup(t, ModeReadOnly, ap)
	r.Register("computer", riskTool{"computer_screenshot", RiskRead})
	env.Policy = &Policy{Channel: "tg"}
	if res := r.Execute(context.Background(), env, call("computer_screenshot", map[string]any{})); !res.IsError || ap.asks != 1 {
		t.Fatalf("channel screenshot: asks=%d %+v", ap.asks, res)
	}
}

func TestLocalTurnUnchanged(t *testing.T) {
	r, env := setup(t, ModeFull, nil)
	r.Register("computer", riskTool{"computer_screenshot", RiskRead})
	env.Taint = NewTaint(false)
	for _, name := range []string{"exec", "computer_screenshot"} {
		in := map[string]any{}
		if name == "exec" {
			in["command"] = "true"
		}
		if res := r.Execute(context.Background(), env, call(name, in)); res.IsError {
			t.Fatalf("local full-mode %s: %+v", name, res)
		}
	}
	// Computer actions are exec risk but their own capability: a screenshot
	// taints the turn, and the next click still runs as the mode says.
	r.Register("computer", riskTool{"computer_click", RiskExec})
	env.Taint = NewTaint(true)
	if res := r.Execute(context.Background(), env, call("computer_click", map[string]any{})); res.IsError {
		t.Fatalf("tainted local computer click: %+v", res)
	}
}

// The approver is told why it is asked, so a chat can say it in its own
// words instead of showing the policy's sentence.
func TestApprovalRequestSaysWhy(t *testing.T) {
	exec := call("exec", map[string]any{"command": "true"})
	ap := &countingApprover{yes: true}
	r, env := setup(t, ModeFull, ap)
	env.Taint = NewTaint(true)
	r.Execute(context.Background(), env, exec)
	if ap.last.Cause != CauseTainted || !ap.last.Tainted {
		t.Fatalf("tainted exec: cause %q, tainted %v", ap.last.Cause, ap.last.Tainted)
	}
	r, env = setup(t, ModeFull, ap)
	env.Policy = &Policy{Channel: "slack"}
	r.Execute(context.Background(), env, exec)
	if ap.last.Cause != CauseChannel {
		t.Fatalf("channel exec: cause %q", ap.last.Cause)
	}
	r, env = setup(t, ModeAsk, ap)
	r.Execute(context.Background(), env, exec)
	if ap.last.Cause != "" {
		t.Fatalf("ask mode: cause %q, want none", ap.last.Cause)
	}
}
