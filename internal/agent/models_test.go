package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

type memChoices struct {
	mu sync.Mutex
	m  map[string]string
}

func (c *memChoices) SessionModel(_ context.Context, s string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[s], nil
}

func (c *memChoices) SetSessionModel(_ context.Context, s, ref string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[s] = ref
	return nil
}

// chainModel answers at once and records the chain and caller it got.
type chainModel struct {
	mu      sync.Mutex
	chains  [][]llm.Target
	callers []llm.Caller
}

func (m *chainModel) Send(ctx context.Context, chain []llm.Target, req llm.Request, on func(llm.Event)) (*llm.Response, error) {
	m.mu.Lock()
	m.chains = append(m.chains, chain)
	m.callers = append(m.callers, llm.CallerFrom(ctx))
	m.mu.Unlock()
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "ok"}}}, StopReason: llm.StopEnd}, nil
}

func TestModelCommandSwitchesOneSession(t *testing.T) {
	ctx := context.Background()
	model := &chainModel{}
	h := newHarness(t, model)
	h.runner.Agents["habakkuk"].Chain = []llm.Target{{Endpoint: "a", Model: "big"}, {Endpoint: "b", Model: "small"}}
	h.runner.Choices = &memChoices{m: map[string]string{}}
	h.runner.CheckModel = func(ref string) error {
		if !strings.HasPrefix(ref, "a/") && !strings.HasPrefix(ref, "b/") {
			return errors.New("unknown provider")
		}
		return nil
	}
	s1, _ := h.runner.NewSession(ctx, "habakkuk")
	s2, _ := h.runner.NewSession(ctx, "habakkuk")

	if got := h.runner.ModelCommand(ctx, s1.ID, "nope/x"); !strings.Contains(got, "unknown provider") {
		t.Fatalf("bad ref accepted: %s", got)
	}
	if got := h.runner.ModelCommand(ctx, s1.ID, "b/small"); !strings.Contains(got, "now uses b/small") {
		t.Fatalf("switch: %s", got)
	}
	if got := h.runner.ModelCommand(ctx, s1.ID, ""); !strings.Contains(got, "b/small → a/big") {
		t.Fatalf("show: %s", got)
	}

	run := func(sid string) {
		jobID, _ := h.runner.Submit(ctx, sid, "hi")
		j, _ := h.queue.Claim(ctx, "w", time.Minute, KindTurn)
		if j == nil || j.ID != jobID {
			t.Fatal("turn not claimable")
		}
		if _, err := h.runner.HandleTurn(ctx, j, "w"); err != nil {
			t.Fatal(err)
		}
	}
	run(s1.ID)
	run(s2.ID)
	if len(model.chains) != 2 {
		t.Fatalf("calls %d", len(model.chains))
	}
	if c := model.chains[0]; len(c) != 2 || c[0].String() != "b/small" || c[1].String() != "a/big" {
		t.Errorf("switched session chain %v", c)
	}
	if c := model.chains[1]; len(c) != 2 || c[0].String() != "a/big" {
		t.Errorf("other session chain %v", c)
	}
	if c := model.callers[0]; c.Agent != "habakkuk" || c.Session != s1.ID || c.Turn == "" {
		t.Errorf("caller %+v", model.callers[0])
	}

	if got := h.runner.ModelCommand(ctx, s1.ID, "default"); !strings.Contains(got, "own models") {
		t.Fatalf("reset: %s", got)
	}
	if chain, _ := h.runner.Chain(ctx, s1.ID); chain[0].String() != "a/big" {
		t.Fatalf("after reset %v", chain)
	}
}
