package slack

import (
	"context"
	"errors"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

// Post, Edit and Delete implement channel.Editor for progress drafts.

func (a *Adapter) Post(ctx context.Context, to channel.Address, text string) (string, error) {
	t, ch, err := a.target(ctx, to)
	if err != nil {
		return "", err
	}
	params := map[string]any{"channel": ch, "text": text, "mrkdwn": true, "unfurl_links": false}
	if to.Thread != "" {
		params["thread_ts"] = to.Thread
	}
	var out struct {
		TS string `json:"ts"`
	}
	if err := a.api(ctx, t.token, "chat.postMessage", params, &out); err != nil {
		return "", sendErr(err)
	}
	if out.TS == "" {
		return "", errors.New("slack chat.postMessage: no ts in the answer")
	}
	a.replied(to)
	return out.TS, nil
}

func (a *Adapter) Edit(ctx context.Context, to channel.Address, id, text string) error {
	t, ch, err := a.target(ctx, to)
	if err != nil {
		return err
	}
	return sendErr(a.api(ctx, t.token, "chat.update", map[string]any{"channel": ch, "ts": id, "text": text}, nil))
}

func (a *Adapter) Delete(ctx context.Context, to channel.Address, id string) error {
	t, ch, err := a.target(ctx, to)
	if err != nil {
		return err
	}
	err = a.api(ctx, t.token, "chat.delete", map[string]any{"channel": ch, "ts": id}, nil)
	if err != nil && strings.Contains(err.Error(), "message_not_found") {
		return nil
	}
	return sendErr(err)
}
