package slack

import (
	"context"
	"errors"
	"fmt"
)

// Check proves the tokens work: auth.test for each bot token, and
// apps.connections.open for the app-level token (it hands out a Socket Mode
// URL that is left unused and expires). It returns the workspace names.
func (a *Adapter) Check(ctx context.Context) ([]string, error) {
	if a.AppToken == "" || len(a.BotTokens) == 0 {
		return nil, errors.New("slack: needs an app-level token (xapp-) and a bot token (xoxb-)")
	}
	var teams []string
	for _, tok := range a.BotTokens {
		var r struct {
			Team string `json:"team"`
		}
		if err := a.api(ctx, tok, "auth.test", struct{}{}, &r); err != nil {
			return nil, fmt.Errorf("bot token: %w", err)
		}
		teams = append(teams, r.Team)
	}
	if err := a.api(ctx, a.AppToken, "apps.connections.open", struct{}{}, nil); err != nil {
		return nil, fmt.Errorf("app-level token (it needs the connections:write scope and Socket Mode on): %w", err)
	}
	return teams, nil
}
