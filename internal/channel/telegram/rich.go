package telegram

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

// Inline keyboards (channel.ButtonSender), reactions (channel.Reactor) and
// polls (channel.Poller).

type callbackQuery struct {
	ID      string   `json:"id"`
	From    *user    `json:"from"`
	Message *message `json:"message"`
	Data    string   `json:"data"`
}

func (a *Adapter) SendButtons(ctx context.Context, to channel.Address, text string, rows [][]channel.Button) error {
	chat, err := a.chatID(to.Conversation)
	if err != nil {
		return err
	}
	keyboard := make([][]map[string]string, len(rows))
	for i, row := range rows {
		for _, b := range row {
			keyboard[i] = append(keyboard[i], map[string]string{"text": b.Text, "callback_data": b.Data})
		}
	}
	params := map[string]any{"chat_id": chat, "text": text, "parse_mode": "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true},
		"reply_markup":         map[string]any{"inline_keyboard": keyboard}}
	if to.Thread != "" {
		params["message_thread_id"], _ = strconv.ParseInt(to.Thread, 10, 64)
	}
	err = a.call(ctx, "sendMessage", params, nil)
	if err != nil && strings.Contains(err.Error(), "can't parse entities") {
		delete(params, "parse_mode")
		params["text"] = stripTags(text)
		err = a.call(ctx, "sendMessage", params, nil)
	}
	return err
}

// press acknowledges a button press, takes the buttons off the message so
// the prompt is answered once, and turns the press into a message.
func (a *Adapter) press(ctx context.Context, q *callbackQuery) (channel.Message, bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Stops the button's loading spinner; failures only cost that.
	_ = a.call(ctx, "answerCallbackQuery", map[string]string{"callback_query_id": q.ID}, nil)
	if q.From == nil || q.From.IsBot || q.Message == nil || q.Data == "" {
		return channel.Message{}, false
	}
	_ = a.call(ctx, "editMessageReplyMarkup", map[string]any{"chat_id": q.Message.Chat.ID, "message_id": q.Message.MessageID,
		"reply_markup": map[string]any{"inline_keyboard": [][]any{}}}, nil)
	m := channel.Message{
		Conversation: strconv.FormatInt(q.Message.Chat.ID, 10),
		ID:           "cb:" + q.ID,
		Sender:       strconv.FormatInt(q.From.ID, 10),
		SenderName:   strings.TrimSpace(q.From.FirstName + " " + q.From.LastName),
		Direct:       q.Message.Chat.Type == "private",
		Callback:     q.Data,
	}
	if q.Message.IsTopicMessage && q.Message.MessageThreadID != 0 {
		m.Thread = strconv.FormatInt(q.Message.MessageThreadID, 10)
	}
	return m, true
}

// React sets the bot's one reaction on a message; a new one replaces it.
// Bots may only use Telegram's standard reaction emoji.
func (a *Adapter) React(ctx context.Context, to channel.Address, messageID, emoji string) error {
	chat, err := a.chatID(to.Conversation)
	if err != nil {
		return err
	}
	id, err := strconv.ParseInt(messageID, 10, 64)
	if err != nil {
		return nil // not a message Telegram knows (a button press)
	}
	reaction := []map[string]string{}
	if emoji != "" {
		reaction = append(reaction, map[string]string{"type": "emoji", "emoji": emoji})
	}
	return a.call(ctx, "setMessageReaction", map[string]any{"chat_id": chat, "message_id": id, "reaction": reaction}, nil)
}

func (a *Adapter) MaxPollOptions() int { return 10 }

// SendPoll posts a native poll that shows who voted.
func (a *Adapter) SendPoll(ctx context.Context, to channel.Address, question string, options []string, multi bool) error {
	chat, err := a.chatID(to.Conversation)
	if err != nil {
		return err
	}
	opts := make([]map[string]string, len(options))
	for i, o := range options {
		opts[i] = map[string]string{"text": o}
	}
	params := map[string]any{"chat_id": chat, "question": question, "options": opts,
		"is_anonymous": false, "allows_multiple_answers": multi}
	if to.Thread != "" {
		params["message_thread_id"], _ = strconv.ParseInt(to.Thread, 10, 64)
	}
	return a.call(ctx, "sendPoll", params, nil)
}
