package app

import (
	"context"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// untrustedOrProject is untrustedSession, and also sessions that started
// with a project's untrusted instruction files: what the memory review
// learns from them is marked untrusted, so file text cannot become a
// trusted note or reach the user profile.
func (a *App) untrustedOrProject(ctx context.Context, sessionID string) bool {
	if a.untrustedSession(ctx, sessionID) {
		return true
	}
	if a.Sessions == nil {
		return false
	}
	recs, err := a.Sessions.Messages(ctx, sessionID)
	if err != nil {
		return true // cannot tell: err on the side of untrusted
	}
	msgs := make([]llm.Message, 0, 1)
	if len(recs) > 0 {
		msgs = append(msgs, recs[0].Message)
	}
	return agent.ProjectTainted(msgs)
}
