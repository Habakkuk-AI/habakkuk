package app

import (
	"context"
	"path/filepath"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/core/state"
	"github.com/habakkuk-ai/habakkuk/internal/ops/hooks"
)

// openExtras opens state.db (session overrides, goals, steering, task
// lists, progress drafts), the hook runner, and the tools and chat features
// built on them.
func (a *App) openExtras(ctx context.Context, home string) error {
	st, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		return err
	}
	a.State = st
	a.Hooks = hooks.New(a.Cfg.Hooks, home, a.Log)
	// Background hooks finish (bounded by their timeouts) before the
	// databases close.
	// Automatic titles being written finish before the databases close.
	a.closers = append(a.closers, a.Hooks.Wait, a.Runner.WaitTitles, st.Close)
	personas := state.Personalities{Dir: filepath.Join(home, "personalities")}
	a.Runner.Overlay = state.Overlay{Store: st, Personas: personas}
	a.Runner.Hooks = a.Hooks
	state.RegisterTools(a.Tools, st)
	session.RegisterTools(a.Tools, a.Sessions)
	if a.Gateway != nil {
		a.Gateway.State, a.Gateway.Personas = st, personas
	}
	return nil
}

// pruneExtras applies retention to state.db.
func (a *App) pruneExtras(ctx context.Context, cutoff time.Time) {
	if a.State == nil {
		return
	}
	if n, err := a.State.Prune(ctx, cutoff); err != nil {
		a.Log.Warn("state pruning failed", "err", err)
	} else if n > 0 {
		a.Log.Info("state pruned", "rows", n)
	}
}
