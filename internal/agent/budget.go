package agent

import (
	"errors"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// Budget is what the runner needs from the spending limits
// (internal/llm/budget implements it). The limits themselves are enforced
// in the router, before every model call; a refused call surfaces here as
// an *llm.BudgetError.
type Budget interface {
	// Blocked returns the error a call by agent would get now from a daily
	// or monthly limit, or nil.
	Blocked(agent string) *llm.BudgetError
	// Notice reports whether the pause notice for a limit and period (a
	// BudgetError's Key) is still to be given, and marks it given.
	Notice(key string) bool
}

// silentReply, as a turn's whole reply, means nothing is sent to its chat
// (the gateway's Silent).
const silentReply = "[SILENT]"

// BudgetBlocked returns the error agent's next model call would get from a
// spending limit, or nil. Background work asks it before starting a run.
func (r *Runner) BudgetBlocked(agent string) *llm.BudgetError {
	if r.Budget == nil {
		return nil
	}
	return r.Budget.Blocked(agent)
}

// BudgetNotice reports whether background work should tell the user about
// a limit that stopped it: once per limit and period.
func (r *Runner) BudgetNotice(be *llm.BudgetError) bool {
	if r.Budget == nil {
		return true
	}
	return r.Budget.Notice(be.Key)
}

// UnavailableNotice reports whether background work should tell the user
// that no model could take a call (credit or keys): once per outage, as
// the router keeps count (llm.Router.UnavailableNotice).
func (r *Runner) UnavailableNotice(ue *llm.UnavailableError) bool {
	if n, ok := r.Models.(interface{ UnavailableNotice(key string) bool }); ok {
		return n.UnavailableNotice(ue.Key)
	}
	return true
}

// pauseOnBudget ends the turn cleanly when a spending limit refused a model
// call: the turn finishes (no error, so the job is not retried) with the
// limit's message as its reply, also written to the transcript so the
// conversation stays well formed. A background turn (a schedule, a board
// notice, a background result) sends the message once per limit and
// period and is silent after that. Other errors pass through.
func (t *turn) pauseOnBudget(err error, p TurnPayload) error {
	var be *llm.BudgetError
	if !errors.As(err, &be) {
		return err
	}
	t.paused = true
	t.r.Log.Info("turn paused by a spending limit", "agent", t.spec.Name, "session", t.sess.ID, "job", t.job.ID, "limit", be.Key)
	if !p.Compact && len(t.msgs) > 0 && t.msgs[len(t.msgs)-1].Role != llm.Assistant {
		if aerr := t.append(llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: be.Message}}}, "", nil); aerr != nil {
			return aerr
		}
	}
	t.final = be.Message
	if p.Background && !t.r.BudgetNotice(be) {
		t.final = silentReply
	}
	return nil
}
