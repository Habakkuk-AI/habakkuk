package whatsapp

import (
	"context"
	"fmt"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

// SendVoice sends Ogg Opus audio as a push-to-talk audio message, which
// WhatsApp shows as a voice note. Audio messages carry no caption, so a
// caption follows as text.
func (a *Adapter) SendVoice(ctx context.Context, to channel.Address, data []byte, seconds int, caption string) error {
	cli, err := a.connected()
	if err != nil {
		return err
	}
	jid, err := types.ParseJID(to.Conversation)
	if err != nil {
		return channel.Undeliverable{Err: err}
	}
	msg, err := voiceMessage(ctx, cli.Upload, data, seconds)
	if err != nil {
		return err
	}
	if _, err := cli.SendMessage(ctx, jid, msg); err != nil {
		return err
	}
	if caption != "" {
		_, err = cli.SendMessage(ctx, jid, &waE2E.Message{Conversation: proto.String(caption)})
	}
	return err
}

func voiceMessage(ctx context.Context, upload uploader, data []byte, seconds int) (*waE2E.Message, error) {
	up, err := upload(ctx, data, whatsmeow.MediaAudio)
	if err != nil {
		return nil, fmt.Errorf("whatsapp upload: %w", err)
	}
	return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
		Mimetype: proto.String("audio/ogg; codecs=opus"), PTT: proto.Bool(true), Seconds: proto.Uint32(uint32(max(seconds, 0))),
		URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath), MediaKey: up.MediaKey,
		FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: proto.Uint64(up.FileLength),
	}}, nil
}
