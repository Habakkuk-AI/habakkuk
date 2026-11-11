package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/usage"
	"github.com/habakkuk-ai/habakkuk/internal/ops/telemetry"
)

// Telemetry is the app's metrics and trace export.
type Telemetry struct {
	Metrics  *telemetry.Metrics
	Observer *telemetry.Observer
	Exporter *telemetry.Exporter // nil without telemetry.otlp_endpoint
}

// SetVersion labels metrics and exported traces with the binary's version.
func (t *Telemetry) SetVersion(v string) {
	if t == nil {
		return
	}
	t.Metrics.BuildInfo.Reset()
	t.Metrics.BuildInfo.Set(1, v)
	t.Exporter.SetVersion(v)
}

// openTelemetry wraps the model providers, the hook runner and (in Pools)
// the job handlers so they record metrics and spans, and serves /metrics
// on the HTTP surface. It runs last in Open, when all of those exist.
func (a *App) openTelemetry() error {
	t := &Telemetry{Metrics: telemetry.NewMetrics()}
	o := &telemetry.Observer{M: t.Metrics,
		Price: func(provider, model string, u llm.Usage) (float64, bool) {
			return a.Pricer.Cost(usage.Call{Provider: provider, Model: model, Usage: u})
		}}
	if tc := a.Cfg.Telemetry; tc != nil && tc.OTLPEndpoint != "" {
		var headers map[string]string
		if tc.HeadersEnv != "" {
			headers = telemetry.ParseHeaders(a.Secret(tc.HeadersEnv))
		}
		exp, err := telemetry.NewExporter(tc.OTLPEndpoint, headers, tc.ServiceName, "dev", a.Log)
		if err != nil {
			return err
		}
		t.Exporter = exp
		o.Tracer, o.Content = telemetry.NewTracer(exp), tc.IncludeContent
		a.closers = append([]func(){func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			exp.Close(ctx)
		}}, a.closers...) // flush before anything else closes
	}
	t.Observer = o
	for name, ep := range a.Router.Endpoints {
		if ep.Provider != nil {
			ep.Provider = o.Provider(name, ep.Provider)
		}
	}
	a.Runner.Hooks = observedHooks{inner: a.Runner.Hooks, o: o}
	a.scrapeGauges(t.Metrics)
	if a.Web != nil {
		a.Web.Metrics, a.Web.MetricsPublic = t.Metrics.Registry.Handler(), a.Cfg.Metrics.Public
	}
	a.Telemetry = t
	return nil
}

// scrapeGauges reads queue depth and channel state from storage on each
// scrape.
func (a *App) scrapeGauges(m *telemetry.Metrics) {
	m.Registry.OnScrape(func(ctx context.Context) {
		if depth, err := a.Queue.DepthByKind(ctx); err == nil {
			m.QueueDepth.Reset()
			for _, d := range depth {
				m.QueueDepth.Set(float64(d.Pending), d.Kind, "pending")
				m.QueueDepth.Set(float64(d.Running), d.Kind, "running")
			}
		}
		if a.Gateway == nil {
			return
		}
		sts, err := a.Gateway.Store.Statuses(ctx)
		if err != nil {
			return
		}
		m.ChannelConnected.Reset()
		for _, st := range sts {
			if _, ok := a.Cfg.Channels[st.Account]; !ok {
				continue
			}
			up := 0.0
			if st.State == "connected" {
				up = 1
			}
			m.ChannelConnected.Set(up, st.Account)
			m.ChannelReconnects.Set(float64(st.Restarts), st.Account)
		}
	})
}

// observePools wraps each pool's handlers to count job outcomes, retries
// and deliveries.
func (a *App) observePools(pools []*jobs.Pool) {
	if a.Telemetry == nil {
		return
	}
	for _, p := range pools {
		for kind, h := range p.Handlers {
			p.Handlers[kind] = observeJob(a.Telemetry.Metrics, kind, h)
		}
	}
}

func observeJob(m *telemetry.Metrics, kind string, h jobs.Handler) jobs.Handler {
	return func(ctx context.Context, j *jobs.Job, owner string) (string, error) {
		res, err := h(ctx, j, owner)
		outcome := jobOutcome(ctx, j, err)
		m.Jobs.Inc(kind, outcome)
		if outcome == "retry" {
			m.JobRetries.Inc(kind)
		}
		if kind == channel.KindDeliver && outcome != "lost" {
			var p struct {
				To struct {
					Account string `json:"account"`
				} `json:"to"`
			}
			_ = json.Unmarshal(j.Payload, &p)
			if outcome == "done" {
				outcome = "sent"
			}
			m.Deliveries.Inc(p.To.Account, outcome)
		}
		return res, err
	}
}

// jobOutcome mirrors what the pool does with a handler's result.
func jobOutcome(ctx context.Context, j *jobs.Job, err error) string {
	var perm jobs.Permanent
	switch {
	case err == nil:
		return "done"
	case ctx.Err() != nil, errors.Is(err, jobs.ErrLeaseLost):
		return "lost" // shutdown or a lost lease: another run picks it up
	case errors.As(err, &perm), j.Attempts >= j.MaxAttempts:
		return "failed"
	}
	return "retry"
}

// observedHooks sees the runner's turn and tool events on their way to the
// configured hooks.
type observedHooks struct {
	inner agent.Hooks
	o     *telemetry.Observer
}

func (h observedHooks) Fire(ctx context.Context, event, subject string, payload any) string {
	switch p := payload.(type) {
	case *agent.TurnHookPayload:
		return h.Fire(ctx, event, subject, *p)
	case *agent.ToolHookPayload:
		return h.Fire(ctx, event, subject, *p)
	case agent.TurnHookPayload:
		switch event {
		case agent.HookTurnStart:
			h.o.TurnStart(p.Agent, p.Session, p.Job, p.Text)
		case agent.HookTurnEnd:
			h.o.TurnEnd(p.Agent, p.Session, p.Job, p.Final, p.Error)
		}
	case agent.LoopGuardPayload:
		h.o.LoopGuard(p.Agent, p.Check, p.Action)
	case agent.ToolHookPayload:
		switch event {
		case agent.HookToolBefore:
			h.o.ToolStart(p.Session, p.Tool, p.CallID, inputText(p.Input))
			veto := h.fire(ctx, event, subject, payload)
			if veto != "" {
				h.o.ToolEnd(p.Session, p.Tool, p.CallID, veto, "blocked")
			}
			return veto
		case agent.HookToolAfter:
			h.o.ToolEnd(p.Session, p.Tool, p.CallID, p.Result, telemetry.ToolOutcome(p.Result, p.IsError))
		}
	}
	return h.fire(ctx, event, subject, payload)
}

func (h observedHooks) fire(ctx context.Context, event, subject string, payload any) string {
	if h.inner == nil {
		return ""
	}
	return h.inner.Fire(ctx, event, subject, payload)
}

func inputText(v any) string {
	switch x := v.(type) {
	case json.RawMessage:
		return string(x)
	case string:
		return x
	}
	b, _ := json.Marshal(v)
	return string(b)
}
