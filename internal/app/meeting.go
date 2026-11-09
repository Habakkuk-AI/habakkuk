package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools/browser"
	"github.com/habakkuk-ai/habakkuk/internal/tools/meeting"
)

// meetingWorkers is how many meetings one process attends at once; each
// runs a browser of its own.
const meetingWorkers = 3

// openMeetings sets up meeting attendance: the meeting toolset, and the
// manager whose jobs the serving process runs. Each attendance launches its
// own Chrome (browser.path) with the browser section's allow_private and
// blocked_urls, under $HABAKKUK_HOME/meetings.
func (a *App) openMeetings(home string) {
	mc, bc := a.Cfg.MeetingConfig(), a.Cfg.BrowserConfig()
	m := &meeting.Manager{
		Store: meeting.NewStore(filepath.Join(home, "meetings")),
		Cfg:   mc, Queue: a.Queue, Location: a.Runner.Location, Log: a.Log,
		Launch: meeting.ChromeLauncher(bc.Path, *mc.Headless, browser.NewGuard(nil, bc.BlockedURLs, bc.AllowPrivate), a.Log),
		Model:  a.meetingModel(mc),
	}
	if a.Gateway != nil {
		m.Deliver = a.Gateway.Deliver
	}
	m.Notify = func(ctx context.Context, _, session, id, text string) error {
		if _, err := a.Runner.SubmitTurn(ctx, agent.Turn{ID: "turn_" + id, Session: session, Text: text, Background: true}); err != nil {
			return err
		}
		if a.Runner.Wake != nil {
			a.Runner.Wake()
		}
		return nil
	}
	meeting.RegisterTools(a.Tools, m, func(ctx context.Context, session string) json.RawMessage {
		if o := agent.OriginFrom(ctx); len(o) > 0 {
			return o
		}
		return a.here(ctx, session)
	})
	a.Meetings = m
}

// meetingModel writes notes with summary_model, else the agent's own chain.
func (a *App) meetingModel(mc config.Meeting) meeting.Model {
	return func(ctx context.Context, agentName, system, prompt string) (string, error) {
		var chain []llm.Target
		if spec := a.Runner.Agents[agentName]; spec != nil {
			chain = spec.Chain
		}
		if mc.SummaryModel != "" {
			ref, _ := config.ParseModelRef(mc.SummaryModel) // validated at load
			chain = []llm.Target{{Endpoint: ref.Provider, Model: ref.Model}}
		}
		if llm.CallerFrom(ctx).Agent == "" {
			ctx = llm.WithCaller(ctx, llm.Caller{Agent: agentName, Session: "meeting"})
		}
		resp, err := a.Router.Send(ctx, chain, llm.Request{
			System: system, Messages: []llm.Message{llm.UserText(prompt)}, MaxTokens: 4096,
		}, nil)
		if err != nil {
			return "", err
		}
		return resp.Message.Text(), nil
	}
}

// meetingPool runs attendances. Only the serving process runs it: a
// meeting outlives a chat or a one-off run.
func (a *App) meetingPool() *jobs.Pool {
	return &jobs.Pool{
		Queue: a.Queue, Handlers: map[string]jobs.Handler{meeting.Kind: a.Meetings.Handle},
		Concurrency: meetingWorkers, Lease: a.Cfg.Workers.Lease, Log: a.Log,
	}
}

// pruneMeetings applies retention to finished attendances.
func (a *App) pruneMeetings(cutoff time.Time) {
	if a.Meetings == nil {
		return
	}
	if n, err := a.Meetings.Store.Prune(cutoff); err != nil {
		a.Log.Warn("meeting pruning failed", "err", err)
	} else if n > 0 {
		a.Log.Info("meetings pruned", "count", n)
	}
}
