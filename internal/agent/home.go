package agent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// An agent's home chat is where its owner hears about work that started
// somewhere else: a task another agent delegated, a board card, a schedule,
// an A2A task, or a message in another channel. Spec.Home names it;
// Spec.ReportHome says what is posted there on its own:
const (
	ReportOff     = "off"
	ReportResults = "results" // a short result when such work finishes
	ReportAll     = "all"     // also a line when it starts
)

// Sources of unattended work, for TurnPayload.Source. Delegated tasks are
// told by their job kind (KindSubturn) instead.
const (
	SourceSchedule = "schedule"
	SourceBoard    = "board"
	SourceA2A      = "a2a"
)

// MessageHomeName is the tool that posts to the agent's home chat. It is
// offered only to agents with a home.
const MessageHomeName = "message_home"

// HomeNews is one post for an agent's home chat: work starting (Started)
// or finished (Final). Whether the turn came from the home chat itself
// can only be told by whoever reads Origin (the channel gateway), so it
// gets both addresses and drops news from home.
type HomeNews struct {
	Job     string // the turn's job; post ids derive from it, so each is sent once
	Agent   string
	Session string
	Home    string // CHANNEL:CONVERSATION[:THREAD]
	Started bool

	Kind   string          // the job kind: KindTurn, KindSubturn, a board card...
	Source string          // TurnPayload.Source
	Origin json.RawMessage // the turn's reply address, if any
	// ParentOrigin and ParentAgent are, for a delegated task, the chat and
	// agent of the turn that delegated it.
	ParentOrigin json.RawMessage
	ParentAgent  string

	Task  string // one line naming the work
	Final string // the turn's final reply, once finished
}

// HomeReporter posts HomeNews. The channel gateway implements it.
type HomeReporter interface {
	ReportHome(ctx context.Context, n HomeNews) error
}

// homeNews decides whether a turn is reported to its agent's home chat and
// fills in what is known here. It reports only work nobody is watching
// where it started: a delegated task, a board card, a schedule, an A2A
// task, or a turn with a reply address (a chat; the gateway drops the home
// chat's own). Compactions and turns from a local client (terminal,
// editor, API), whose user sees the answer, are not reported. Nor is a
// task an agent delegated to itself: its parent turn is the one to report,
// so a chain of sub-turns posts once.
func (r *Runner) homeNews(ctx context.Context, j *jobs.Job, p TurnPayload, spec *Spec) (HomeNews, bool) {
	if r.Home == nil || spec.Home == "" || spec.ReportHome == ReportOff || p.Compact {
		return HomeNews{}, false
	}
	n := HomeNews{Job: j.ID, Agent: spec.Name, Session: p.Session, Home: spec.Home, Kind: j.Kind,
		Source: p.Source, Origin: p.Origin, Task: p.Task}
	switch {
	case j.Kind == KindSubturn && p.Sub != nil:
		n.ParentOrigin, n.Task = p.Sub.ParentOrigin, p.Sub.Task
		if parent, err := r.Sessions.Get(ctx, p.Sub.Parent); err == nil {
			n.ParentAgent = parent.Agent
		}
		if n.ParentAgent == spec.Name {
			return HomeNews{}, false
		}
	case p.Source != "" || len(p.Origin) > 0:
	default:
		return HomeNews{}, false
	}
	if n.Task == "" {
		n.Task = taskLine(p.Text)
	}
	n.Task = cutRunes(strings.Join(strings.Fields(n.Task), " "), 120)
	return n, true
}

// reportHome hands news to the reporter. A failure is logged, never the
// turn's: the work itself succeeded and its requester has the result.
func (r *Runner) reportHome(ctx context.Context, n HomeNews) {
	if err := r.Home.ReportHome(ctx, n); err != nil {
		r.Log.Warn("home report failed", "agent", n.Agent, "home", n.Home, "job", n.Job, "err", err)
	}
}

// taskLine is the first line of a turn's text that is not a bracketed
// header such as "[Scheduled task ...]".
func taskLine(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || (strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]")) {
			continue
		}
		return line
	}
	return ""
}

// withoutHomeTool drops message_home for an agent with no home chat.
func withoutHomeTool(specs []llm.ToolSpec) []llm.ToolSpec {
	out := specs[:0:0]
	for _, s := range specs {
		if s.Name != MessageHomeName {
			out = append(out, s)
		}
	}
	return out
}
