package agent

import (
	"context"
	"encoding/json"

	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// PolicyFunc sets a turn's tool policy (env.Policy, and env.Taint for
// outside content) from where the turn came from: origin is its reply
// address, the parent's for a delegated task, and empty for a turn started
// on this machine.
type PolicyFunc func(ctx context.Context, spec *Spec, origin json.RawMessage, env *tools.Env)

// applyPolicy runs the Runner's Policy for this turn. A delegated task
// inherits its parent's origin, so a sub-agent gets no more than the chat
// that asked for it, and its parent's taint.
func (t *turn) applyPolicy(env *tools.Env) {
	origin := originFrom(t.ctx)
	if t.sub != nil {
		if len(origin) == 0 {
			origin = t.sub.ParentOrigin
		}
		if t.sub.Tainted {
			env.Taint.Mark()
		}
	}
	if t.r.Policy != nil {
		t.r.Policy(t.ctx, t.spec, origin, env)
	}
}
