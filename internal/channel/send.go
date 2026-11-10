package channel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/tools/media"
)

// ParseTarget reads CHANNEL:CHAT or CHANNEL:CHAT:THREAD, as `hk send`,
// schedules and the board take it.
func ParseTarget(s string) (Address, error) {
	acct, rest, ok := strings.Cut(s, ":")
	chat, thread, _ := strings.Cut(rest, ":")
	if !ok || acct == "" || chat == "" {
		return Address{}, fmt.Errorf("%q: want CHANNEL:CHAT or CHANNEL:CHAT:THREAD", s)
	}
	return Address{Account: acct, Conversation: chat, Thread: thread}, nil
}

// TargetNormalizer is implemented by adapters whose conversations can be
// written more than one way, such as a WhatsApp phone number for its JID.
// NormalizeTarget returns the form inbound messages use, or an error when
// nothing can ever be sent there.
type TargetNormalizer interface {
	NormalizeTarget(conversation string) (string, error)
}

// NormalizeTarget puts a target's conversation in its adapter's canonical
// form. Targets on other adapters come back unchanged.
func (g *Gateway) NormalizeTarget(to Address) (Address, error) {
	n, ok := g.Adapters[to.Account].(TargetNormalizer)
	if !ok {
		return to, nil
	}
	conv, err := n.NormalizeTarget(to.Conversation)
	if err != nil {
		return to, err
	}
	to.Conversation = conv
	return to, nil
}

// SendText queues a message to a conversation without running a turn. The
// delivery is durable, like a reply: `hk serve` sends it, and the job ID
// makes a repeated call send once.
func (g *Gateway) SendText(ctx context.Context, id string, to Address, text string) error {
	if g.Adapters[to.Account] == nil {
		return fmt.Errorf("%w %q", errNoAdapter, to.Account)
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("the message is empty")
	}
	to, err := g.NormalizeTarget(to)
	if err != nil {
		return err
	}
	to.ReplyTo = ""
	return g.enqueue(withSender(ctx), id, deliverPayload{To: to, Text: text})
}

// withSender names `hk send` as the sender of what is queued under
// ctx, unless the caller named another (notes.go).
func withSender(ctx context.Context) context.Context {
	if agent.PosterFrom(ctx) != (agent.Poster{}) {
		return ctx
	}
	return agent.WithPoster(ctx, agent.Poster{What: "`hk send`"})
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
	to, err := g.NormalizeTarget(to)
	if err != nil {
		return err
	}
	name = media.SafeName(name)
	f := &outFile{Name: name, MediaType: media.Detect(name, "", data), Size: int64(len(data))}
	if f.Ref, err = g.Blobs.Put(data); err != nil {
		return err
	}
	to.ReplyTo = ""
	return g.enqueue(withSender(ctx), id, deliverPayload{To: to, Text: strings.TrimSpace(caption), File: f})
}
