package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// ErrBusy means the session already has a turn queued or running. Its live
// events would mix with the new turn's, so callers that stream refuse to
// queue behind it.
var ErrBusy = errors.New("this session already has a turn in progress; wait for it to finish or use another session")

// ErrCanceled means the turn was stopped before it finished.
var ErrCanceled = errors.New("the turn was canceled")

// Ask runs one turn for a client that waits for the answer (the HTTP API,
// MCP and editor clients) and returns the reply. A Turn whose ID already
// names a job attaches to it instead of queueing another, so a retried
// request gets the first request's answer. on, when set, receives the
// session's live events while the turn runs; they are best effort, as the
// turn may run in another process, so the reply is the result to trust.
// Returning early because ctx ended leaves the turn running: it is durable.
func (r *Runner) Ask(ctx context.Context, t Turn, on func(events.Event)) (string, error) {
	var evs <-chan events.Event
	if r.Bus != nil && on != nil {
		ch, unsub := r.Bus.Subscribe(t.Session) // before queueing, so no event slips by
		defer unsub()
		evs = ch
	}
	id, err := r.Begin(ctx, t)
	if err != nil {
		return "", err
	}
	return r.Await(ctx, id, evs, on)
}

// Begin queues t, or finds the job t.ID names, and returns the job ID. It
// refuses with ErrBusy when another turn of the session is pending or running.
func (r *Runner) Begin(ctx context.Context, t Turn) (string, error) {
	if t.ID != "" {
		if j, err := r.Queue.Get(ctx, t.ID); err == nil && j != nil {
			return j.ID, nil
		}
	}
	busy, err := r.Queue.Busy(ctx, t.Session, "")
	if err != nil {
		return "", err
	}
	if busy {
		return "", ErrBusy
	}
	if t.ID == "" {
		t.ID = jobs.NewID("turn")
	}
	id, err := r.SubmitTurn(ctx, t)
	if err != nil {
		return "", err
	}
	if r.Wake != nil {
		r.Wake()
	}
	return id, nil
}

// Await waits for a turn job to end, passing events from evs to on.
func (r *Runner) Await(ctx context.Context, jobID string, evs <-chan events.Event, on func(events.Event)) (string, error) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	check := true
	for {
		if check {
			j, err := r.Queue.Get(ctx, jobID)
			if err != nil {
				return "", fmt.Errorf("turn %s: %w", jobID, err)
			}
			switch j.Status {
			case jobs.Done:
				drain(evs, on)
				return j.Result, nil
			case jobs.Failed:
				return "", fmt.Errorf("the turn failed: %s", j.LastError)
			case jobs.Canceled:
				return "", ErrCanceled
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case e := <-evs:
			if on != nil {
				on(e)
			}
			// Look at the job on a tick, or as soon as an attempt ends.
			check = e.Kind == events.TurnDone || e.Kind == events.TurnError
		case <-tick.C:
			check = true
		}
	}
}

// drain hands over events already buffered when the job finished, so a
// client sees the last text before the turn's end.
func drain(evs <-chan events.Event, on func(events.Event)) {
	if evs == nil || on == nil {
		return
	}
	for {
		select {
		case e := <-evs:
			on(e)
		default:
			return
		}
	}
}

// NewSessionAt is NewSession for work in another folder (an editor's
// project): the frozen prompt names that folder as the workspace. Tools see
// it through the Runner's Env, which the caller arranges.
func (r *Runner) NewSessionAt(ctx context.Context, agentName, workspace string) (*session.Session, error) {
	spec, ok := r.Agents[agentName]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", agentName)
	}
	var snap Snapshot
	if r.Snapshot != nil {
		snap = r.Snapshot(ctx, spec)
	}
	return r.Sessions.Create(ctx, agentName, BuildSystemPrompt(spec.Name, spec.Dir, workspace, snap))
}

// Seed writes earlier conversation into a new, empty session, so a client
// that kept its own history (an OpenAI-style API caller) can carry it over.
// Messages of the same role in a row are joined, and a history that starts
// with the assistant gets a short user line first, since models expect the
// user to speak first.
func (r *Runner) Seed(ctx context.Context, sessionID string, history []llm.Message) error {
	var msgs []llm.Message
	for _, m := range history {
		text := m.Text()
		if text == "" {
			continue
		}
		if n := len(msgs); n > 0 && msgs[n-1].Role == m.Role {
			msgs[n-1] = llm.Message{Role: m.Role, Blocks: []llm.Block{{Type: llm.Text, Text: msgs[n-1].Text() + "\n\n" + text}}}
			continue
		}
		msgs = append(msgs, llm.Message{Role: m.Role, Blocks: []llm.Block{{Type: llm.Text, Text: text}}})
	}
	if len(msgs) > 0 && msgs[0].Role == llm.Assistant {
		msgs = append([]llm.Message{llm.UserText("[Earlier conversation, carried over by the client.]")}, msgs...)
	}
	// The turn that follows adds a user message; two in a row would be
	// joined by some providers and rejected by others.
	if n := len(msgs); n > 0 && msgs[n-1].Role == llm.User {
		msgs = append(msgs, llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "(noted)"}}})
	}
	for i, m := range msgs {
		if err := r.Sessions.Append(ctx, sessionID, "", i, m, "", nil); err != nil {
			return err
		}
	}
	return nil
}

// TurnUsage adds up the tokens of every model response a turn job wrote.
func (r *Runner) TurnUsage(ctx context.Context, sessionID, jobID string) llm.Usage {
	var u llm.Usage
	recs, err := r.Sessions.Messages(ctx, sessionID)
	if err != nil {
		return u
	}
	for _, rec := range recs {
		if rec.JobID == jobID && rec.Usage != nil {
			u.Add(*rec.Usage)
		}
	}
	return u
}
