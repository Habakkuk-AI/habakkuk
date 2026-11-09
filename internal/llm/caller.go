package llm

import "context"

// Caller says who a model call is for, so usage can be attributed. It rides
// on the context: the turn sets it once and every call made under that turn
// (the agent's own, compaction, the approval reviewer) carries it.
type Caller struct {
	Agent   string
	Session string
	// Turn names the turn the call belongs to, for per-turn budgets: the
	// turn's job ID, and for a sub-turn the ID of the turn that delegated
	// it, so a turn's sub-agents spend from the same allowance.
	Turn string
}

type callerKey struct{}

// WithCaller returns a context whose model calls are attributed to c.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom returns the caller set by WithCaller, or the zero Caller.
func CallerFrom(ctx context.Context) Caller {
	c, _ := ctx.Value(callerKey{}).(Caller)
	return c
}

// ModelLister is implemented by providers whose API can list its models.
type ModelLister interface {
	ListModels(ctx context.Context, apiKey string) ([]string, error)
}
