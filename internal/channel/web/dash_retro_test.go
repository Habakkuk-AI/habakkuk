package web

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/retro"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

type retroAnswer string

func (a retroAnswer) Send(context.Context, []llm.Target, llm.Request, func(llm.Event)) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: string(a)}}}}, nil
}

func TestDashRetros(t *testing.T) {
	ctx := context.Background()
	r := newDashRig(t)
	if code, _ := r.do(t, "GET", "/api/retros/habakkuk", token, "", ""); code != http.StatusNotImplemented {
		t.Fatalf("no runner: %d", code)
	}
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "control.db"), jobs.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	q := jobs.NewQueue(db)
	rr := &retro.Runner{
		Home: r.home, Mem: r.s.Dash.Memory, Queue: q,
		Models: retroAnswer(`{"summary": "One question.", "actions": [{"kind": "ask", "target": "Reviewers", "change": "Who reviews PRs?", "why": "nobody does"}]}`),
		Agents: map[string]retro.Agent{"habakkuk": {Name: "habakkuk", Chain: []llm.Target{{Endpoint: "fake", Model: "m"}},
			Dir: filepath.Join(r.home, "agents", "habakkuk")}},
	}
	to := time.Now()
	id, err := rr.Enqueue(ctx, "habakkuk", to.Add(-24*time.Hour), to, true)
	if err != nil {
		t.Fatal(err)
	}
	j, err := q.Claim(ctx, "worker:test:1:x", time.Minute, retro.Kind)
	if err != nil || j == nil {
		t.Fatalf("claim %v %v", j, err)
	}
	if _, err := rr.Handle(ctx, j, "worker:test:1:x"); err != nil {
		t.Fatal(err)
	}
	r.s.Dash.Retros = rr

	for _, p := range []string{"/api/retros/habakkuk", "/api/retros/habakkuk/latest"} {
		if code, _ := r.do(t, "GET", p, "", "", ""); code != http.StatusUnauthorized {
			t.Errorf("%s without token: %d", p, code)
		}
	}
	retros := list(r.get(t, "/api/retros/habakkuk"), "retros")
	if len(retros) != 1 {
		t.Fatalf("retros %v", retros)
	}
	first := retros[0].(map[string]any)
	if first["id"] != id || first["evidence"] != nil || len(first["actions"].([]any)) != 1 {
		t.Fatalf("listed %v", first)
	}
	for _, p := range []string{"/api/retros/habakkuk/" + id, "/api/retros/habakkuk/latest"} {
		one := r.get(t, p)
		action := one["actions"].([]any)[0].(map[string]any)
		if one["id"] != id || one["evidence"] == nil || action["status"] != retro.Proposed || action["kind"] != "ask" || one["done"] != true {
			t.Fatalf("%s: %v", p, one)
		}
	}
	if code, _ := r.do(t, "GET", "/api/retros/habakkuk/retro_nope", token, "", ""); code != http.StatusNotFound {
		t.Fatalf("unknown retro: %d", code)
	}
	if code, _ := r.do(t, "GET", "/api/retros/nobody", token, "", ""); code != http.StatusNotFound {
		t.Fatalf("unknown agent: %d", code)
	}
}
