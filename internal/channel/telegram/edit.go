package telegram

import (
	"context"
	"strconv"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

// Post, Edit and Delete implement channel.Editor for progress drafts.

// Post sends a message without a notification: progress drafts use it.
func (a *Adapter) Post(ctx context.Context, to channel.Address, text string) (string, error) {
	return a.postMessage(ctx, to, text, "", true)
}

// PostLoud sends a message that notifies, quoting replyTo when set: a reply
// streamed into the chat starts with it (channel.LoudPoster).
func (a *Adapter) PostLoud(ctx context.Context, to channel.Address, text, replyTo string) (string, error) {
	return a.postMessage(ctx, to, text, replyTo, false)
}

func (a *Adapter) postMessage(ctx context.Context, to channel.Address, text, replyTo string, silent bool) (string, error) {
	chat, err := a.chatID(to.Conversation)
	if err != nil {
		return "", err
	}
	params := map[string]any{"chat_id": chat, "text": text, "parse_mode": "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true}}
	if silent {
		params["disable_notification"] = true
	}
	if to.Thread != "" {
		params["message_thread_id"], _ = strconv.ParseInt(to.Thread, 10, 64)
	}
	if replyTo != "" {
		id, _ := strconv.ParseInt(replyTo, 10, 64)
		params["reply_parameters"] = map[string]any{"message_id": id, "allow_sending_without_reply": true}
	}
	var sent struct {
		MessageID int64 `json:"message_id"`
	}
	err = a.call(ctx, "sendMessage", params, &sent)
	if err != nil && strings.Contains(err.Error(), "can't parse entities") {
		delete(params, "parse_mode")
		params["text"] = stripTags(text)
		err = a.call(ctx, "sendMessage", params, &sent)
	}
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(sent.MessageID, 10), nil
}

func (a *Adapter) Edit(ctx context.Context, to channel.Address, id, text string) error {
	chat, err := a.chatID(to.Conversation)
	if err != nil {
		return err
	}
	msg, _ := strconv.ParseInt(id, 10, 64)
	params := map[string]any{"chat_id": chat, "message_id": msg, "text": text, "parse_mode": "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true}}
	err = a.call(ctx, "editMessageText", params, nil)
	if err != nil && strings.Contains(err.Error(), "can't parse entities") {
		delete(params, "parse_mode")
		params["text"] = stripTags(text)
		err = a.call(ctx, "editMessageText", params, nil)
	}
	if err != nil && strings.Contains(err.Error(), "message is not modified") {
		return nil
	}
	return err
}

func (a *Adapter) Delete(ctx context.Context, to channel.Address, id string) error {
	chat, err := a.chatID(to.Conversation)
	if err != nil {
		return err
	}
	msg, _ := strconv.ParseInt(id, 10, 64)
	err = a.call(ctx, "deleteMessage", map[string]any{"chat_id": chat, "message_id": msg}, nil)
	if err != nil && (strings.Contains(err.Error(), "message to delete not found") ||
		strings.Contains(err.Error(), "message can't be deleted")) {
		return nil // gone, or too old to delete (48h): nothing more to do
	}
	return err
}
