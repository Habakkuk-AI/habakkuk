package telegram

import (
	"context"
	"strconv"
)

// Sender is someone who wrote to the bot in a private chat.
type Sender struct {
	ID   string // Telegram user id, as allow_from takes it
	Name string
}

// Check proves the token works (getMe) and returns the bot's username.
// It then peeks at the updates waiting for the bot and returns who wrote to
// it privately, so setup can let its owner in. The peek passes no offset,
// which confirms nothing: the adapter's first real poll still gets every
// update. A failed peek (a webhook is set, another poller) is not an error.
func (a *Adapter) Check(ctx context.Context) (username string, senders []Sender, err error) {
	var me user
	if err := a.call(ctx, "getMe", struct{}{}, &me); err != nil {
		return "", nil, err
	}
	var ups []struct {
		Message *struct {
			From *user `json:"from"`
			Chat struct {
				Type string `json:"type"`
			} `json:"chat"`
		} `json:"message"`
	}
	if a.call(ctx, "getUpdates", map[string]any{"timeout": 0, "allowed_updates": []string{"message"}}, &ups) != nil {
		return me.Username, nil, nil
	}
	seen := map[int64]bool{}
	for _, u := range ups {
		m := u.Message
		if m == nil || m.From == nil || m.From.IsBot || m.Chat.Type != "private" || seen[m.From.ID] {
			continue
		}
		seen[m.From.ID] = true
		name := m.From.FirstName
		if m.From.Username != "" {
			name += " (@" + m.From.Username + ")"
		}
		senders = append(senders, Sender{ID: strconv.FormatInt(m.From.ID, 10), Name: name})
	}
	return me.Username, senders, nil
}
