package app

import (
	"context"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/judge"
)

// openJudge builds the decision model client from config (nil without a
// judge: section). Its calls are recorded and capped like model calls.
func (a *App) openJudge() {
	jc := a.Cfg.Judge
	if jc == nil {
		return
	}
	a.Judge = &judge.Client{
		BaseURL: jc.BaseURL, Model: jc.Model, Timeout: time.Duration(jc.TimeoutMS) * time.Millisecond,
		Key: func() string { return a.Secret(jc.KeyEnv) },
		OnUsage: func(ctx context.Context, t llm.Target, u llm.Usage) {
			if a.Router != nil && a.Router.OnUsage != nil {
				a.Router.OnUsage(ctx, t, u)
			}
		},
	}
}

func (a *App) judgeForRouting() *judge.Client {
	if a.Cfg.Judge == nil {
		return nil
	}
	return a.judgeFor(a.Cfg.Judge.Routing)
}

// judgeFor returns the judge when it is on for a use, else nil.
func (a *App) judgeFor(on *bool) *judge.Client {
	if a.Judge == nil || on == nil || !*on {
		return nil
	}
	return a.Judge
}
