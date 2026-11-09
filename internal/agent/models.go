package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// SessionModels stores the model a session was switched to with /model.
type SessionModels interface {
	SessionModel(ctx context.Context, session string) (string, error)
	SetSessionModel(ctx context.Context, session, ref string) error
}

// chainFor is the model chain a session's turns use: a /model choice first,
// then the agent's own chain as fallbacks. overridden reports a /model choice.
func (r *Runner) chainFor(ctx context.Context, session string, spec *Spec) (chain []llm.Target, overridden bool) {
	if r.Choices == nil {
		return spec.Chain, false
	}
	ref, err := r.Choices.SessionModel(ctx, session)
	if err != nil {
		r.Log.Warn("read session model failed", "session", session, "err", err)
		return spec.Chain, false
	}
	p, m, ok := strings.Cut(ref, "/")
	if !ok || p == "" || m == "" {
		return spec.Chain, false
	}
	first := llm.Target{Endpoint: p, Model: m}
	out := []llm.Target{first}
	for _, t := range spec.Chain {
		if t != first {
			out = append(out, t)
		}
	}
	return out, true
}

// Chain returns the model chain a session uses now.
func (r *Runner) Chain(ctx context.Context, session string) ([]llm.Target, error) {
	sess, err := r.Sessions.Get(ctx, session)
	if err != nil {
		return nil, err
	}
	spec, ok := r.Agents[sess.Agent]
	if !ok {
		return nil, fmt.Errorf("agent %q is not configured", sess.Agent)
	}
	chain, _ := r.chainFor(ctx, session, spec)
	return chain, nil
}

// ModelCommand runs /model for a session: with no argument it shows the
// models in use; "default" (or "reset") goes back to the agent's own; a
// "<provider>/<model>" switches this session only. It returns the reply.
func (r *Runner) ModelCommand(ctx context.Context, session, arg string) string {
	arg = strings.TrimSpace(arg)
	if r.Choices == nil {
		return "Switching models is not available here."
	}
	switch arg {
	case "":
		chain, err := r.Chain(ctx, session)
		if err != nil {
			return "Couldn't read this session: " + err.Error()
		}
		cur, _ := r.Choices.SessionModel(ctx, session)
		names := make([]string, len(chain))
		for i, t := range chain {
			names[i] = t.String()
		}
		msg := "Models: " + strings.Join(names, " → ")
		if cur != "" {
			msg += "\n(this session was switched to " + cur + "; /model default goes back)"
		} else if sess, err := r.Sessions.Get(ctx, session); err == nil {
			if spec, ok := r.Agents[sess.Agent]; ok {
				if note := routingNote(spec); note != "" {
					msg += "\n" + note
				}
			}
		}
		return msg + "\nSwitch with /model <provider>/<model>."
	case "default", "reset":
		if err := r.Choices.SetSessionModel(ctx, session, ""); err != nil {
			return "Couldn't reset the model: " + err.Error()
		}
		return "This session uses the agent's own models again."
	}
	if r.CheckModel != nil {
		if err := r.CheckModel(arg); err != nil {
			return "Can't switch: " + err.Error()
		}
	}
	if err := r.Choices.SetSessionModel(ctx, session, arg); err != nil {
		return "Couldn't switch the model: " + err.Error()
	}
	return "This session now uses " + arg + " (the agent's own models stay as fallbacks)."
}
