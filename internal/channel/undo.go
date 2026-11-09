package channel

import "context"

// undo answers /undo. It refuses while a turn runs in the conversation, so
// it never restores files under a turn that is still changing them.
func (g *Gateway) undo(ctx context.Context, to Address) string {
	if g.Undo == nil {
		return g.t(to, "undo.unavailable")
	}
	conv, err := g.Store.Conversation(ctx, to)
	if err != nil || conv == nil {
		return g.t(to, "undo.nothing")
	}
	if busy, err := g.Queue.Busy(ctx, conv.SessionID, ""); err == nil && busy {
		return g.t(to, "undo.busy")
	}
	msg, err := g.Undo(ctx, conv.SessionID)
	if err != nil {
		g.Log.Error("undo failed", "session", conv.SessionID, "err", err)
		return g.t(to, "undo.failed", err.Error())
	}
	return msg
}
