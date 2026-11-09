package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

// Message components (channel.ButtonSender), reactions (channel.Reactor)
// and polls (channel.Poller). Button presses arrive over the gateway as
// INTERACTION_CREATE when the application has no interactions endpoint URL.

// Discord button styles.
const (
	stylePrimary   = 1
	styleSecondary = 2
	styleDanger    = 4
)

func (a *Adapter) SendButtons(ctx context.Context, to channel.Address, text string, rows [][]channel.Button) error {
	id, err := target(to)
	if err != nil {
		return err
	}
	var comps []map[string]any
	for _, row := range rows {
		var buttons []map[string]any
		for _, b := range row {
			style := styleSecondary
			switch b.Style {
			case channel.ButtonPrimary:
				style = stylePrimary
			case channel.ButtonDanger:
				style = styleDanger
			}
			buttons = append(buttons, map[string]any{"type": 2, "style": style, "label": b.Text, "custom_id": b.Data})
			if len(buttons) == 5 { // Discord's row limit
				comps = append(comps, map[string]any{"type": 1, "components": buttons})
				buttons = nil
			}
		}
		if len(buttons) > 0 {
			comps = append(comps, map[string]any{"type": 1, "components": buttons})
		}
	}
	if len(comps) > 5 {
		comps = comps[:5]
	}
	params := map[string]any{"content": text, "allowed_mentions": noPings, "flags": flagSuppressEmbeds, "components": comps}
	if err := a.call(ctx, http.MethodPost, "/channels/"+id+"/messages", params, nil); err != nil {
		return sendErr(err)
	}
	a.posted(id)
	return nil
}

type interaction struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	Type      int    `json:"type"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
	Data      struct {
		CustomID string `json:"custom_id"`
	} `json:"data"`
	Member *struct {
		Nick string `json:"nick"`
		User user   `json:"user"`
	} `json:"member"`
	User    *user `json:"user"`
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
}

// press answers a component interaction by taking the buttons off the
// prompt (keeping its text), and turns it into a message.
func (a *Adapter) press(ctx context.Context, raw json.RawMessage) (channel.Message, bool) {
	var it interaction
	if json.Unmarshal(raw, &it) != nil || it.Type != 3 || it.Data.CustomID == "" {
		return channel.Message{}, false
	}
	var u user
	nick := ""
	switch {
	case it.Member != nil:
		u, nick = it.Member.User, it.Member.Nick
	case it.User != nil:
		u = *it.User
	}
	if u.ID == "" || u.Bot {
		return channel.Message{}, false
	}
	// Discord wants an answer within three seconds: update the message.
	body, _ := json.Marshal(map[string]any{"type": 7, "data": map[string]any{"content": it.Message.Content, "components": []any{}}})
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_ = a.do(rctx, http.MethodPost, "/interactions/"+it.ID+"/"+it.Token+"/callback", "application/json", bytes.NewReader(body), nil)
	cancel()
	name := nick
	if name == "" {
		name = u.GlobalName
	}
	if name == "" {
		name = u.Username
	}
	return channel.Message{
		Conversation: it.ChannelID,
		ID:           "cb:" + it.ID,
		Sender:       u.ID,
		SenderName:   name,
		Direct:       it.GuildID == "",
		Callback:     it.Data.CustomID,
	}, true
}

// React replaces the bot's reaction on a message: the previous one is
// removed, then the new one added.
func (a *Adapter) React(ctx context.Context, to channel.Address, messageID, emoji string) error {
	id, err := target(to)
	if err != nil {
		return err
	}
	if _, err := strconv.ParseUint(messageID, 10, 64); err != nil {
		return nil // not a Discord message (a button press)
	}
	key := id + "/" + messageID
	a.mu.Lock()
	if a.reacted == nil {
		a.reacted = map[string]string{}
	}
	prev := a.reacted[key]
	a.mu.Unlock()
	if prev == emoji {
		return nil
	}
	base := "/channels/" + id + "/messages/" + messageID + "/reactions/"
	if prev != "" {
		err := a.do(ctx, http.MethodDelete, base+url.PathEscape(prev)+"/@me", "", nil, nil)
		if err != nil && !isUnknown(err) {
			return sendErr(err)
		}
	}
	if emoji != "" {
		if err := a.do(ctx, http.MethodPut, base+url.PathEscape(emoji)+"/@me", "", nil, nil); err != nil {
			return sendErr(err)
		}
	}
	a.mu.Lock()
	if emoji == "" {
		delete(a.reacted, key)
	} else {
		a.reacted[key] = emoji
	}
	a.mu.Unlock()
	return nil
}

func isUnknown(err error) bool {
	var u channel.Undeliverable
	return errors.As(err, &u)
}

func (a *Adapter) MaxPollOptions() int { return 10 }

// SendPoll posts a native poll open for a day.
func (a *Adapter) SendPoll(ctx context.Context, to channel.Address, question string, options []string, multi bool) error {
	id, err := target(to)
	if err != nil {
		return err
	}
	answers := make([]map[string]any, len(options))
	for i, o := range options {
		answers[i] = map[string]any{"poll_media": map[string]string{"text": o}}
	}
	params := map[string]any{"allowed_mentions": noPings, "poll": map[string]any{
		"question": map[string]string{"text": question}, "answers": answers, "duration": 24, "allow_multiselect": multi}}
	if err := a.call(ctx, http.MethodPost, "/channels/"+id+"/messages", params, nil); err != nil {
		return sendErr(err)
	}
	a.posted(id)
	return nil
}
