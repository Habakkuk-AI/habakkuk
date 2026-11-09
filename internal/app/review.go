package app

import (
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
	"github.com/habakkuk-ai/habakkuk/internal/tools/review"
)

// newReviewer builds an agent's approval reviewer: reviewer_model when set,
// otherwise the agent's own model chain.
func newReviewer(a *App, name string, ac *config.Agent, chain []llm.Target) tools.Reviewer {
	if ac.ReviewerModel != "" {
		ref, _ := config.ParseModelRef(ac.ReviewerModel) // validated at load
		chain = []llm.Target{{Endpoint: ref.Provider, Model: ref.Model}}
	}
	rv := &review.Reviewer{Agent: name, Models: a.Router, Chain: chain, Bus: a.Bus, Log: a.Log}
	if jc := a.Cfg.Judge; jc != nil {
		rv.Judge, rv.ApproveAbove = a.judgeFor(jc.Approvals), jc.ApproveAbove
	}
	return rv
}
