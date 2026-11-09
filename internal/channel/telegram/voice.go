package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

// SendVoice sends Ogg Opus audio with sendVoice, so it plays as a voice
// note. A user whose privacy settings refuse voice notes gets it as a file.
func (a *Adapter) SendVoice(ctx context.Context, to channel.Address, data []byte, seconds int, caption string) error {
	chat, err := a.chatID(to.Conversation)
	if err != nil {
		return err
	}
	fields := map[string]string{"chat_id": fmt.Sprint(chat)}
	if to.Thread != "" {
		fields["message_thread_id"] = to.Thread
	}
	if seconds > 0 {
		fields["duration"] = strconv.Itoa(seconds)
	}
	if caption != "" {
		fields["caption"], fields["parse_mode"] = caption, "HTML"
	}
	err = a.upload(ctx, "sendVoice", fields, "voice", "voice.ogg", data)
	if err != nil && caption != "" && (strings.Contains(err.Error(), "can't parse entities") || strings.Contains(err.Error(), "caption is too long")) {
		delete(fields, "parse_mode")
		fields["caption"] = truncate(stripTags(caption), maxCaption)
		err = a.upload(ctx, "sendVoice", fields, "voice", "voice.ogg", data)
	}
	if err != nil && strings.Contains(err.Error(), "VOICE_MESSAGES_FORBIDDEN") {
		return a.SendFile(ctx, to, "voice.ogg", "audio/ogg", data, caption)
	}
	return err
}
