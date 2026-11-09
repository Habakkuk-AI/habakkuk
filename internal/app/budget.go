package app

import (
	"context"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/budget"
)

// openBudget builds the spending limits from config and this month's
// usage, and puts them in front of every model call: the router asks the
// tracker before each call (Gate) and tells it what each one cost
// (OnUsage). Every caller of the router (turns, sub-turns, titles,
// compaction, the approval reviewer, memory review, meeting notes) goes
// through it.
func (a *App) openBudget(ctx context.Context) error {
	a.Budget = budget.New(a.Cfg, a.Pricer, a.Log)
	if err := a.Budget.Load(ctx, a.Usage); err != nil {
		return err
	}
	a.Budget.Warn = a.budgetWarn
	a.Router.Gate = a.Budget.Check
	record := a.Router.OnUsage
	a.Router.OnUsage = func(ctx context.Context, t llm.Target, u llm.Usage) {
		if record != nil {
			record(ctx, t, u)
		}
		a.Budget.Record(ctx, t, u)
	}
	return nil
}

// budgetWarn posts an 80% warning in the chat of the call that crossed the
// line; with no chat (the terminal, a sub-agent) it goes to the log.
func (a *App) budgetWarn(ctx context.Context, agentName, key, text string) {
	origin := agent.OriginFrom(ctx)
	if c := llm.CallerFrom(ctx); len(origin) == 0 && c.Session != "" {
		origin = a.here(ctx, c.Session)
	}
	if a.Gateway != nil && len(origin) > 0 {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		id := "budget_" + strings.NewReplacer(":", "_", "-", "").Replace(key)
		if err := a.Gateway.Deliver(dctx, id, origin, text); err == nil {
			return
		}
	}
	a.Log.Warn(text, "agent", agentName)
}

// reloadBudget applies changed limits from a reloaded config in place.
func (a *App) reloadBudget(next *config.Config) {
	if a.Budget == nil || budgetsEqual(a.Cfg, next) {
		return
	}
	a.Budget.Configure(next)
	a.Cfg.Budget = next.Budget // only the watcher reads it after startup
	for name, ac := range a.Cfg.Agents {
		if n := next.Agents[name]; n != nil {
			ac.Budget = n.Budget
		}
	}
	a.Log.Info("spending limits reloaded")
}

func budgetsEqual(cur, next *config.Config) bool {
	if !sameBudget(cur.Budget, next.Budget) {
		return false
	}
	for name, ac := range next.Agents {
		if c := cur.Agents[name]; c != nil && !sameBudget(c.Budget, ac.Budget) {
			return false
		}
	}
	return true
}

func sameBudget(a, b *config.Budget) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// withoutBudgets copies agents with their budgets left out, so a budget
// change alone does not ask for a restart.
func withoutBudgets(agents map[string]*config.Agent) map[string]config.Agent {
	out := make(map[string]config.Agent, len(agents))
	for name, ac := range agents {
		if ac != nil {
			c := *ac
			c.Budget = nil
			out[name] = c
		}
	}
	return out
}

// BudgetReport is spend against the limits now, for every configured agent.
func (a *App) BudgetReport() budget.Report {
	return a.Budget.Report(a.Cfg.AgentNames())
}

// sessionBudget answers /budget: the conversation's agent's spend and
// limits, and the install's.
func (a *App) sessionBudget(ctx context.Context, session string) string {
	sess, err := a.Sessions.Get(ctx, session)
	if err != nil {
		return "Couldn't read this conversation: " + err.Error()
	}
	return a.BudgetReport().Brief(sess.Agent)
}
