package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/budget"
	"github.com/habakkuk-ai/habakkuk/internal/llm/usage"
)

// dollarProvider serves a test Sender as a provider, each answer costing
// $1 (1M input tokens of a model priced at $1 per million).
type dollarProvider struct{ s Sender }

func (p dollarProvider) Stream(ctx context.Context, _ string, req llm.Request, on func(llm.Event)) (*llm.Response, error) {
	resp, err := p.s.Send(ctx, nil, req, on)
	if resp != nil {
		resp.Usage = llm.Usage{InputTokens: 1_000_000}
	}
	return resp, err
}

// newBudgetHarness runs the harness's turns through a real router guarded
// by a budget tracker with the agent's limits b.
func newBudgetHarness(t *testing.T, model Sender, b *config.Budget) (*harness, *budget.Tracker) {
	t.Helper()
	h := newHarness(t, model)
	cfg := &config.Config{Timezone: "America/Toronto", Agents: map[string]*config.Agent{"habakkuk": {Budget: b}},
		Pricing: map[string]*config.Price{"m": {Input: 1}}}
	tr := budget.New(cfg, usage.NewPricer(cfg), h.runner.Log)
	h.runner.Models = &llm.Router{Endpoints: map[string]*llm.Endpoint{"fake": {Name: "fake", Provider: dollarProvider{model},
		Credentials: []llm.Credential{{Name: "k", Key: "k"}}}}, Log: h.runner.Log, Gate: tr.Check, OnUsage: tr.Record}
	h.runner.Budget = tr
	return h, tr
}

// pool runs turn and sub-turn jobs until the test ends.
func (h *harness) pool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var pools []*jobs.Pool
	for _, kind := range []string{KindTurn, KindSubturn} {
		p := &jobs.Pool{Queue: h.queue, Handlers: map[string]jobs.Handler{kind: h.runner.HandleTurn},
			Concurrency: 2, Lease: 5 * time.Second, Poll: 20 * time.Millisecond, Log: h.runner.Log}
		pools = append(pools, p)
		go p.Run(ctx)
	}
	h.runner.Wake = func() {
		for _, p := range pools {
			p.Wake()
		}
	}
}

func (h *harness) finish(t *testing.T, id string) (string, *jobs.Job) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	final, err := h.runner.wait(ctx, id)
	if err != nil {
		t.Fatalf("job %s: %v", id, err)
	}
	j, _ := h.queue.Get(ctx, id)
	return final, j
}

func (f *fakeModel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// A turn that meets a used-up daily limit finishes once with the limit's
// message: no error, no retries, no further model calls.
func TestDailyBudgetPausesTurnsWithoutRetrying(t *testing.T) {
	model := &fakeModel{}
	h, _ := newBudgetHarness(t, model, &config.Budget{Daily: config.Dollar(2)})
	h.pool(t)
	ctx := context.Background()
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	id, _ := h.runner.Submit(ctx, s.ID, "what's in my workspace?")
	h.runner.Wake()
	if final, _ := h.finish(t, id); !strings.Contains(final, "hello.txt") {
		t.Fatalf("first turn: %q", final)
	}
	if model.count() != 2 {
		t.Fatalf("model calls %d", model.count())
	}
	for range 2 { // every message the user sends gets the answer
		id, _ = h.runner.Submit(ctx, s.ID, "and now?")
		h.runner.Wake()
		final, j := h.finish(t, id)
		if !strings.HasPrefix(final, "Paused: the daily budget of $2.00 for agent habakkuk is used up; it resets at 00:00 America/Toronto.") {
			t.Fatalf("final %q", final)
		}
		if j.Status != jobs.Done || j.Attempts != 1 {
			t.Fatalf("job %s after %d attempt(s), want done after 1", j.Status, j.Attempts)
		}
	}
	if model.count() != 2 {
		t.Fatalf("model called %d times; a paused turn must not reach it", model.count())
	}
	msgs := h.transcript(t, s.ID)
	last := msgs[len(msgs)-1]
	if last.Role != llm.Assistant || !strings.HasPrefix(last.Text(), "Paused:") || msgs[len(msgs)-2].Role != llm.User {
		t.Fatalf("transcript ends %v %q", last.Role, last.Text())
	}
}

// loopModel asks for a tool on every call.
type loopModel struct{ fakeModel }

func (m *loopModel) Send(_ context.Context, _ []llm.Target, _ llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{
		{Type: llm.ToolUse, ToolUseID: "c", ToolName: "list_dir", Input: json.RawMessage(`{}`)},
	}}, StopReason: llm.StopToolUse}, nil
}

func TestPerTurnBudgetStopsTheToolLoop(t *testing.T) {
	model := &loopModel{}
	h, _ := newBudgetHarness(t, model, &config.Budget{PerTurn: config.Dollar(1.5)})
	ctx := context.Background()
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	for turn := 1; turn <= 2; turn++ {
		h.runner.Submit(ctx, s.ID, "go")
		j, _ := h.queue.Claim(ctx, "w", time.Minute, KindTurn)
		final, err := h.runner.HandleTurn(ctx, j, "w")
		if err != nil {
			t.Fatal(err)
		}
		h.queue.Complete(ctx, j.ID, "w", final)
		if !strings.HasPrefix(final, "Stopped: this turn used its budget of $1.50 (agents.habakkuk.budget.per_turn).") {
			t.Fatalf("final %q", final)
		}
		// $1 a call: the second call takes the turn past $1.50 and the third
		// is refused. Each turn has its own allowance.
		if model.count() != 2*turn {
			t.Fatalf("after turn %d: %d model calls", turn, model.count())
		}
	}
}

// The calls of the sub-agents a turn delegates to spend from that turn's
// allowance, and from the daily spend of the agent that ran them.
func TestSubTurnsSpendFromTheParentTurn(t *testing.T) {
	b := &boss{sawTools: map[string][]string{}}
	h, tr := newBudgetHarness(t, b, &config.Budget{PerTurn: config.Dollar(2.5)})
	h.runner.RegisterDelegate(h.runner.Tools)
	h.runner.Agents["habakkuk"].Toolsets = []string{"core", "delegate"}
	h.pool(t)
	ctx := context.Background()
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	id, _ := h.runner.Submit(ctx, s.ID, "split this")
	h.runner.Wake()
	final, _ := h.finish(t, id)
	// $1 for the delegating call and $1 per sub-agent: $3 is past $2.50,
	// so the call that would read the reports is refused.
	if !strings.HasPrefix(final, "Stopped: this turn used its budget of $2.50") {
		t.Fatalf("final %q", final)
	}
	if got := tr.TurnSpent(id); got != 3 {
		t.Fatalf("turn spent %v, want 3", got)
	}
	subs, _ := h.queue.Active(ctx, KindSubturn)
	if len(subs) != 0 {
		t.Fatal("sub-turns still active")
	}
	if rep := tr.Report([]string{"habakkuk"}); rep.Agents[0].Today.Spent != 3 {
		t.Fatalf("agent spent %v today", rep.Agents[0].Today.Spent)
	}
	j, _ := h.queue.Get(ctx, subturnID(s.ID, "habakkuk", delegateInput{Task: "apples"}))
	var p TurnPayload
	if j == nil || j.Decode(&p) != nil || p.Sub == nil || p.Sub.Turn != id {
		t.Fatalf("sub-turn does not carry the parent turn: %+v", p.Sub)
	}
}

// Background turns (schedules, notices) tell the user once per limit and
// period, then stay silent.
func TestBackgroundTurnsNoticeOnce(t *testing.T) {
	model := &fakeModel{}
	h, tr := newBudgetHarness(t, model, &config.Budget{Daily: config.Dollar(1)})
	ctx := llm.WithCaller(context.Background(), llm.Caller{Agent: "habakkuk"})
	tr.Record(ctx, llm.Target{Endpoint: "fake", Model: "m"}, llm.Usage{InputTokens: 1_000_000})
	var finals []string
	for range 3 {
		s, _ := h.runner.NewSession(ctx, "habakkuk")
		h.runner.SubmitTurn(ctx, Turn{Session: s.ID, Text: "[Scheduled task]", Background: true})
		j, _ := h.queue.Claim(ctx, "w", time.Minute, KindTurn)
		final, err := h.runner.HandleTurn(ctx, j, "w")
		if err != nil {
			t.Fatal(err)
		}
		finals = append(finals, final)
	}
	if !strings.HasPrefix(finals[0], "Paused:") || finals[1] != silentReply || finals[2] != silentReply {
		t.Fatalf("finals %q", finals)
	}
	if model.count() != 0 {
		t.Fatal("model called past the limit")
	}
}
