package discord

import (
	"context"
	"net/http"
)

// Check proves the bot token works and returns the bot's username.
func (a *Adapter) Check(ctx context.Context) (string, error) {
	var me struct {
		Username string `json:"username"`
	}
	if err := a.do(ctx, http.MethodGet, "/users/@me", "", nil, &me); err != nil {
		return "", err
	}
	return me.Username, nil
}
