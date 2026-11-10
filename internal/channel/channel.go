// Package channel connects messaging platforms to agents. Adapters speak one
// platform each; the gateway turns their messages into durable turn jobs and
// sends replies as durable delivery jobs; the supervisor keeps every adapter
// connected and restarts any that stop making progress.
package channel

import (
	"context"
	"errors"
	"time"
)

// Message is one inbound message, already stripped of the bot's own mention.
type Message struct {
	Account      string // channel account name from config
	Conversation string // platform chat id (Slack: "TEAM/CHANNEL")
	Thread       string // thread or topic within the conversation, if any
	ID           string // platform message id, used to drop redeliveries
	Sender       string // platform user id
	SenderAlt    string // another id for the same sender (WhatsApp phone number behind a LID)
	SenderName   string
	Text         string
	Direct       bool   // a one-to-one chat
	Mentioned    bool   // in a group: the bot was mentioned or replied to
	Agent        string // agent the sender picked (web chat); overrides routing when configured
	Attachments  []Attachment
	// Callback is the data of a button the sender pressed (ButtonSender).
	// Such a message is a button press, not text for the agent.
	Callback string
	// Quote is the message this one replies to, when the sender quoted one
	// and the platform says what it said; nil otherwise.
	Quote *Quote
}

// Quote is a message that a reply quotes.
type Quote struct {
	From string // who wrote it, as the platform names them; "" when unknown
	Self bool   // it is one of the bot's own messages
	Text string // what it said, or a placeholder such as "(photo)"
}

// Attachment is a file on a message. Fetch downloads it; the gateway calls
// it only for messages it accepts.
type Attachment struct {
	Name      string
	MediaType string // as declared by the platform; may be empty
	Size      int64  // 0 when unknown
	Fetch     func(ctx context.Context) ([]byte, error)
}

// Address is where a reply goes.
type Address struct {
	Account      string `json:"account"`
	Conversation string `json:"conversation"`
	Thread       string `json:"thread,omitempty"`
	ReplyTo      string `json:"reply_to,omitempty"` // message the first chunk answers
	// Trust says who started a turn, in the origin of turns only: owner
	// (listed by id or paired), guest (let in by allow_from "*") or
	// external (a webhook or trigger payload). Empty is unknown.
	Trust string `json:"trust,omitempty"`
}

// Trust levels of a turn's origin (Address.Trust).
const (
	TrustOwner    = "owner"
	TrustGuest    = "guest"
	TrustExternal = "external"
)

func (a Address) key() string { return a.Account + "|" + a.Conversation + "|" + a.Thread }

// Inbox is what an adapter reports to while it runs.
type Inbox interface {
	// Deliver hands over an inbound message. It returns quickly.
	Deliver(ctx context.Context, m Message)
	// Beat says the connection is alive. Adapters call it on every message
	// and on every successful keepalive; silence means the link is stuck.
	Beat()
	// Connected records that the adapter is online, with a short detail
	// such as the bot's name.
	Connected(detail string)
}

// Adapter is one platform connection.
type Adapter interface {
	// Run connects and reports to the inbox until ctx ends or the connection
	// fails. Returning (with or without an error) gets it restarted.
	Run(ctx context.Context, in Inbox) error
	// Send posts one chunk of already formatted text. replyTo is set on the
	// first chunk of a reply.
	Send(ctx context.Context, to Address, text string, replyTo string) error
	// Format converts the agent's Markdown into the platform's markup.
	Format(markdown string) string
	// MaxLen is the longest message the platform accepts, in characters.
	MaxLen() int
	// StaleAfter is how long the adapter may go without a Beat before the
	// supervisor treats it as hung and restarts it.
	StaleAfter() time.Duration
}

// Typer is implemented by adapters that can show a typing indicator.
type Typer interface {
	Typing(ctx context.Context, to Address) error
}

// FileSender is implemented by adapters that can post files.
type FileSender interface {
	// SendFile posts one file to the conversation (and thread). caption is
	// already formatted for the platform and may be empty. Images may be
	// shown inline; other files arrive as documents.
	SendFile(ctx context.Context, to Address, name, mediaType string, data []byte, caption string) error
	// MaxFileSize is the largest file the adapter will send, in bytes.
	MaxFileSize() int64
}

// Editor is implemented by adapters that can post a message and later edit
// or delete it. Live progress drafts need it: one "working" message that is
// edited as tools run and removed when the reply goes out.
type Editor interface {
	// Post sends one already formatted message and returns its platform id.
	Post(ctx context.Context, to Address, text string) (id string, err error)
	// Edit replaces the text of a message Post returned. Editing to the same
	// text succeeds.
	Edit(ctx context.Context, to Address, id, text string) error
	// Delete removes a message Post returned. A message already gone is not
	// an error.
	Delete(ctx context.Context, to Address, id string) error
}

// Button is one choice shown under a message.
type Button struct {
	Text  string // the label
	Data  string // comes back as Message.Callback when pressed; at most 64 bytes
	Style string // ButtonPrimary, ButtonDanger or "" for the platform's default
}

// Button styles.
const (
	ButtonPrimary = "primary"
	ButtonDanger  = "danger"
)

// ButtonSender is implemented by adapters that can show buttons under a
// message (Telegram inline keyboards, Slack Block Kit, Discord components).
// A press comes back through the inbox as a Message whose Callback is the
// button's Data; the adapter acknowledges it to the platform and removes
// the buttons, so each prompt is answered once.
type ButtonSender interface {
	// SendButtons posts already formatted text with rows of buttons.
	SendButtons(ctx context.Context, to Address, text string, rows [][]Button) error
}

// Reactor is implemented by adapters that can react to a message with an
// emoji (Telegram setMessageReaction, Slack reactions.add, Discord).
type Reactor interface {
	// React replaces the bot's reaction on a message with emoji (a Unicode
	// emoji); "" removes it.
	React(ctx context.Context, to Address, messageID, emoji string) error
}

// Poller is implemented by adapters with native polls (Telegram, Discord).
type Poller interface {
	// SendPoll posts a poll with 2 to MaxPollOptions options.
	SendPoll(ctx context.Context, to Address, question string, options []string, multi bool) error
	MaxPollOptions() int
}

// Fatal marks an adapter error that a restart cannot fix (bad token, logged
// out). The supervisor waits for the longest backoff before trying again.
type Fatal struct{ Err error }

func (f Fatal) Error() string { return f.Err.Error() }
func (f Fatal) Unwrap() error { return f.Err }

// Undeliverable marks a send that can never succeed (chat gone, bot blocked),
// so the delivery job stops retrying.
type Undeliverable struct{ Err error }

func (u Undeliverable) Error() string { return u.Err.Error() }
func (u Undeliverable) Unwrap() error { return u.Err }

// RetryAfter marks a send the platform rate-limited.
type RetryAfter struct {
	Err   error
	After time.Duration
}

func (r RetryAfter) Error() string { return r.Err.Error() }
func (r RetryAfter) Unwrap() error { return r.Err }

var errNoAdapter = errors.New("no adapter for account")

type sendKey struct{}

// SendKey is a stable identity for the send ctx carries: the delivery job and
// chunk, the same on every retry. Adapters whose platform deduplicates sends
// by a client-chosen id (Matrix transaction ids) derive that id from it. It
// is empty for notices sent outside the job system.
func SendKey(ctx context.Context) string {
	k, _ := ctx.Value(sendKey{}).(string)
	return k
}

// WithSendKey sets the key SendKey returns.
func WithSendKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, sendKey{}, key)
}
