// Package agent runs conversation turns as durable jobs.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/judge"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

const KindTurn = "turn"

// Spec is the runtime description of one agent.
type Spec struct {
	Name          string
	Chain         []llm.Target
	MaxTokens     int
	Effort        string
	MaxIterations int
	Toolsets      []string
	ServerTools   []string // provider-hosted tools, e.g. web_search
	Mode          tools.Mode
	Approvals     tools.Approvals // who decides calls needing approval
	Reviewer      tools.Reviewer  // the agent's approval reviewer, if any
	Workspace     string
	Dir           string
	// CompactAt is the prompt size in tokens past which the session is
	// compacted before the next model call; 0 turns it off.
	CompactAt int
	// ToolSearchOver: with more tool specs than this, the model is offered
	// only the core tools plus tool_search and tool_describe, and the tools
	// it finds join on the next call; 0 turns it off.
	ToolSearchOver int
	// AutoTitle gives a session a short title from a model call after its
	// first turn, on TitleChain (default Chain).
	AutoTitle  bool
	TitleChain []llm.Target
	// FastChain is the agent's fast model followed by Chain (route.go);
	// nil without one. Routing is auto, fast or smart; EscalateAfter is the
	// tool rounds a fast turn may take before moving to Chain.
	FastChain     []llm.Target
	Routing       string
	EscalateAfter int
	// Home is the agent's home chat, CHANNEL:CONVERSATION[:THREAD], where
	// it tells its owner about work that started elsewhere; "" is none.
	// ReportHome is off, results or all (home.go).
	Home       string
	ReportHome string
}

type TurnPayload struct {
	Session string          `json:"session"`
	Text    string          `json:"text"`
	Media   []Attachment    `json:"media,omitempty"`
	Origin  json.RawMessage `json:"origin,omitempty"` // opaque to the runner; channels put the reply address here

	// Compact makes the turn only compact the session; Text is unused.
	Compact bool `json:"compact,omitempty"`
	// Background marks a turn nobody is waiting on (a schedule, a board or
	// background-task notice): a spending limit stops it with one notice
	// per period instead of one per run.
	Background bool `json:"background,omitempty"`
	// Sub holds a delegated task's options (KindSubturn).
	Sub *SubturnSpec `json:"sub,omitempty"`
	// Source says what started work nobody watches live (SourceSchedule,
	// SourceBoard, SourceA2A); empty for a chat message or a local client.
	// Task names that work in one line for home reports.
	Source string `json:"source,omitempty"`
	Task   string `json:"task,omitempty"`
}

// Attachment is a file that came with a user message. The bytes live in the
// blob store; Path is a copy in the agent's workspace that tools can open.
type Attachment struct {
	Ref       string `json:"ref"`
	MediaType string `json:"media_type"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Path      string `json:"path,omitempty"`
	Note      string `json:"note,omitempty"` // e.g. a voice note's transcript
}

// Limits for sending media straight to the model; larger files are only
// saved to the workspace.
const (
	maxImageBytes = 5 << 20
	maxPDFBytes   = 32 << 20
)

var modelImageTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}

// userMessage builds the user turn: the stamped text, a line per attachment
// saying where it was saved, and image or PDF blocks the model can read.
func userMessage(text string, media []Attachment) llm.Message {
	var notes strings.Builder
	var blocks []llm.Block
	for _, a := range media {
		fmt.Fprintf(&notes, "\n[Attached %s (%s, %d KB)", a.Name, a.MediaType, (a.Size+1023)/1024)
		if a.Path != "" {
			fmt.Fprintf(&notes, ", saved at %s", a.Path)
		}
		notes.WriteString("]")
		if a.Note != "" {
			notes.WriteString("\n" + a.Note)
		}
		switch {
		case modelImageTypes[a.MediaType] && a.Size <= maxImageBytes:
			blocks = append(blocks, llm.Block{Type: llm.Image, Media: a.Ref, MediaType: a.MediaType, Name: a.Name})
		case a.MediaType == "application/pdf" && a.Size <= maxPDFBytes:
			blocks = append(blocks, llm.Block{Type: llm.Document, Media: a.Ref, MediaType: a.MediaType, Name: a.Name})
		}
	}
	return llm.Message{Role: llm.User, Blocks: append([]llm.Block{{Type: llm.Text, Text: text + notes.String()}}, blocks...)}
}

// Turn is a request to run one user message.
type Turn struct {
	ID      string // optional; enqueueing the same ID twice runs the turn once
	Kind    string // KindTurn (default) or KindSubturn
	Session string
	Text    string
	Media   []Attachment
	Origin  json.RawMessage
	Compact bool // summarize the session's early messages instead of running a message
	Sub     *SubturnSpec
	Source  string // what started unattended work (TurnPayload.Source)
	Task    string // a one-line name for it
	// Background: nobody is waiting on this turn (see TurnPayload).
	Background bool
}

// Sender is the model call. *llm.Router implements it; tests use fakes.
type Sender interface {
	Send(ctx context.Context, chain []llm.Target, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error)
}

type Runner struct {
	Agents   map[string]*Spec
	Sessions *session.Store
	Queue    *jobs.Queue
	Models   Sender
	Tools    *tools.Registry
	Bus      *events.Bus
	BlobDir  string
	Media    func(ref string) ([]byte, error) // loads attachment bytes for the model
	Wake     func()                           // nudges idle workers after enqueueing
	Env      func(spec *Spec, sessionID string) *tools.Env
	Log      *slog.Logger
	Now      func() time.Time
	Location *time.Location // the user's timezone for message time stamps; default the server's
	// Approvers supplies an approver for sessions with no attached client,
	// such as a chat channel that can ask its user.
	Approvers func(sessionID string) tools.Approver
	// Snapshot lists an agent's memory and skills for a new session's
	// prompt. Nil means neither.
	Snapshot func(ctx context.Context, spec *Spec) Snapshot
	// Overlay supplies per-session settings that chat commands and tools
	// change (effort, persona, task list, steering); nil means none.
	Overlay Overlay
	// Hooks runs user commands on turn, tool and session events; nil means
	// none.
	Hooks Hooks
	// Policy sets each turn's tool policy from its origin (policy.go); nil
	// leaves every turn the local policy.
	Policy PolicyFunc

	approvers sync.Map // session id -> tools.Approver
	textOnly  sync.Map // session id -> true once the model rejected its media
	routes    sync.Map // session id -> time of its last turn on the main model (route.go)

	// Judge, when set, decides routing for agents with a fast model; the
	// word rules decide when it is unset or fails (route.go).
	Judge *judge.Client

	titles sync.WaitGroup // automatic titles being written

	// Home posts to an agent's home chat about work that started
	// elsewhere (home.go); nil turns home reports off.
	Home HomeReporter

	// Choices keeps per-session model overrides (/model); nil means none.
	Choices SessionModels
	// CheckModel validates a "<provider>/<model>" reference for /model.
	CheckModel func(ref string) error
	// Budget is the spending limits (budget.go); nil means none.
	Budget Budget
	// ToolsPending waits briefly for tool sources a turn's toolsets need
	// that are still starting (MCP servers connecting in the background)
	// and names those still not ready; nil means none ever are.
	ToolsPending func(ctx context.Context, toolsets []string) []string
}

// Attach registers an approver for a session while a client is present.
func (r *Runner) Attach(sessionID string, a tools.Approver) func() {
	r.approvers.Store(sessionID, a)
	return func() { r.approvers.Delete(sessionID) }
}

// NewSession creates a session with the agent's frozen system prompt.
func (r *Runner) NewSession(ctx context.Context, agent string) (*session.Session, error) {
	return r.NewSessionWith(ctx, agent, SessionOptions{})
}

// NewSessionWith is NewSession for a chat with the user: the first one an
// agent has (or any, with opts.Onboard) starts with the welcome.
func (r *Runner) NewSessionWith(ctx context.Context, agent string, opts SessionOptions) (*session.Session, error) {
	spec, ok := r.Agents[agent]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", agent)
	}
	var snap Snapshot
	if r.Snapshot != nil {
		snap = r.Snapshot(ctx, spec)
	}
	switch {
	case opts.Onboard:
		snap.Onboarding = true
		_ = claimOnboarding(spec.Dir)
	case opts.Chat && NeedsOnboarding(spec.Dir):
		snap.Onboarding = claimOnboarding(spec.Dir)
	}
	sess, err := r.Sessions.Create(ctx, agent, BuildSystemPrompt(spec.Name, spec.Dir, spec.Workspace, snap))
	if err == nil {
		r.Fire(ctx, HookSessionNew, agent, map[string]string{"event": HookSessionNew, "agent": agent, "session": sess.ID})
	}
	return sess, err
}

// Submit queues a user message as a turn. The message is written by the turn
// itself so that it can never interleave with a turn already in progress.
func (r *Runner) Submit(ctx context.Context, sessionID, text string) (string, error) {
	return r.SubmitTurn(ctx, Turn{Session: sessionID, Text: text})
}

func (r *Runner) SubmitTurn(ctx context.Context, t Turn) (string, error) {
	sess, err := r.Sessions.Get(ctx, t.Session)
	if err != nil {
		return "", err
	}
	kind := t.Kind
	if kind == "" {
		kind = KindTurn
	}
	return r.Queue.Enqueue(ctx, jobs.NewJob{
		ID: t.ID, Kind: kind, Agent: sess.Agent, SerialKey: t.Session, Priority: 10,
		Payload: TurnPayload{Session: t.Session, Text: t.Text, Media: t.Media, Origin: t.Origin, Compact: t.Compact, Sub: t.Sub,
			Source: t.Source, Task: t.Task, Background: t.Background},
	})
}

// HandleTurn is the jobs.Handler for KindTurn. It is safe to run again after
// a crash at any point: it reloads the transcript and continues from there.
func (r *Runner) HandleTurn(ctx context.Context, j *jobs.Job, owner string) (string, error) {
	var p TurnPayload
	if err := j.Decode(&p); err != nil {
		return "", jobs.Permanent{Err: err}
	}
	sess, err := r.Sessions.Get(ctx, p.Session)
	if err != nil {
		return "", jobs.Permanent{Err: err}
	}
	spec, ok := r.Agents[sess.Agent]
	if !ok {
		return "", jobs.Permanent{Err: fmt.Errorf("agent %q is not configured", sess.Agent)}
	}
	// Every model call under this turn is attributed to the agent and
	// session, and spends from the turn's allowance: a sub-turn's is the
	// delegating turn's.
	caller := llm.Caller{Agent: sess.Agent, Session: sess.ID, Turn: j.ID}
	if p.Sub != nil && p.Sub.Turn != "" {
		caller.Turn = p.Sub.Turn
	}
	ctx = llm.WithCaller(ctx, caller)
	ctx = withOrigin(ctx, p.Origin)
	chain, overridden := r.chainFor(ctx, sess.ID, spec)
	t := &turn{r: r, ctx: ctx, job: j, owner: owner, sess: sess, spec: spec, chain: chain, overridden: overridden}
	t.useSub(p.Sub)
	hook := TurnHookPayload{Event: HookTurnStart, Agent: spec.Name, Session: sess.ID, Job: j.ID, Text: p.Text}
	r.Fire(ctx, HookTurnStart, spec.Name, hook)
	news, home := r.homeNews(ctx, j, p, spec)
	if home && spec.ReportHome == ReportAll {
		news.Started = true
		r.reportHome(ctx, news)
		news.Started = false
	}
	if p.Compact {
		err = t.runCompact()
	} else {
		err = t.run(p.Text, p.Media)
	}
	err = t.pauseOnBudget(err, p)
	if j.Kind == KindSubturn {
		err = t.pauseOnBudget(t.finishSubturn(p, err), p)
	}
	if err == nil && j.Kind == KindTurn && !p.Compact && !t.paused {
		r.autoTitle(ctx, sess.ID, spec, p.Text, t.final)
	}
	if err == nil && home {
		news.Final = t.final
		r.reportHome(ctx, news)
	}
	hook.Event, hook.Text, hook.Final = HookTurnEnd, "", t.final
	if err != nil {
		hook.Error = err.Error()
	}
	r.Fire(ctx, HookTurnEnd, spec.Name, hook)
	if err != nil {
		r.publish(sess.ID, events.TurnError, err.Error(), true)
		if errors.Is(err, jobs.ErrLeaseLost) {
			return "", err
		}
		if a := llm.Classify(err); a.Kind == llm.ErrBadRequest {
			return "", jobs.Permanent{Err: err}
		}
		return "", err
	}
	r.publish(sess.ID, events.TurnDone, t.final, false)
	return t.final, nil
}

type turn struct {
	r     *Runner
	ctx   context.Context
	job   *jobs.Job
	owner string
	sess  *session.Session
	spec  *Spec
	chain []llm.Target // the agent's chain, after any /model override
	// Routing (route.go): overridden is a /model choice; onFast means the
	// turn runs on FastChain and mainChain is where it escalates to.
	overridden bool
	onFast     bool
	mainChain  []llm.Target
	sub        *SubturnSpec // a delegated task's options, or nil
	msgs       []llm.Message
	final      string

	// Compaction state (compact.go). msgs is always the whole transcript;
	// the model sees summary plus msgs[base:].
	base         int
	summary      string
	compactedAt  int       // transcript length when the latest compaction was written
	usage        llm.Usage // of the latest model response
	usageAt      int       // its seq, or -1
	compactTried bool

	paused bool // a spending limit ended the turn (budget.go)
}

func (t *turn) run(userText string, media []Attachment) error {
	recs, err := t.r.Sessions.Messages(t.ctx, t.sess.ID)
	if err != nil {
		return err
	}
	started := false
	for _, rec := range recs {
		t.msgs = append(t.msgs, rec.Message)
		if rec.JobID == t.job.ID {
			started = true
		}
	}
	// A crashed earlier turn may have left a dangling tool call.
	for _, fix := range session.Repair(t.msgs) {
		if err := t.append(fix, "", nil); err != nil {
			return err
		}
	}
	if err := t.loadCompaction(recs); err != nil {
		return err
	}
	if !started {
		if t.r.Sessions.SetTitle(t.ctx, t.sess.ID, title(userText)) != nil {
			t.r.Log.Warn("set title failed", "session", t.sess.ID)
		}
		if err := t.append(userMessage(t.stamp(userText+t.r.preamble(t.ctx, t.sess.ID)), media), "", nil); err != nil {
			return err
		}
	}

	env := t.r.Env(t.spec, t.sess.ID)
	env.Session, env.Agent = t.sess.ID, t.spec.Name
	t.equip(env)
	env.Approvals, env.Reviewer, env.Task = t.spec.Approvals, t.spec.Reviewer, userText
	if a, ok := t.r.approvers.Load(t.sess.ID); ok {
		env.Approver = a.(tools.Approver)
	} else if t.r.Approvers != nil {
		env.Approver = t.r.Approvers(t.sess.ID)
	}
	t.applyPolicy(env)
	var pending []string
	if t.r.ToolsPending != nil {
		pending = t.r.ToolsPending(t.ctx, t.toolsets())
	}
	specs := t.toolSpecs()

	system, effort := t.r.system(t.ctx, t.sess.SystemPrompt, t.sess.ID), t.r.effort(t.ctx, t.spec, t.sess.ID)
	if len(pending) > 0 {
		system += "\n\n" + pendingNote(pending)
	}
	t.pickRoute(userText, media)
	paused, rounds := false, 0
	for i := 0; i < t.spec.MaxIterations; i++ {
		last := t.msgs[len(t.msgs)-1]
		if last.Role == llm.Assistant && len(last.ToolUses()) == 0 && !paused {
			t.final = last.Text()
			return nil
		}
		media := t.r.Media
		if _, off := t.r.textOnly.Load(t.sess.ID); off {
			media = nil // attachments become placeholders
		}
		if t.shouldCompact() {
			if _, err := t.autoCompact(); err != nil {
				return err
			}
		}
		resp, err := t.r.Models.Send(t.ctx, t.chain, llm.Request{
			System: system, Messages: t.context(), Tools: specs,
			MaxTokens: t.spec.MaxTokens, Effort: effort, Media: media, ServerTools: t.spec.ServerTools,
		}, t.onModelEvent)
		if err != nil {
			// One malformed attachment must not wedge the conversation: it is
			// replayed on every turn. Retry this session without media.
			if llm.Classify(err).Kind == llm.ErrBadRequest && media != nil && hasMedia(t.msgs) {
				t.r.Log.Warn("model rejected a request with media; continuing this session text-only", "session", t.sess.ID, "err", err)
				t.r.textOnly.Store(t.sess.ID, true)
				i--
				continue
			}
			// The window filled up before the threshold caught it (or the
			// threshold is off): compact once and try again.
			if llm.Classify(err).Kind == llm.ErrContextOverflow && !t.compactTried {
				compacted, cerr := t.autoCompact()
				if cerr != nil {
					return cerr
				}
				if compacted {
					i--
					continue
				}
			}
			return err
		}
		if len(resp.Message.Blocks) == 0 {
			resp.Message.Blocks = []llm.Block{{Type: llm.Text, Text: "(no response)"}}
		}
		if err := t.append(resp.Message, resp.Model, &resp.Usage); err != nil {
			return err
		}
		markHostedContent(env, resp.Message)
		if resp.StopReason == llm.StopRefusal {
			t.final = resp.Message.Text()
			return nil
		}
		// A server-side tool loop (web search) hit its step limit: send the
		// conversation back unchanged and the provider resumes.
		paused = resp.StopReason == llm.StopPause && len(resp.Message.ToolUses()) == 0
		if paused {
			continue
		}
		calls := resp.Message.ToolUses()
		if len(calls) == 0 {
			t.final = resp.Message.Text()
			return nil
		}
		results := t.runTools(env, calls, resp.StopReason == llm.StopMaxTokens)
		results = append(results, t.r.steer(t.ctx, t.sess.ID)...) // after the results, as the APIs require
		// Tools that tool_search found join the next call.
		specs = t.moreTools(specs, calls, results)
		if err := t.append(llm.Message{Role: llm.User, Blocks: results}, "", nil); err != nil {
			return err
		}
		rounds++
		t.escalate(rounds)
	}
	return jobs.Permanent{Err: fmt.Errorf("turn stopped after %d iterations without finishing", t.spec.MaxIterations)}
}

func hasMedia(msgs []llm.Message) bool {
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == llm.Image || b.Type == llm.Document {
				return true
			}
		}
	}
	return false
}

// runTools executes all calls from one assistant message concurrently and
// returns their results in call order, in a single message. Media the tools
// attached (view_image, image_generate, browser screenshots) follows the
// results, since providers want tool results first.
func (t *turn) runTools(env *tools.Env, calls []llm.Block, truncated bool) []llm.Block {
	results := make([]llm.Block, len(calls))
	media := make([][]llm.Block, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		if truncated {
			results[i] = llm.Block{Type: llm.ToolResult, ToolUseID: c.ToolUseID, IsError: true,
				Text: "the response hit max_tokens before this call was complete; send it again"}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			t.r.publishEvent(events.Event{Session: t.sess.ID, Kind: events.ToolStart, Text: c.ToolName + " " + preview(string(c.Input)),
				ToolID: c.ToolUseID, Tool: c.ToolName, Input: c.Input})
			res, extra := t.runTool(env, c)
			t.r.publishEvent(events.Event{Session: t.sess.ID, Kind: events.ToolDone, Text: c.ToolName + ": " + preview(res.Text),
				IsError: res.IsError, ToolID: c.ToolUseID, Tool: c.ToolName})
			results[i], media[i] = res, extra
		}()
	}
	wg.Wait()
	for _, m := range media {
		results = append(results, m...)
	}
	return results
}

// append persists one message, but only while this worker still owns the
// job, so a worker that lost its lease can never write into the transcript.
func (t *turn) append(m llm.Message, model string, usage *llm.Usage) error {
	if err := t.r.Queue.Owns(t.ctx, t.job.ID, t.owner); err != nil {
		return err
	}
	if err := t.r.Sessions.Append(t.ctx, t.sess.ID, t.job.ID, len(t.msgs), m, model, usage); err != nil {
		return err
	}
	if usage != nil {
		t.usage, t.usageAt = *usage, len(t.msgs)
	}
	t.msgs = append(t.msgs, m)
	return nil
}

func (t *turn) onModelEvent(e llm.Event) {
	switch e.Type {
	case llm.EventText:
		t.r.publish(t.sess.ID, events.Text, e.Text, false)
	case llm.EventThinking:
		t.r.publish(t.sess.ID, events.Thinking, e.Text, false)
	case llm.EventRetry:
		t.r.publish(t.sess.ID, events.Retry, e.Text, false)
	}
}

// stamp puts the time in the user turn rather than the system prompt, so the
// cached prefix stays byte-identical across turns.
func (t *turn) stamp(text string) string {
	now := time.Now
	if t.r.Now != nil {
		now = t.r.Now
	}
	ts := now()
	if t.r.Location != nil {
		ts = ts.In(t.r.Location)
	}
	return fmt.Sprintf("[%s]\n%s", ts.Format("Mon 2006-01-02 15:04 MST"), text)
}

func (r *Runner) publish(session string, kind events.Kind, text string, isErr bool) {
	if r.Bus != nil {
		r.Bus.Publish(events.Event{Session: session, Kind: kind, Text: text, IsError: isErr})
	}
}

func (r *Runner) publishEvent(e events.Event) {
	if r.Bus != nil {
		r.Bus.Publish(e)
	}
}

func title(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 80 {
		s = s[:80] + "..."
	}
	return s
}

func preview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}
