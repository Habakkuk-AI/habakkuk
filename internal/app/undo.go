package app

import "context"

// Undo restores the workspace to before the session's latest turn that
// changed it, as /undo does in a chat, and returns the reply.
func (a *App) Undo(ctx context.Context, sessionID string) (string, error) {
	return a.undo(ctx, sessionID)
}
