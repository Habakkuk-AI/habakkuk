package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// Subturn is a delegated task still queued or running.
type Subturn struct {
	ID         string
	Agent      string
	Task       string
	Status     jobs.Status
	Started    time.Time
	Background bool
	Model      string
	Worktree   string // its branch, when it works in a worktree
}

// Subturns lists the delegated tasks of a session that are still queued or
// running, oldest first.
func (r *Runner) Subturns(ctx context.Context, parent string) ([]Subturn, error) {
	js, err := r.Queue.Active(ctx, KindSubturn)
	if err != nil {
		return nil, err
	}
	var out []Subturn
	for _, j := range js {
		var p TurnPayload
		if j.Decode(&p) != nil || p.Sub == nil || p.Sub.Parent != parent {
			continue
		}
		s := Subturn{ID: j.ID, Agent: j.Agent, Task: p.Sub.Task, Status: j.Status, Started: j.CreatedAt,
			Background: p.Sub.Background, Model: p.Sub.Model}
		if p.Sub.Worktree != nil {
			s.Worktree = p.Sub.Worktree.Branch
		}
		out = append(out, s)
	}
	return out, nil
}

// StopSubturn cancels one of a session's delegated tasks. id may be the
// job id or a unique prefix of its hex part. A background task's parent
// gets a turn saying it was stopped; a waiting delegate call sees it end.
func (r *Runner) StopSubturn(ctx context.Context, parent, id string) (Subturn, error) {
	subs, err := r.Subturns(ctx, parent)
	if err != nil {
		return Subturn{}, err
	}
	id = strings.TrimSpace(id)
	var hit []Subturn
	for _, s := range subs {
		if s.ID == id || (id != "" && strings.HasPrefix(strings.TrimPrefix(s.ID, "sub_"), strings.TrimPrefix(id, "sub_"))) {
			hit = append(hit, s)
		}
	}
	switch len(hit) {
	case 0:
		return Subturn{}, fmt.Errorf("no running sub-agent task %q in this conversation", id)
	case 1:
	default:
		return Subturn{}, fmt.Errorf("%q matches %d tasks; give more of the id", id, len(hit))
	}
	s := hit[0]
	if err := r.Queue.Cancel(ctx, s.ID); err != nil {
		return Subturn{}, err
	}
	if s.Background {
		j, err := r.Queue.Get(ctx, s.ID)
		if err == nil {
			var p TurnPayload
			if j.Decode(&p) == nil && p.Sub != nil {
				err = r.notifyParent(ctx, s.ID, s.Agent, p.Sub, "", nil)
			}
		}
		if err != nil {
			r.Log.Warn("stopped background task not reported", "job", s.ID, "err", err)
		}
	}
	return s, nil
}

// FormatSubturns renders a list for /subagents.
func FormatSubturns(subs []Subturn, now time.Time) string {
	if len(subs) == 0 {
		return "No sub-agent tasks are running in this conversation."
	}
	var b strings.Builder
	b.WriteString("Sub-agent tasks:")
	for _, s := range subs {
		state := "running"
		if s.Status == jobs.Pending {
			state = "queued"
		}
		fmt.Fprintf(&b, "\n- %s (%s, %s %s", s.ID, s.Agent, state, now.Sub(s.Started).Round(time.Second))
		if s.Background {
			b.WriteString(", background")
		}
		if s.Model != "" {
			b.WriteString(", " + s.Model)
		}
		if s.Worktree != "" {
			b.WriteString(", branch " + s.Worktree)
		}
		fmt.Fprintf(&b, "): %s", cutRunes(strings.Join(strings.Fields(s.Task), " "), 120))
	}
	b.WriteString("\n\nStop one with /subagents stop ID.")
	return b.String()
}

// Fork starts a new session from a copy of sessionID's conversation: all of
// it (rewind 0), or all but its last rewind user turns. The copy ends at
// the last finished turn, so a turn in progress is left out. The original
// is not changed.
func (r *Runner) Fork(ctx context.Context, sessionID string, rewind int) (*session.Session, error) {
	sess, err := r.Sessions.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if _, ok := r.Agents[sess.Agent]; !ok {
		return nil, fmt.Errorf("agent %q is not configured", sess.Agent)
	}
	recs, err := r.Sessions.Messages(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	upto, err := forkPoint(recs, rewind)
	if err != nil {
		return nil, err
	}
	title := sess.Title
	if title == "" {
		title = sessionID
	}
	if rewind > 0 {
		title = fmt.Sprintf("%s (rewound %d)", title, rewind)
	} else {
		title += " (fork)"
	}
	fork, err := r.Sessions.Fork(ctx, sessionID, upto, cutRunes(title, 120))
	if err == nil {
		r.Fire(ctx, HookSessionNew, sess.Agent, map[string]string{"event": HookSessionNew, "agent": sess.Agent, "session": fork.ID})
	}
	return fork, err
}

// forkPoint is how many messages a fork keeps: everything up to the end of
// the last finished turn, after dropping the last rewind user turns.
func forkPoint(recs []session.Record, rewind int) (int, error) {
	if rewind < 0 {
		return 0, errors.New("rewind takes a positive number of turns")
	}
	end := len(recs)
	if rewind > 0 {
		var starts []int
		for i, rec := range recs {
			if startsTurn(rec.Message) {
				starts = append(starts, i)
			}
		}
		if rewind > len(starts) {
			return 0, fmt.Errorf("this conversation has only %d turn(s)", len(starts))
		}
		end = starts[len(starts)-rewind]
	}
	// Back up to the end of a finished turn: an assistant message with no
	// tool calls, so the fork never starts inside a turn.
	for end > 0 {
		m := recs[end-1].Message
		if m.Role == llm.Assistant && len(m.ToolUses()) == 0 {
			break
		}
		end--
	}
	return end, nil
}
