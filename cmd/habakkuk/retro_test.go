package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/app"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/retro"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

type cliRetroModel struct {
	mu     sync.Mutex
	inputs []string
}

const cliRetroPlan = `{"summary": "One turn failed on a bad key.",
  "went_badly": [{"what": "A turn failed three times", "cause": "The provider key was revoked", "evidence": ["job:turn_failed"]}],
  "actions": [
    {"kind": "memory", "target": "Provider keys", "change": "Check the key before long jobs.", "why": "a revoked key failed a turn", "risk": "low", "evidence": ["job:turn_failed"]},
    {"kind": "config", "target": "agents.habakkuk.fallbacks", "change": "[openrouter/some-model]", "why": "no working fallback", "risk": "medium"}
  ]}`

func (m *cliRetroModel) Send(_ context.Context, _ []llm.Target, req llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inputs = append(m.inputs, req.Messages[0].Text())
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: cliRetroPlan}}}}, nil
}

func TestRetroCLI(t *testing.T) {
	ctx := context.Background()
	home := writeHome(t, "")
	// A week with one failed turn, written as the server would.
	a, err := app.Open(ctx, home, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Queue.Enqueue(ctx, jobs.NewJob{ID: "turn_failed", Kind: "turn", Agent: "habakkuk", Payload: map[string]string{"text": "deploy it"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Control.ExecContext(ctx, `UPDATE jobs SET status = 'failed', attempts = 3, last_error = 'auth: key revoked' WHERE id = 'turn_failed'`); err != nil {
		t.Fatal(err)
	}
	a.Close()

	model := &cliRetroModel{}
	retroModels = func(*app.App) retro.Sender { return model }
	t.Cleanup(func() { retroModels = nil })
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := retroCmd(ctx, home, args, &out)
		return out.String(), err
	}

	// A dry run prints the evidence and the plan and changes nothing.
	out, err := run("-dry-run", "-days", "3")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Evidence", "job:turn_failed", "auth: key revoked", "# Plan", "A turn failed three times",
		"### 1. memory `Provider keys` [proposed]", "Dry run: nothing was stored, changed or sent."} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hk retro apply dry-run") {
		t.Error("dry run offers to apply")
	}
	if len(model.inputs) != 1 {
		t.Fatalf("%d model calls", len(model.inputs))
	}
	mem, err := memory.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	mem.DB().QueryRow(`SELECT count(*) FROM retros`).Scan(&n)
	mem.Close()
	if n != 0 {
		t.Fatalf("dry run stored %d retros", n)
	}
	if _, err := os.Stat(filepath.Join(home, "agents", "habakkuk", "retros")); !os.IsNotExist(err) {
		t.Fatal("dry run wrote a report")
	}

	// A real run stores it, writes the report and proposes.
	out, err = run("-agent", "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	day := time.Now().Format("2006-01-02")
	if !strings.Contains(out, "## Proposals") || !strings.Contains(out, "Report: agents/habakkuk/retros/"+day+".md") {
		t.Fatalf("run:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, "agents", "habakkuk", "retros", day+".md")); err != nil {
		t.Fatal(err)
	}
	out, err = run("list")
	if err != nil || !strings.Contains(out, "retro_habakkuk_") || !strings.Contains(out, "1 went badly, 2 proposed") {
		t.Fatalf("list: %q %v", out, err)
	}
	id := strings.Fields(out)[0]
	if out, err = run("show", id); err != nil || !strings.Contains(out, "## Evidence") {
		t.Fatalf("show: %v\n%s", err, out)
	}
	out, err = run("apply", "latest", "1", "2")
	if err != nil || !strings.Contains(out, "1. memory Provider keys: applied") || !strings.Contains(out, "2. config agents.habakkuk.fallbacks: approved") ||
		!strings.Contains(out, "config.yaml") {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if out, err = run("reject", id, "2"); err != nil || !strings.Contains(out, "rejected") {
		t.Fatalf("reject: %v %s", err, out)
	}
	for _, args := range [][]string{{"apply", id}, {"apply", id, "9"}, {"reject", id, "1"}, {"show"}, {"dance"}, {"-days", "0"}, {"show", "retro_nope"}} {
		if _, err := run(args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}
