package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

// Block Kit buttons (channel.ButtonSender) and reactions (channel.Reactor).
// Buttons need Interactivity switched on for the app (Socket Mode delivers
// the presses); reactions need the reactions:write scope.

const maxSectionText = 3000

func (a *Adapter) SendButtons(ctx context.Context, to channel.Address, text string, rows [][]channel.Button) error {
	t, ch, err := a.target(ctx, to)
	if err != nil {
		return err
	}
	section := text
	if r := []rune(section); len(r) > maxSectionText {
		section = string(r[:maxSectionText-1]) + "…"
	}
	blocks := []map[string]any{{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": section}}}
	n := 0
	for _, row := range rows {
		var elems []map[string]any
		for _, b := range row {
			e := map[string]any{"type": "button", "text": map[string]any{"type": "plain_text", "text": b.Text, "emoji": true},
				"value": b.Data, "action_id": fmt.Sprintf("habakkuk_%d", n)}
			if b.Style == channel.ButtonPrimary || b.Style == channel.ButtonDanger {
				e["style"] = b.Style
			}
			elems = append(elems, e)
			n++
		}
		blocks = append(blocks, map[string]any{"type": "actions", "elements": elems})
	}
	params := map[string]any{"channel": ch, "text": text, "blocks": blocks, "unfurl_links": false}
	if to.Thread != "" {
		params["thread_ts"] = to.Thread
	}
	if err := a.api(ctx, t.token, "chat.postMessage", params, nil); err != nil {
		return sendErr(err)
	}
	a.replied(to)
	return nil
}

// interaction is the part of a block_actions payload the adapter reads.
type interaction struct {
	Type string `json:"type"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Name     string `json:"name"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Message struct {
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
		Text     string `json:"text"`
	} `json:"message"`
	Actions []struct {
		Value    string `json:"value"`
		ActionTS string `json:"action_ts"`
	} `json:"actions"`
}

// press turns a button press into a message and takes the buttons off the
// prompt, leaving its text, so it is answered once.
func (a *Adapter) press(ctx context.Context, payload json.RawMessage) (channel.Message, bool) {
	var p interaction
	if json.Unmarshal(payload, &p) != nil || p.Type != "block_actions" || len(p.Actions) == 0 || p.Actions[0].Value == "" {
		return channel.Message{}, false
	}
	t := a.team(p.Team.ID)
	if t == nil || p.User.ID == "" || p.Channel.ID == "" {
		return channel.Message{}, false
	}
	if p.Message.TS != "" {
		uctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = a.api(uctx, t.token, "chat.update", map[string]any{"channel": p.Channel.ID, "ts": p.Message.TS,
			"text": p.Message.Text, "blocks": []map[string]any{{"type": "section",
				"text": map[string]string{"type": "mrkdwn", "text": orDash(p.Message.Text)}}}}, nil)
		cancel()
	}
	name := p.User.Name
	if name == "" {
		name = p.User.Username
	}
	return channel.Message{
		Conversation: t.id + "/" + p.Channel.ID,
		Thread:       p.Message.ThreadTS,
		ID:           "cb:" + p.Actions[0].ActionTS,
		Sender:       p.User.ID,
		SenderName:   name,
		Direct:       strings.HasPrefix(p.Channel.ID, "D"),
		Callback:     p.Actions[0].Value,
	}, true
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// slackEmoji names the emoji the gateway reacts with.
var slackEmoji = map[string]string{"👀": "eyes", "👍": "+1", "😢": "cry", "✅": "white_check_mark", "❌": "x"}

// React replaces the bot's reaction on a message: Slack keeps several, so
// the previous one is removed first.
func (a *Adapter) React(ctx context.Context, to channel.Address, messageID, emoji string) error {
	t, ch, err := a.target(ctx, to)
	if err != nil {
		return err
	}
	if strings.HasPrefix(messageID, "cb:") {
		return nil
	}
	key := to.Conversation + "/" + messageID
	a.mu.Lock()
	if a.reacted == nil {
		a.reacted = map[string]string{}
	}
	prev := a.reacted[key]
	a.mu.Unlock()
	name := ""
	if emoji != "" {
		if name = slackEmoji[emoji]; name == "" {
			return fmt.Errorf("slack: no name for the emoji %q", emoji)
		}
	}
	if prev == name {
		return nil
	}
	if prev != "" {
		err := a.api(ctx, t.token, "reactions.remove", map[string]any{"channel": ch, "timestamp": messageID, "name": prev}, nil)
		if err != nil && !strings.Contains(err.Error(), "no_reaction") {
			return sendErr(err)
		}
	}
	if name != "" {
		err := a.api(ctx, t.token, "reactions.add", map[string]any{"channel": ch, "timestamp": messageID, "name": name}, nil)
		if err != nil && !strings.Contains(err.Error(), "already_reacted") {
			return sendErr(err)
		}
	}
	a.mu.Lock()
	if name == "" {
		delete(a.reacted, key)
	} else {
		a.reacted[key] = name
	}
	a.mu.Unlock()
	return nil
}
