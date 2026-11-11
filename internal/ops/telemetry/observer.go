package telemetry

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// Metrics are Habakkuk's metric families.
type Metrics struct {
	Registry *Registry

	TurnsStarted  *Counter
	TurnsFinished *Counter
	TurnsFailed   *Counter
	TurnDuration  *Histogram

	ModelCalls    *Counter
	ModelDuration *Histogram
	Tokens        *Counter
	Cost          *Counter

	ToolCalls    *Counter
	ToolDuration *Histogram
	LoopGuard    *Counter

	Deliveries        *Counter
	ChannelConnected  *Gauge
	ChannelReconnects *Counter

	QueueDepth *Gauge
	Jobs       *Counter
	JobRetries *Counter

	StartTime *Gauge
	BuildInfo *Gauge
}

// NewMetrics registers Habakkuk's metrics in a new registry.
func NewMetrics() *Metrics {
	r := NewRegistry()
	m := &Metrics{
		Registry:      r,
		TurnsStarted:  r.Counter("habakkuk_turns_started_total", "Turns started (a turn resumed after a crash counts again).", "agent"),
		TurnsFinished: r.Counter("habakkuk_turns_finished_total", "Turns that finished with a reply.", "agent"),
		TurnsFailed:   r.Counter("habakkuk_turns_failed_total", "Turn runs that ended in an error.", "agent"),
		TurnDuration:  r.Histogram("habakkuk_turn_duration_seconds", "Wall time of one turn run.", DurationBuckets, "agent"),

		ModelCalls:    r.Counter("habakkuk_model_calls_total", "Model requests, one per attempt, by outcome (ok or the error class).", "provider", "model", "outcome"),
		ModelDuration: r.Histogram("habakkuk_model_call_duration_seconds", "Wall time of one model request, streaming included.", DurationBuckets, "provider", "model"),
		Tokens:        r.Counter("habakkuk_model_tokens_total", "Tokens by kind: input, output, cache_read, cache_write.", "provider", "model", "kind"),
		Cost:          r.Counter("habakkuk_model_cost_usd_total", "Estimated cost in US dollars at list prices (0 for logins, absent for unknown models).", "provider", "model"),

		ToolCalls:    r.Counter("habakkuk_tool_calls_total", "Tool calls by outcome: ok, error, denied or blocked.", "tool", "outcome"),
		ToolDuration: r.Histogram("habakkuk_tool_call_duration_seconds", "Wall time of one tool call, approval wait included.", DurationBuckets, "tool"),
		LoopGuard:    r.Counter("habakkuk_loop_guard_total", "Times the loop guard warned a model that repeated itself, or stopped its turn, by check (repeat or idle) and action (warn or stop).", "agent", "check", "action"),

		Deliveries:        r.Counter("habakkuk_deliveries_total", "Delivery attempts by channel and outcome: sent, retry or failed.", "channel", "outcome"),
		ChannelConnected:  r.Gauge("habakkuk_channel_connected", "1 while the channel adapter is connected, else 0.", "channel"),
		ChannelReconnects: r.Counter("habakkuk_channel_reconnects_total", "Adapter restarts since serve started.", "channel"),

		QueueDepth: r.Gauge("habakkuk_queue_depth", "Jobs pending or running, by kind.", "kind", "status"),
		Jobs:       r.Counter("habakkuk_jobs_total", "Job runs by kind and outcome: done, retry, failed or lost.", "kind", "outcome"),
		JobRetries: r.Counter("habakkuk_job_retries_total", "Job runs that failed and were queued again.", "kind"),

		StartTime: r.Gauge("habakkuk_start_time_seconds", "Unix time this process started."),
		BuildInfo: r.Gauge("habakkuk_build_info", "Always 1; the version is a label.", "version"),
	}
	m.StartTime.Set(float64(time.Now().Unix()))
	m.BuildInfo.Set(1, "dev")
	return m
}

// Observer turns turn, tool and model events into metrics and spans. Turn
// spans are found by session (one session runs one turn at a time), so a
// model call made anywhere under a turn, with the turn's caller on its
// context, becomes the turn's child.
type Observer struct {
	M       *Metrics
	Tracer  *Tracer // nil: no traces
	Content bool    // put message text and tool input and output on spans
	// Price returns a call's cost in dollars, ok false when unknown.
	Price func(provider, model string, u llm.Usage) (float64, bool)

	mu    sync.Mutex
	turns map[string]*turnState // by session
	tools map[string]*toolState // by session + call id
}

type turnState struct {
	job   string
	start time.Time
	span  *Span
}

type toolState struct {
	start time.Time
	span  *Span
}

// MaxContent caps content attributes.
const MaxContent = 4096

func (o *Observer) init() {
	if o.turns == nil {
		o.turns = map[string]*turnState{}
		o.tools = map[string]*toolState{}
	}
}

// TurnStart records a turn run beginning.
func (o *Observer) TurnStart(agent, session, job, text string) {
	o.M.TurnsStarted.Inc(agent)
	sp := o.Tracer.Start("turn "+agent, nil, KindInternal)
	sp.Set("habakkuk.agent", agent)
	sp.Set("habakkuk.session", session)
	sp.Set("habakkuk.job", job)
	sp.Set("habakkuk.input.chars", len(text))
	if o.Content {
		sp.Set("habakkuk.input", truncate(text, MaxContent))
	}
	o.mu.Lock()
	o.init()
	if old := o.turns[session]; old != nil {
		old.span.End(errors.New("superseded")) // a lost lease never reached turn_end
	}
	o.turns[session] = &turnState{job: job, start: time.Now(), span: sp}
	o.mu.Unlock()
}

// TurnEnd records a turn run ending; errText is empty on success.
func (o *Observer) TurnEnd(agent, session, job, final, errText string) {
	o.mu.Lock()
	o.init()
	st := o.turns[session]
	if st != nil && st.job == job {
		delete(o.turns, session)
	} else {
		st = nil
	}
	o.mu.Unlock()
	var err error
	if errText != "" {
		err = errors.New(errText)
		o.M.TurnsFailed.Inc(agent)
	} else {
		o.M.TurnsFinished.Inc(agent)
	}
	if st == nil {
		return
	}
	o.M.TurnDuration.Observe(time.Since(st.start).Seconds(), agent)
	st.span.Set("habakkuk.output.chars", len(final))
	if o.Content && final != "" {
		st.span.Set("habakkuk.output", truncate(final, MaxContent))
	}
	st.span.End(err)
}

func (o *Observer) turnSpan(session string) *Span {
	o.mu.Lock()
	defer o.mu.Unlock()
	if st := o.turns[session]; st != nil {
		return st.span
	}
	return nil
}

// ToolStart records a tool call beginning.
func (o *Observer) ToolStart(session, tool, callID, input string) {
	sp := o.Tracer.Start("tool "+tool, o.turnSpan(session), KindInternal)
	sp.Set("gen_ai.tool.name", tool)
	sp.Set("gen_ai.tool.call.id", callID)
	sp.Set("habakkuk.tool.input.bytes", len(input))
	if o.Content {
		sp.Set("habakkuk.tool.input", truncate(input, MaxContent))
	}
	o.mu.Lock()
	o.init()
	o.tools[session+"\x00"+callID] = &toolState{start: time.Now(), span: sp}
	o.mu.Unlock()
}

// ToolEnd records a tool call's result. outcome is ok, error, denied or
// blocked.
func (o *Observer) ToolEnd(session, tool, callID, result, outcome string) {
	key := session + "\x00" + callID
	o.mu.Lock()
	o.init()
	st := o.tools[key]
	delete(o.tools, key)
	o.mu.Unlock()
	o.M.ToolCalls.Inc(tool, outcome)
	if st == nil {
		return
	}
	o.M.ToolDuration.Observe(time.Since(st.start).Seconds(), tool)
	st.span.Set("habakkuk.tool.outcome", outcome)
	st.span.Set("habakkuk.tool.result.bytes", len(result))
	if o.Content {
		st.span.Set("habakkuk.tool.result", truncate(result, MaxContent))
	}
	var err error
	if outcome != "ok" {
		err = errors.New(outcome)
	}
	st.span.End(err)
}

// LoopGuard records the loop guard warning a model or stopping a turn.
func (o *Observer) LoopGuard(agent, check, action string) {
	o.M.LoopGuard.Inc(agent, check, action)
}

// ToolOutcome classifies a tool result for the outcome label.
func ToolOutcome(text string, isError bool) string {
	switch {
	case !isError:
		return "ok"
	case strings.HasPrefix(text, "denied by permission policy"), strings.HasPrefix(text, "refused:"):
		return "denied"
	case strings.HasPrefix(text, "blocked by"):
		return "blocked"
	}
	return "error"
}

// Provider wraps a model provider so each request is counted, timed,
// priced and traced. It keeps the ModelLister ability of p, if any.
func (o *Observer) Provider(endpoint string, p llm.Provider) llm.Provider {
	op := &observedProvider{o: o, endpoint: endpoint, inner: p}
	if l, ok := p.(llm.ModelLister); ok {
		return &observedLister{observedProvider: op, lister: l}
	}
	return op
}

type observedProvider struct {
	o        *Observer
	endpoint string
	inner    llm.Provider
}

type observedLister struct {
	*observedProvider
	lister llm.ModelLister
}

func (l *observedLister) ListModels(ctx context.Context, key string) ([]string, error) {
	return l.lister.ListModels(ctx, key)
}

func (p *observedProvider) Stream(ctx context.Context, key string, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error) {
	o := p.o
	caller := llm.CallerFrom(ctx)
	sp := o.Tracer.Start("chat "+req.Model, o.turnSpan(caller.Session), KindClient)
	sp.Set("gen_ai.operation.name", "chat")
	sp.Set("gen_ai.request.model", req.Model)
	sp.Set("habakkuk.provider", p.endpoint)
	if caller.Agent != "" {
		sp.Set("habakkuk.agent", caller.Agent)
	}
	start := time.Now()
	resp, err := p.inner.Stream(ctx, key, req, onEvent)
	o.M.ModelDuration.Observe(time.Since(start).Seconds(), p.endpoint, req.Model)
	if err != nil {
		kind := string(llm.Classify(err).Kind)
		o.M.ModelCalls.Inc(p.endpoint, req.Model, kind)
		sp.Set("error.type", kind)
		sp.End(err)
		return resp, err
	}
	o.M.ModelCalls.Inc(p.endpoint, req.Model, "ok")
	u := resp.Usage
	o.M.Tokens.Add(float64(u.InputTokens), p.endpoint, req.Model, "input")
	o.M.Tokens.Add(float64(u.OutputTokens), p.endpoint, req.Model, "output")
	o.M.Tokens.Add(float64(u.CacheReadTokens), p.endpoint, req.Model, "cache_read")
	o.M.Tokens.Add(float64(u.CacheWriteTokens), p.endpoint, req.Model, "cache_write")
	if o.Price != nil {
		if cost, ok := o.Price(p.endpoint, req.Model, u); ok {
			o.M.Cost.Add(cost, p.endpoint, req.Model)
			sp.Set("habakkuk.cost_usd", cost)
		}
	}
	sp.Set("gen_ai.usage.input_tokens", u.InputTokens)
	sp.Set("gen_ai.usage.output_tokens", u.OutputTokens)
	sp.Set("habakkuk.usage.cache_read_tokens", u.CacheReadTokens)
	sp.Set("gen_ai.response.finish_reasons", string(resp.StopReason))
	sp.End(nil)
	return resp, nil
}
