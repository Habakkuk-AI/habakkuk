// Package budget enforces spending limits on model calls: per day and per
// month for the whole install and for each agent, and per turn.
//
// The Tracker keeps the spend of the current day and month in memory,
// loaded from usage.db at start and updated as each call finishes, so the
// check before a call is a map lookup under a mutex. Spend is counted
// toward the agent that made the call (a sub-agent's calls count toward
// the sub-agent), and toward the install. A turn's per-turn spend also
// includes the sub-turns it delegated, whichever agent ran them.
//
// A call is refused once spend has reached a limit. A call already under
// way is never cut off, so a limit can be passed by what the calls running
// at that moment cost; after that nothing more is sent.
package budget

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/usage"
)

// Limits are spending caps in US dollars; 0 is no cap.
type Limits struct {
	Daily, Monthly, PerTurn float64
}

func (l Limits) any() bool { return l.Daily > 0 || l.Monthly > 0 || l.PerTurn > 0 }

func limitsOf(b *config.Budget) Limits {
	if b == nil {
		return Limits{}
	}
	return Limits{Daily: b.Daily.Dollars, Monthly: b.Monthly.Dollars, PerTurn: b.PerTurn.Dollars}
}

// WarnAt is the share of a daily or monthly limit past which one warning
// is posted per period.
const WarnAt = 0.8

// turnTTL is how long a turn's spend is kept after its last call.
const turnTTL = 24 * time.Hour

// Tracker checks calls against the limits and counts what they cost. It is
// safe for concurrent use.
type Tracker struct {
	Pricer *usage.Pricer
	Log    *slog.Logger
	// Warn posts a warning about agent's spending, in the chat of the call
	// that crossed the line when ctx has one; key names the limit and
	// period (for a delivery ID). Nil logs it.
	Warn func(ctx context.Context, agent, key, text string)
	Now  func() time.Time // default time.Now

	mu            sync.Mutex
	global        Limits
	agents        map[string]Limits
	blockUnpriced bool
	loc           *time.Location
	day, month    time.Time           // starts of the current periods
	daily         map[string]float64  // spend today by agent; "" is the install
	monthly       map[string]float64  // spend this month, likewise
	turns         map[string]*turnUse // per-turn spend by turn key
	once          map[string]bool     // warnings and notices given, by key
	unpriced      map[string]bool     // models warned about having no price
}

type turnUse struct {
	agent string // the agent whose turn it is
	spent float64
	last  time.Time
}

// New builds a tracker for cfg's limits and timezone.
func New(cfg *config.Config, pricer *usage.Pricer, log *slog.Logger) *Tracker {
	if log == nil {
		log = slog.Default()
	}
	t := &Tracker{Pricer: pricer, Log: log, daily: map[string]float64{}, monthly: map[string]float64{},
		turns: map[string]*turnUse{}, once: map[string]bool{}, unpriced: map[string]bool{}}
	t.Configure(cfg)
	return t
}

// Configure applies cfg's limits, and on the first call its timezone;
// config reloads call it to change limits while running.
func (t *Tracker) Configure(cfg *config.Config) {
	loc := time.Local
	if cfg.Timezone != "" {
		if l, err := time.LoadLocation(cfg.Timezone); err == nil {
			loc = l
		}
	}
	agents := map[string]Limits{}
	for name, a := range cfg.Agents {
		if a != nil && a.Budget != nil {
			agents[name] = limitsOf(a.Budget)
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.global, t.agents, t.blockUnpriced = limitsOf(cfg.Budget), agents, cfg.BlockUnpriced()
	if t.loc == nil { // a new timezone takes a restart, like schedules
		t.loc = loc
	}
}

func (t *Tracker) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Load reads this month's calls from usage.db into the spend counters,
// replacing them. Call it at start, before the first model call.
func (t *Tracker) Load(ctx context.Context, st *usage.Store) error {
	t.mu.Lock()
	t.roll(t.now())
	since := t.month
	t.mu.Unlock()
	calls, err := st.Calls(ctx, usage.Filter{Since: since})
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.roll(t.now())
	clear(t.daily)
	clear(t.monthly)
	for _, c := range calls {
		cost, _ := t.Pricer.Cost(c)
		if c.At.Before(t.month) {
			continue
		}
		t.add(t.monthly, c.Agent, cost)
		if !c.At.Before(t.day) {
			t.add(t.daily, c.Agent, cost)
		}
	}
	return nil
}

func (t *Tracker) add(m map[string]float64, agent string, cost float64) {
	m[""] += cost
	if agent != "" {
		m[agent] += cost
	}
}

// roll starts a new day or month when now has passed into one. The caller
// holds mu.
func (t *Tracker) roll(now time.Time) {
	n := now.In(t.loc)
	day := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, t.loc)
	month := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, t.loc)
	if !day.Equal(t.day) {
		t.day = day
		clear(t.daily)
	}
	if !month.Equal(t.month) {
		t.month = month
		clear(t.monthly)
		clear(t.once) // warnings and notices start over each month
	}
}

// limit is one cap that applies to a call.
type limit struct {
	scope  string // "" for the install, else the agent
	period string // daily | monthly | per_turn
	amount float64
	spent  float64
}

// applicable lists the daily and monthly caps on agent's calls. The caller
// holds mu.
func (t *Tracker) applicable(agent string) []limit {
	var out []limit
	add := func(scope string, l Limits) {
		if l.Daily > 0 {
			out = append(out, limit{scope, "daily", l.Daily, t.daily[scope]})
		}
		if l.Monthly > 0 {
			out = append(out, limit{scope, "monthly", l.Monthly, t.monthly[scope]})
		}
	}
	add("", t.global)
	if agent != "" {
		add(agent, t.agents[agent])
	}
	return out
}

// perTurn is the per-turn cap on a turn of root's that agent is calling
// in: the smallest of the install's, root's and agent's. The caller holds
// mu.
func (t *Tracker) perTurn(root, agent string) (amount float64, setting string) {
	for _, c := range []struct {
		scope string
		v     float64
	}{{"", t.global.PerTurn}, {root, t.agents[root].PerTurn}, {agent, t.agents[agent].PerTurn}} {
		if c.v > 0 && (amount == 0 || c.v < amount) {
			amount, setting = c.v, t.setting(c.scope, "per_turn")
		}
	}
	return amount, setting
}

// Check is the router's gate: it refuses a call to target when a limit
// that covers the caller on ctx is used up, or when the model has no known
// price and unknown_price is block.
func (t *Tracker) Check(ctx context.Context, target llm.Target) error {
	c := llm.CallerFrom(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.roll(now)
	if err := t.blocked(c.Agent); err != nil {
		return err
	}
	if c.Turn != "" {
		tu := t.turns[c.Turn]
		if tu == nil {
			t.prune(now)
			tu = &turnUse{agent: c.Agent}
			t.turns[c.Turn] = tu
		}
		tu.last = now
		if most, setting := t.perTurn(tu.agent, c.Agent); most > 0 && tu.spent >= most {
			return &llm.BudgetError{Key: "turn:" + c.Turn, Message: fmt.Sprintf(
				"Stopped: this turn used its budget of %s (%s). Send another message to carry on, "+
					"or raise that limit in config.yaml.", Dollars(most), setting)}
		}
	}
	if t.blockUnpriced && (t.global.any() || t.agents[c.Agent].any()) {
		if _, ok := t.Pricer.Cost(usage.Call{Provider: target.Endpoint, Model: target.Model}); !ok {
			return &llm.BudgetError{Unpriced: true, Key: "unpriced:" + target.String(), Message: fmt.Sprintf(
				"Paused: %s has no known price and budget.unknown_price is block, so it is not used. "+
					"Add its price under pricing: in config.yaml, or set budget.unknown_price: allow.", target)}
		}
	}
	return nil
}

// Blocked returns the error a call by agent would get now from a daily or
// monthly limit, or nil. Background work asks before it starts anything.
func (t *Tracker) Blocked(agent string) *llm.BudgetError {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.roll(t.now())
	return t.blocked(agent)
}

func (t *Tracker) blocked(agent string) *llm.BudgetError {
	for _, l := range t.applicable(agent) {
		if l.spent >= l.amount {
			return &llm.BudgetError{Key: t.key(l), Message: t.pausedMessage(l)}
		}
	}
	return nil
}

// key names a limit in its current period.
func (t *Tracker) key(l limit) string {
	start := t.day.Format(time.DateOnly)
	if l.period == "monthly" {
		start = t.month.Format("2006-01")
	}
	scope := "install"
	if l.scope != "" {
		scope = "agent:" + l.scope
	}
	return scope + ":" + l.period + ":" + start
}

func (t *Tracker) whose(l limit) string {
	if l.scope == "" {
		return "this install"
	}
	return "agent " + l.scope
}

func (t *Tracker) setting(scope, period string) string {
	if scope == "" {
		return "budget." + period
	}
	return "agents." + scope + ".budget." + period
}

func (t *Tracker) resets(period string) string {
	if period == "monthly" {
		return "on " + t.month.AddDate(0, 1, 0).Format("Jan 2") + " at 00:00 " + t.loc.String()
	}
	return "at 00:00 " + t.loc.String()
}

func (t *Tracker) pausedMessage(l limit) string {
	return fmt.Sprintf("Paused: the %s budget of %s for %s is used up; it resets %s. "+
		"Raise it with %s in config.yaml (it applies without a restart).",
		l.period, Dollars(l.amount), t.whose(l), t.resets(l.period), t.setting(l.scope, l.period))
}

// prune forgets turns idle for a day. The caller holds mu.
func (t *Tracker) prune(now time.Time) {
	for k, tu := range t.turns {
		if now.Sub(tu.last) > turnTTL {
			delete(t.turns, k)
		}
	}
}

// Record counts a finished call (the router's OnUsage). A model with no
// known price counts as $0, with one warning per model in the log.
func (t *Tracker) Record(ctx context.Context, target llm.Target, u llm.Usage) {
	c := llm.CallerFrom(ctx)
	cost, ok := t.Pricer.Cost(usage.Call{Provider: target.Endpoint, Model: target.Model, Agent: c.Agent, Usage: u})
	t.mu.Lock()
	if !ok && !t.unpriced[target.String()] {
		t.unpriced[target.String()] = true
		t.Log.Warn("model has no known price; its calls count as $0 toward spending limits (add it under pricing: in config.yaml)",
			"model", target.String())
	}
	now := t.now()
	t.roll(now)
	t.add(t.daily, c.Agent, cost)
	t.add(t.monthly, c.Agent, cost)
	if c.Turn != "" {
		if tu := t.turns[c.Turn]; tu != nil {
			tu.spent += cost
			tu.last = now
		} else {
			t.turns[c.Turn] = &turnUse{agent: c.Agent, spent: cost, last: now}
		}
	}
	var warnings [][2]string // key, text
	for _, l := range t.applicable(c.Agent) {
		key := "warn:" + t.key(l)
		if l.spent >= WarnAt*l.amount && l.spent < l.amount && !t.once[key] {
			t.once[key] = true
			warnings = append(warnings, [2]string{key, fmt.Sprintf("Heads up: %s of the %s budget of %s for %s is used (%.0f%%); it resets %s.",
				Dollars(l.spent), l.period, Dollars(l.amount), t.whose(l), 100*l.spent/l.amount, t.resets(l.period))})
		}
	}
	t.mu.Unlock()
	for _, w := range warnings {
		if t.Warn != nil {
			t.Warn(ctx, c.Agent, w[0], w[1])
		} else {
			t.Log.Warn(w[1], "agent", c.Agent)
		}
	}
}

// Notice reports whether the pause notice for a limit and period (a
// BudgetError's Key) has not been given yet, and marks it given. Background
// work tells the user once per period, not once per attempt.
func (t *Tracker) Notice(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.once["notice:"+key] {
		return false
	}
	t.once["notice:"+key] = true
	return true
}

// TurnSpent is what a turn (its key) has spent so far.
func (t *Tracker) TurnSpent(turn string) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if tu := t.turns[turn]; tu != nil {
		return tu.spent
	}
	return 0
}

// Dollars formats an amount: $5.00, or $0.0042 below a cent.
func Dollars(v float64) string {
	if v > 0 && v < 0.01 {
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.2f", math.Round(v*100)/100)
}
