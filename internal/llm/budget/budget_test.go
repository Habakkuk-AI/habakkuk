package budget

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/usage"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

var toronto, _ = time.LoadLocation("America/Toronto")

// priced is a model at $1 per million input tokens, so 1M input tokens
// cost $1.00.
var priced = llm.Target{Endpoint: "p", Model: "cheap-1"}

func dollars(v float64) llm.Usage { return llm.Usage{InputTokens: int64(v * 1e6)} }

type rig struct {
	t     *Tracker
	mu    sync.Mutex
	now   time.Time
	warns []string
}

func (r *rig) at(tm time.Time) {
	r.mu.Lock()
	r.now = tm
	r.mu.Unlock()
}

func (r *rig) warnings() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.warns...)
}

func newRig(t *testing.T, cfg *config.Config) *rig {
	t.Helper()
	if cfg.Timezone == "" {
		cfg.Timezone = "America/Toronto"
	}
	cfg.Pricing = map[string]*config.Price{"cheap-1": {Input: 1}}
	r := &rig{now: time.Date(2026, 11, 9, 12, 0, 0, 0, toronto)}
	r.t = New(cfg, usage.NewPricer(cfg), quiet)
	r.t.Now = func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now }
	r.t.Warn = func(_ context.Context, agent, key, text string) {
		r.mu.Lock()
		r.warns = append(r.warns, key+" "+text)
		r.mu.Unlock()
	}
	return r
}

func caller(agent, turn string) context.Context {
	return llm.WithCaller(context.Background(), llm.Caller{Agent: agent, Session: "s", Turn: turn})
}

func budgetErr(t *testing.T, err error) *llm.BudgetError {
	t.Helper()
	var be *llm.BudgetError
	if !errors.As(err, &be) {
		t.Fatalf("want a BudgetError, got %v", err)
	}
	return be
}

func agentBudget(b *config.Budget) map[string]*config.Agent {
	return map[string]*config.Agent{"main": {Budget: b}, "helper": {}}
}

func TestDailyCapBlocksTheNextCall(t *testing.T) {
	r := newRig(t, &config.Config{Agents: agentBudget(&config.Budget{Daily: config.Dollar(5)})})
	ctx := caller("main", "")
	for range 4 {
		if err := r.t.Check(ctx, priced); err != nil {
			t.Fatal(err)
		}
		r.t.Record(ctx, priced, dollars(1.25))
	}
	// $5.00 spent: the cap is reached and nothing more is sent.
	be := budgetErr(t, r.t.Check(ctx, priced))
	want := "Paused: the daily budget of $5.00 for agent main is used up; it resets at 00:00 America/Toronto. Raise it with agents.main.budget.daily"
	if !strings.HasPrefix(be.Message, want) {
		t.Fatalf("message %q", be.Message)
	}
	if be.Key != "agent:main:daily:2026-11-09" || be.Unpriced {
		t.Fatalf("key %q", be.Key)
	}
	if llm.Classify(be).Kind != llm.ErrBudget || llm.Classify(be).Retry {
		t.Fatal("a budget error must not be retried")
	}
	// Other agents have no cap of their own and no install cap applies.
	if err := r.t.Check(caller("helper", ""), priced); err != nil {
		t.Fatal(err)
	}
	if r.t.Blocked("main") == nil || r.t.Blocked("helper") != nil {
		t.Fatal("Blocked disagrees with Check")
	}
}

func TestInstallMonthlyCapCoversEveryAgent(t *testing.T) {
	r := newRig(t, &config.Config{Budget: &config.Budget{Monthly: config.Dollar(3)}, Agents: agentBudget(nil)})
	r.t.Record(caller("main", ""), priced, dollars(2))
	r.t.Record(caller("helper", ""), priced, dollars(1))
	be := budgetErr(t, r.t.Check(caller("helper", ""), priced))
	if !strings.Contains(be.Message, "monthly budget of $3.00 for this install") ||
		!strings.Contains(be.Message, "resets on Dec 1 at 00:00 America/Toronto") ||
		!strings.Contains(be.Message, "budget.monthly in config.yaml") {
		t.Fatalf("message %q", be.Message)
	}
	// A call with no agent (outside any turn) counts too, and is refused.
	if err := r.t.Check(context.Background(), priced); err == nil {
		t.Fatal("unattributed call allowed past the install cap")
	}
}

func TestPerTurnCapIncludesSubTurns(t *testing.T) {
	cfg := &config.Config{Agents: map[string]*config.Agent{
		"main":   {Budget: &config.Budget{PerTurn: config.Dollar(1), Daily: config.Dollar(100)}},
		"helper": {Budget: &config.Budget{Daily: config.Dollar(100)}},
	}}
	r := newRig(t, cfg)
	parent := caller("main", "job_1")
	if err := r.t.Check(parent, priced); err != nil {
		t.Fatal(err)
	}
	r.t.Record(parent, priced, dollars(0.6))
	// The sub-agent runs as another agent but spends from the same turn.
	sub := caller("helper", "job_1")
	if err := r.t.Check(sub, priced); err != nil {
		t.Fatal(err)
	}
	r.t.Record(sub, priced, dollars(0.5))
	if r.t.TurnSpent("job_1") < 1.1-1e-9 {
		t.Fatalf("turn spent %v", r.t.TurnSpent("job_1"))
	}
	for _, ctx := range []context.Context{parent, sub} {
		be := budgetErr(t, r.t.Check(ctx, priced))
		if !strings.HasPrefix(be.Message, "Stopped: this turn used its budget of $1.00 (agents.main.budget.per_turn)") {
			t.Fatalf("message %q", be.Message)
		}
	}
	// Daily spend is counted toward the agent that ran each call.
	rep := r.t.Report([]string{"main", "helper"})
	for _, s := range rep.Agents {
		want := map[string]float64{"main": 0.6, "helper": 0.5}[s.Agent]
		if diff := s.Today.Spent - want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s spent %v today, want %v", s.Agent, s.Today.Spent, want)
		}
	}
	// The next turn starts with a fresh allowance.
	if err := r.t.Check(caller("main", "job_2"), priced); err != nil {
		t.Fatal(err)
	}
}

func TestWarningAt80PercentOncePerPeriod(t *testing.T) {
	r := newRig(t, &config.Config{Agents: agentBudget(&config.Budget{Daily: config.Dollar(10)})})
	ctx := caller("main", "")
	r.t.Record(ctx, priced, dollars(7.9))
	if len(r.warnings()) != 0 {
		t.Fatal("warned below 80%")
	}
	r.t.Record(ctx, priced, dollars(0.2))
	r.t.Record(ctx, priced, dollars(0.5))
	w := r.warnings()
	if len(w) != 1 || !strings.Contains(w[0], "Heads up: $8.10 of the daily budget of $10.00 for agent main is used (81%)") {
		t.Fatalf("warnings %q", w)
	}
	// The next day starts over and warns again once.
	r.at(time.Date(2026, 11, 10, 9, 0, 0, 0, toronto))
	r.t.Record(ctx, priced, dollars(9))
	r.t.Record(ctx, priced, dollars(0.1))
	if w := r.warnings(); len(w) != 2 || !strings.Contains(w[1], "daily:2026-11-10") {
		t.Fatalf("warnings %q", w)
	}
}

func TestPeriodsRollAtMidnightInTheConfiguredTimezone(t *testing.T) {
	r := newRig(t, &config.Config{Agents: agentBudget(&config.Budget{Daily: config.Dollar(1), Monthly: config.Dollar(50)})})
	ctx := caller("main", "")
	// 18:00 in Toronto is 23:00 UTC.
	r.at(time.Date(2026, 11, 9, 18, 0, 0, 0, toronto))
	r.t.Record(ctx, priced, dollars(1))
	// 01:00 UTC on the 10th is still the 9th in Toronto.
	r.at(time.Date(2026, 11, 10, 1, 0, 0, 0, time.UTC))
	if err := r.t.Check(ctx, priced); err == nil {
		t.Fatal("the day rolled at UTC midnight")
	}
	r.at(time.Date(2026, 11, 10, 0, 0, 1, 0, toronto))
	if err := r.t.Check(ctx, priced); err != nil {
		t.Fatalf("the day did not roll at Toronto midnight: %v", err)
	}
	if rep := r.t.Report([]string{"main"}); rep.Agents[0].Month.Spent != 1 || rep.Day != "2026-11-10" {
		t.Fatalf("month spend %+v", rep.Agents[0])
	}
	// The month rolls on the 1st, Toronto time.
	r.t.Record(ctx, priced, dollars(0.5))
	r.at(time.Date(2026, 12, 1, 0, 30, 0, 0, toronto))
	if rep := r.t.Report([]string{"main"}); rep.Agents[0].Month.Spent != 0 || rep.Month != "2026-12" {
		t.Fatalf("month did not roll: %+v", rep)
	}
}

func TestLoadCountsThisMonthFromUsageDB(t *testing.T) {
	ctx := context.Background()
	st, err := usage.Open(ctx, filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, c := range []usage.Call{
		{At: time.Date(2026, 10, 31, 23, 0, 0, 0, toronto), Agent: "main"}, // last month
		{At: time.Date(2026, 11, 3, 10, 0, 0, 0, toronto), Agent: "main"},
		{At: time.Date(2026, 11, 9, 8, 0, 0, 0, toronto), Agent: "main"},
		{At: time.Date(2026, 11, 9, 9, 0, 0, 0, toronto), Agent: ""},
	} {
		c.Provider, c.Model, c.Usage = priced.Endpoint, priced.Model, dollars(1)
		if err := st.Record(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	r := newRig(t, &config.Config{Budget: &config.Budget{Daily: config.Dollar(2)}, Agents: agentBudget(nil)})
	if err := r.t.Load(ctx, st); err != nil {
		t.Fatal(err)
	}
	rep := r.t.Report([]string{"main"})
	if rep.Install.Today.Spent != 2 || rep.Install.Month.Spent != 3 || rep.Agents[0].Today.Spent != 1 || rep.Agents[0].Month.Spent != 2 {
		t.Fatalf("report %+v", rep)
	}
	if rep.Install.Paused == "" || r.t.Check(caller("main", ""), priced) == nil {
		t.Fatal("loaded spend did not reach the install's daily cap")
	}
}

func TestUnknownPrices(t *testing.T) {
	unknown := llm.Target{Endpoint: "p", Model: "mystery-9"}
	// allow (default): counted as $0, one warning in the log per model.
	r := newRig(t, &config.Config{Agents: agentBudget(&config.Budget{Daily: config.Dollar(1)})})
	ctx := caller("main", "")
	r.t.Record(ctx, unknown, dollars(100))
	if err := r.t.Check(ctx, unknown); err != nil {
		t.Fatal(err)
	}
	if !r.t.unpriced[unknown.String()] {
		t.Fatal("no warning recorded for the unpriced model")
	}
	// block: refused while a cap applies; another model may still run.
	cfg := &config.Config{Budget: &config.Budget{Daily: config.Dollar(10), UnknownPrice: "block"}, Agents: agentBudget(nil)}
	r = newRig(t, cfg)
	be := budgetErr(t, r.t.Check(ctx, unknown))
	if !be.Unpriced || !strings.Contains(be.Message, "p/mystery-9 has no known price") {
		t.Fatalf("%+v", be)
	}
	if err := r.t.Check(ctx, priced); err != nil {
		t.Fatal(err)
	}
}

// fixed answers every call, counting them.
type fixed struct {
	mu    sync.Mutex
	calls map[string]int
}

func (f *fixed) Stream(_ context.Context, _ string, req llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	f.mu.Lock()
	f.calls[req.Model]++
	f.mu.Unlock()
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "ok"}}},
		Usage: dollars(1)}, nil
}

func TestRouterGate(t *testing.T) {
	cfg := &config.Config{Budget: &config.Budget{Daily: config.Dollar(2), UnknownPrice: "block"}, Agents: agentBudget(nil)}
	r := newRig(t, cfg)
	prov := &fixed{calls: map[string]int{}}
	router := &llm.Router{Endpoints: map[string]*llm.Endpoint{"p": {Name: "p", Provider: prov,
		Credentials: []llm.Credential{{Name: "k", Key: "k"}}}}, Log: quiet, Gate: r.t.Check, OnUsage: r.t.Record}
	chain := []llm.Target{{Endpoint: "p", Model: "mystery-9"}, priced}
	ctx := caller("main", "")
	for range 2 {
		resp, err := router.Send(ctx, chain, llm.Request{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Model != priced.String() {
			t.Fatalf("answered by %s; the unpriced model should be skipped", resp.Model)
		}
	}
	_, err := router.Send(ctx, chain, llm.Request{}, nil)
	if be := budgetErr(t, err); be.Unpriced {
		t.Fatal("want the daily cap's error")
	}
	if prov.calls["mystery-9"] != 0 || prov.calls["cheap-1"] != 2 {
		t.Fatalf("provider calls %v", prov.calls)
	}
}

func TestNoticeOncePerKey(t *testing.T) {
	r := newRig(t, &config.Config{})
	if !r.t.Notice("agent:main:daily:2026-11-09") || r.t.Notice("agent:main:daily:2026-11-09") {
		t.Fatal("notice not given exactly once")
	}
	if !r.t.Notice("agent:main:daily:2026-11-10") {
		t.Fatal("a new period gets its own notice")
	}
}

func TestConcurrentChecksAndRecords(t *testing.T) {
	r := newRig(t, &config.Config{Budget: &config.Budget{Daily: config.Dollar(1000), PerTurn: config.Dollar(1000)},
		Agents: agentBudget(&config.Budget{Monthly: config.Dollar(1000)})})
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			agent := []string{"main", "helper"}[g%2]
			ctx := caller(agent, "job_"+agent)
			for range 100 {
				_ = r.t.Check(ctx, priced)
				r.t.Record(ctx, priced, dollars(0.25))
				_ = r.t.Report([]string{"main"})
				_ = r.t.Blocked(agent)
			}
		}()
	}
	wg.Wait()
	rep := r.t.Report(nil)
	if rep.Install.Today.Spent != 400 {
		t.Fatalf("install spent %v, want 400", rep.Install.Today.Spent)
	}
	if r.t.TurnSpent("job_main")+r.t.TurnSpent("job_helper") != 400 {
		t.Fatal("turn spend lost updates")
	}
}

func TestReportText(t *testing.T) {
	r := newRig(t, &config.Config{Budget: &config.Budget{Daily: config.Dollar(5)}, Agents: agentBudget(&config.Budget{PerTurn: config.Dollar(0.5)})})
	r.t.Record(caller("main", ""), priced, dollars(5))
	rep := r.t.Report([]string{"main", "helper"})
	var b strings.Builder
	rep.Write(&b)
	out := b.String()
	for _, want := range []string{"America/Toronto", "install", "$5.00 / $5.00 (100%)", "agent main", "$0.50", "Paused: the daily budget of $5.00 for this install"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	brief := rep.Brief("main")
	if !strings.Contains(brief, "Agent main: today $5.00, this month $5.00, $0.50 per turn.") || !strings.Contains(brief, "Paused:") {
		t.Fatalf("brief:\n%s", brief)
	}
}
