package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/usage"
)

func TestBudgetWiredAndReloadedInPlace(t *testing.T) {
	ctx := context.Background()
	a, home, logs := openTestApp(t, "budget:\n  daily: 5\n")
	if a.Router.Gate == nil || a.Runner.Budget == nil || a.Web != nil && a.Web.Dash != nil && a.Web.Dash.Budget == nil {
		t.Fatal("spending limits not wired")
	}
	// The router refuses calls once the install's daily limit is spent.
	caller := llm.WithCaller(ctx, llm.Caller{Agent: "habakkuk", Session: "s"})
	a.Router.OnUsage(caller, llm.Target{Endpoint: "anthropic", Model: "claude-opus-5-5"}, llm.Usage{InputTokens: 1_250_000})
	if err := a.Router.Gate(caller, llm.Target{Endpoint: "anthropic", Model: "claude-opus-5-5"}); err == nil {
		t.Fatal("call allowed past the daily limit")
	}
	calls, _ := a.Usage.Calls(ctx, usage.Filter{})
	if len(calls) != 1 {
		t.Fatalf("usage recorded %d calls", len(calls))
	}
	sess, err := a.Runner.NewSession(ctx, "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	cmd, ok := a.ChatCommands()["/budget"]
	if !ok {
		t.Fatal("/budget missing")
	}
	if got := cmd.Run(ctx, sess.ID, ""); !strings.Contains(got, "Whole install: today $5.00 / $5.00 (100%)") || !strings.Contains(got, "Paused:") {
		t.Fatalf("/budget:\n%s", got)
	}

	// Raising the limit in config.yaml applies without a restart.
	w := &watcher{a: a, seen: a.fingerprints()}
	path := filepath.Join(home, "config.yaml")
	data, _ := os.ReadFile(path)
	next := strings.Replace(string(data), "budget:\n  daily: 5\n", "budget:\n  daily: 20\n", 1)
	next = strings.Replace(next, "    effort: high\n", "    effort: high\n    budget: {per_turn: 2}\n", 1)
	os.WriteFile(path, []byte(next), 0o600)
	future := time.Now().Add(time.Minute)
	os.Chtimes(path, future, future)
	w.check(ctx)
	if err := a.Router.Gate(caller, llm.Target{Endpoint: "anthropic", Model: "claude-opus-5-5"}); err != nil {
		t.Fatalf("raised limit not applied: %v", err)
	}
	if !strings.Contains(logs.String(), "spending limits reloaded") || strings.Contains(logs.String(), "restart habakkuk serve") {
		t.Fatalf("logs: %s", logs.String())
	}
	if rep := a.BudgetReport(); rep.Agents[0].PerTurn != 2 {
		t.Fatalf("per-agent limit not reloaded: %+v", rep.Agents[0])
	}
}
