package channel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/tools/media"
)

// ParseTarget reads CHANNEL:CHAT or CHANNEL:CHAT:THREAD, as `habakkuk send`,
// schedules and the board take it.
func ParseTarget(s string) (Address, error) {
	acct, rest, ok := strings.Cut(s, ":")
	chat, thread, _ := strings.Cut(rest, ":")
	if !ok || acct == "" || chat == "" {
		return Address{}, fmt.Errorf("%q: want CHANNEL:CHAT or CHANNEL:CHAT:THREAD", s)
	}
	return Address{Account: acct, Conversation: chat, Thread: thread}, nil
}

// SendText queues a message to a conversation without running a turn. The
// delivery is durable, like a reply: `habakkuk serve` sends it, and the job ID
// makes a repeated call send once.
func (g *Gateway) SendText(ctx context.Context, id string, to Address, text string) error {
	if g.Adapters[to.Account] == nil {
		return fmt.Errorf("%w %q", errNoAdapter, to.Account)
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("the message is empty")
	}
	to.ReplyTo = ""
	return g.enqueueDelivery(ctx, id, to, text)
}

// SendFile queues a file, with an optional caption, to a conversation. The
// bytes go to the blob store first, so the delivery survives a restart.
func (g *Gateway) SendFile(ctx context.Context, id string, to Address, name string, data []byte, caption string) error {
	fs, ok := g.Adapters[to.Account].(FileSender)
	switch {
	case g.Adapters[to.Account] == nil:
		return fmt.Errorf("%w %q", errNoAdapter, to.Account)
	case !ok:
		return fmt.Errorf("channel %s cannot send files", to.Account)
	case g.Blobs == nil:
		return errors.New("no blob store to queue the file in")
	case len(data) == 0:
		return fmt.Errorf("%s is empty", name)
	case int64(len(data)) > min(fs.MaxFileSize(), maxSendFile):
		return fmt.Errorf("%s is %s; %s takes files up to %s", name, size(int64(len(data))), to.Account, size(min(fs.MaxFileSize(), maxSendFile)))
	case utf8.RuneCountInString(caption) > maxCaption:
		return fmt.Errorf("the caption is longer than %d characters", maxCaption)
	}
	name = media.SafeName(name)
	f := &outFile{Name: name, MediaType: media.Detect(name, "", data), Size: int64(len(data))}
	var err error
	if f.Ref, err = g.Blobs.Put(data); err != nil {
		return err
	}
	to.ReplyTo = ""
	return g.enqueue(ctx, id, deliverPayload{To: to, Text: strings.TrimSpace(caption), File: f})
}
