// Package whatsappagent is the adapter for WhatsApp's agent platform: the
// "Agents" a WhatsApp user creates under Settings > Agents, which give an
// API key. Habakkuk long-polls the platform for messages with that key, so it
// needs no public URL, no linked device and no business account. While the
// platform is in beta an agent may only talk to the person who created it,
// which suits a personal assistant. These chats are not end-to-end
// encrypted: Meta relays them.
package whatsappagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/metagraph"
)

// DefaultBase is the platform's API.
const DefaultBase = "https://api.whatsapp.com/agent/v1"

const (
	pollTimeout = 20 // seconds the server holds a poll open (it allows up to 25)
	pollLimit   = 100
	maxText     = 4096
	maxCaption  = 1024
	maxImage    = 5 << 20
	maxMedia    = 16 << 20 // video, audio, documents and other files
	maxDownload = 32 << 20
	typingEvery = 20 * time.Second // the indicator lasts 25 seconds or until the reply
	// backlog is how far before its first start a new agent still answers
	// messages, so a key created a few minutes earlier loses nothing.
	backlog = 10 * time.Minute
	// Creator is a conversation that means "whoever last wrote to the
	// agent": the only person it may message, so schedules can deliver to
	// <account>:creator without knowing the id.
	Creator = "creator"
)

// pollGap spaces poll starts so a burst of messages stays under the
// platform's 15 polls a minute. A variable for tests.
var pollGap = 4300 * time.Millisecond

// Platform error codes this adapter treats specially.
const (
	codeInvalid    = 100     // with HTTP 400 on a poll: the key is not valid
	codeNotCreator = 131005  // only the agent's creator may be messaged
	codeReplaced   = 1752041 // another poll on the same key took over
)

// Config is the adapter's settings; the token is already resolved.
type Config struct {
	Token     string // the agent's API key
	APIURL    string // default DefaultBase
	StatePath string // $HABAKKUK_HOME/channels/whatsappagent-<account>.json
}

type Adapter struct {
	cfg  Config
	api  *metagraph.Client
	Now  func() time.Time
	host string // the API's host; media may be fetched from it with the key

	mu       sync.Mutex
	loaded   bool
	state    state
	typed    map[string]time.Time // message id -> last typing indicator
	lastPoll time.Time
}

// state survives restarts: where polling stopped, and who the creator is.
type state struct {
	// Offset is the platform's next_offset, passed back unchanged. Nil
	// until the first poll answers.
	Offset *int64 `json:"offset,omitempty"`
	// Since is the unix second before which messages are history and are
	// not answered; set on the first start.
	Since int64 `json:"since,omitempty"`
	// Creator is the user:<id> of the last person who wrote; the platform
	// says to prefer a recent sender id over an old stored one.
	Creator string `json:"creator,omitempty"`
}

func New(cfg Config) *Adapter {
	if cfg.APIURL == "" {
		cfg.APIURL = DefaultBase
	}
	base := strings.TrimRight(cfg.APIURL, "/")
	a := &Adapter{cfg: cfg, api: &metagraph.Client{Name: "whatsappagent", Base: base, Token: cfg.Token,
		HTTP: &http.Client{Timeout: (pollTimeout + 40) * time.Second}}}
	if u, err := url.Parse(base); err == nil {
		a.host = u.Host
	}
	return a
}

func (a *Adapter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Adapter) MaxLen() int               { return maxText }
func (a *Adapter) Format(md string) string   { return channel.FormatWhatsApp(md) }
func (a *Adapter) StaleAfter() time.Duration { return 3 * time.Minute }
func (a *Adapter) MaxFileSize() int64        { return maxMedia }

func (a *Adapter) load() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loaded {
		return nil
	}
	if a.cfg.StatePath != "" {
		if err := channel.LoadState(a.cfg.StatePath, &a.state); err != nil {
			return fmt.Errorf("whatsappagent state: %w", err)
		}
	}
	if a.state.Since == 0 {
		a.state.Since = a.now().Add(-backlog).Unix()
		a.saveLocked()
	}
	a.loaded = true
	return nil
}

func (a *Adapter) saveLocked() {
	if a.cfg.StatePath != "" {
		_ = channel.SaveState(a.cfg.StatePath, a.state)
	}
}

type updates struct {
	Object string `json:"object"`
	Entry  []struct {
		ID      string `json:"id"`
		Changes []struct {
			Field string `json:"field"`
			Value struct {
				Contacts []struct {
					WaID    string `json:"wa_id"`
					Profile *struct {
						Name string `json:"name"`
					} `json:"profile"`
				} `json:"contacts"`
				Messages []message `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
	NextOffset *int64 `json:"next_offset"`
}

type message struct {
	From      string `json:"from"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Text      *struct {
		Body string `json:"body"`
	} `json:"text"`
	Image    *media `json:"image"`
	Audio    *media `json:"audio"`
	Video    *media `json:"video"`
	Document *media `json:"document"`
	Sticker  *media `json:"sticker"`
	// Context is set on a reply (id: the message it quotes) and on a
	// forward, as in the Cloud API.
	Context *struct {
		ID                  string `json:"id"`
		Forwarded           bool   `json:"forwarded"`
		FrequentlyForwarded bool   `json:"frequently_forwarded"`
	} `json:"context"`

	// bare: the message had no keys beyond from, id, timestamp, type and
	// text, so nothing (a forward, a reply, an ad referral, a field added
	// later) qualifies its text.
	bare bool
}

// plainKeys are the keys of a message that is text and nothing more.
var plainKeys = map[string]bool{"from": true, "id": true, "timestamp": true, "type": true, "text": true}

// UnmarshalJSON decodes a message and notes whether it is bare.
func (m *message) UnmarshalJSON(data []byte) error {
	type plainMessage message
	if err := json.Unmarshal(data, (*plainMessage)(m)); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return err
	}
	m.bare = true
	for k := range keys {
		m.bare = m.bare && plainKeys[k]
	}
	return nil
}

type media struct {
	ID       string `json:"id"`
	MimeType string `json:"mime_type"`
	Caption  string `json:"caption"`
	Filename string `json:"filename"`
	Voice    bool   `json:"voice"`
}

// Run polls for messages until ctx ends. Updates stay on the platform for
// 30 days and polling does not consume them, so the saved offset lets a
// restart pick up exactly where the last run stopped.
func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	if a.cfg.Token == "" {
		return channel.Fatal{Err: errors.New("whatsappagent: the API key variable is empty")}
	}
	if err := a.load(); err != nil {
		return err
	}
	connected := false
	for ctx.Err() == nil {
		if err := a.wait(ctx); err != nil {
			return nil
		}
		ups, err := a.poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ra channel.RetryAfter
			if errors.As(err, &ra) {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(ra.After):
				}
				continue
			}
			return err
		}
		in.Beat()
		if !connected {
			in.Connected("WhatsApp agent")
			connected = true
		}
		if ups == nil || ups.NextOffset == nil {
			continue // nothing arrived before the timeout
		}
		a.deliver(ctx, in, ups)
		a.mu.Lock()
		next := *ups.NextOffset
		a.state.Offset = &next
		a.saveLocked()
		a.mu.Unlock()
	}
	return nil
}

// wait holds the next poll until pollGap after the last one started.
func (a *Adapter) wait(ctx context.Context) error {
	a.mu.Lock()
	d := pollGap - a.now().Sub(a.lastPoll)
	a.mu.Unlock()
	if d > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
	a.mu.Lock()
	a.lastPoll = a.now()
	a.mu.Unlock()
	return nil
}

// poll asks for updates after the saved offset, or from the start of what
// the platform keeps on the very first poll (older messages are skipped by
// timestamp). nil means the poll timed out empty.
func (a *Adapter) poll(ctx context.Context) (*updates, error) {
	a.mu.Lock()
	offset := int64(0)
	if a.state.Offset != nil {
		offset = *a.state.Offset
	}
	a.mu.Unlock()
	q := url.Values{"offset": {strconv.FormatInt(offset, 10)}, "limit": {strconv.Itoa(pollLimit)},
		"timeout": {strconv.Itoa(pollTimeout)}}
	var ups updates
	err := a.api.JSON(ctx, http.MethodGet, "/updates?"+q.Encode(), nil, &ups)
	if err != nil {
		var e *metagraph.Error
		if errors.As(err, &e) {
			switch {
			case e.Status == http.StatusConflict || e.Code == codeReplaced:
				return nil, channel.Fatal{Err: fmt.Errorf("whatsappagent: another program is polling with this agent's API key; only one may (%w)", err)}
			case e.Status == http.StatusBadRequest && e.Code == codeInvalid:
				return nil, channel.Fatal{Err: fmt.Errorf("whatsappagent: the API key is not valid; it changes when it is regenerated or WhatsApp is reinstalled (%w)", err)}
			}
		}
		return nil, err
	}
	if ups.NextOffset == nil {
		return nil, nil
	}
	return &ups, nil
}

func (a *Adapter) deliver(ctx context.Context, in channel.Inbox, ups *updates) {
	a.mu.Lock()
	since := a.state.Since
	a.mu.Unlock()
	for _, e := range ups.Entry {
		for _, c := range e.Changes {
			if c.Field != "messages" {
				continue
			}
			names := map[string]string{}
			for _, ct := range c.Value.Contacts {
				if ct.Profile != nil {
					names[ct.WaID] = ct.Profile.Name
				}
			}
			for i := range c.Value.Messages {
				msg := &c.Value.Messages[i]
				if sec, err := strconv.ParseInt(msg.Timestamp, 10, 64); err == nil && sec < since {
					continue // history from before Habakkuk's first start
				}
				m, ok := a.convert(msg)
				if !ok {
					continue
				}
				m.SenderName = names[msg.From]
				a.mu.Lock()
				if a.state.Creator != msg.From {
					a.state.Creator = msg.From
					a.saveLocked()
				}
				a.mu.Unlock()
				in.Deliver(ctx, m)
			}
		}
	}
}

// convert turns a platform message into Habakkuk's. The conversation is the
// sender's id without its "user:" prefix, since Habakkuk's addresses use
// colons as separators; send puts it back.
func (a *Adapter) convert(msg *message) (channel.Message, bool) {
	conv, ok := strings.CutPrefix(msg.From, "user:")
	if !ok || conv == "" || strings.Contains(conv, ":") || msg.ID == "" {
		return channel.Message{}, false
	}
	m := channel.Message{ID: msg.ID, Sender: conv, Conversation: conv, Direct: true}
	var ref *media
	name := ""
	switch msg.Type {
	case "text":
		if msg.Text != nil {
			m.Text = msg.Text.Body
		}
		// Fail closed: Plain only for a message that is text and nothing
		// more. A forward or a reply carries "context", an ad "referral".
		m.Forwarded = msg.Context != nil && (msg.Context.Forwarded || msg.Context.FrequentlyForwarded)
		m.Plain = msg.bare
	case "image":
		ref, name = msg.Image, "image.jpg"
	case "audio":
		ref, name = msg.Audio, "audio.ogg"
		if ref != nil && ref.Voice {
			name = "voice.ogg"
		}
	case "video":
		ref, name = msg.Video, "video.mp4"
	case "document":
		ref, name = msg.Document, "document"
	case "sticker":
		ref, name = msg.Sticker, "sticker.webp"
	default:
		return channel.Message{}, false // reactions and anything newer
	}
	if ref != nil && ref.ID != "" {
		if ref.Filename != "" {
			name = ref.Filename
		}
		m.Text = ref.Caption
		id := ref.ID
		m.Attachments = []channel.Attachment{{Name: name, MediaType: ref.MimeType,
			Fetch: func(ctx context.Context) ([]byte, error) { return a.download(ctx, id) }}}
	}
	m.Text = strings.TrimSpace(m.Text)
	return m, m.Text != "" || len(m.Attachments) > 0
}

// download looks up a media id's URL, then fetches it with the key. The key
// only goes to Meta's media host or the API's own host.
func (a *Adapter) download(ctx context.Context, id string) ([]byte, error) {
	var meta struct {
		URL string `json:"url"`
	}
	if err := a.api.JSON(ctx, http.MethodGet, "/media/"+url.PathEscape(id), nil, &meta); err != nil {
		return nil, err
	}
	u, err := url.Parse(meta.URL)
	if err != nil || meta.URL == "" {
		return nil, errors.New("whatsappagent: media has no usable URL")
	}
	if !a.mediaHost(u) {
		return nil, fmt.Errorf("whatsappagent: refusing to send the API key to media host %q", u.Host)
	}
	return a.api.Download(ctx, meta.URL, true, maxDownload)
}

func (a *Adapter) mediaHost(u *url.URL) bool {
	if u.Host == a.host && u.Host != "" {
		return true
	}
	h := u.Hostname()
	return u.Scheme == "https" && (h == "lookaside.fbsbx.com" || strings.HasSuffix(h, ".whatsapp.com") ||
		strings.HasSuffix(h, ".whatsapp.net"))
}

// Resolve turns the "creator" alias into the conversation the creator's
// messages arrive in, once the creator has written, so a home chat set to
// <account>:creator is recognized (channel.Aliaser).
func (a *Adapter) Resolve(conv string) (string, bool) {
	if conv != Creator {
		return "", false
	}
	c, err := a.recipient(conv)
	if err != nil {
		return "", false
	}
	id, ok := strings.CutPrefix(c, "user:")
	return id, ok && id != ""
}

// recipient turns a conversation into the platform's user:<id>.
func (a *Adapter) recipient(conv string) (string, error) {
	if conv == "" || conv == Creator {
		a.mu.Lock()
		c := a.state.Creator
		a.mu.Unlock()
		if c == "" {
			if err := a.load(); err == nil {
				a.mu.Lock()
				c = a.state.Creator
				a.mu.Unlock()
			}
		}
		if c == "" {
			return "", channel.Undeliverable{Err: errors.New("whatsappagent: the creator has not written to the agent yet, so its id is unknown; send it one message first")}
		}
		return c, nil
	}
	if strings.HasPrefix(conv, "user:") {
		return conv, nil
	}
	return "user:" + conv, nil
}

// send posts one message object and explains the platform's one-recipient
// rule when it refuses. A 500 leaves it unknown whether the message went
// out; the delivery job retries it, so a rare duplicate is possible.
func (a *Adapter) send(ctx context.Context, conv, kind string, payload any, replyTo string) error {
	to, err := a.recipient(conv)
	if err != nil {
		return err
	}
	msg := map[string]any{"messaging_product": "whatsapp", "to": to, "type": kind, kind: payload}
	if replyTo != "" {
		msg["context"] = map[string]string{"message_id": replyTo}
	}
	err = a.api.JSON(ctx, http.MethodPost, "/messages", msg, nil)
	if code, _ := metagraph.Code(err); code == codeNotCreator {
		return channel.Undeliverable{Err: fmt.Errorf("whatsappagent: a WhatsApp agent may only message the person who created it (%w)", err)}
	}
	return err
}

func (a *Adapter) Send(ctx context.Context, to channel.Address, text, replyTo string) error {
	return a.send(ctx, to.Conversation, "text", map[string]any{"body": text}, replyTo)
}

// Typing marks the user's message read and shows the typing indicator,
// which the platform ties to that message. It is refreshed at most every
// 20 seconds, since it lasts 25 and statuses are limited to 12 a minute.
func (a *Adapter) Typing(ctx context.Context, to channel.Address) error {
	id := to.ReplyTo
	if id == "" {
		return nil
	}
	now := a.now()
	a.mu.Lock()
	if a.typed == nil {
		a.typed = map[string]time.Time{}
	}
	for k, t := range a.typed {
		if now.Sub(t) > time.Minute {
			delete(a.typed, k)
		}
	}
	if t, ok := a.typed[id]; ok && now.Sub(t) < typingEvery {
		a.mu.Unlock()
		return nil
	}
	a.typed[id] = now
	a.mu.Unlock()
	return a.api.JSON(ctx, http.MethodPost, "/statuses", map[string]any{
		"messaging_product": "whatsapp", "status": "read", "message_id": id,
		"typing_indicator": map[string]string{"type": "text"}}, nil)
}

// kind picks how WhatsApp shows a file: images, video and audio inline
// when their type and size allow it, everything else as a document.
func kind(mediaType string, size int) string {
	switch mediaType {
	case "image/jpeg", "image/png":
		if size <= maxImage {
			return "image"
		}
	case "video/mp4", "video/3gpp":
		return "video"
	case "audio/aac", "audio/mp4", "audio/mpeg", "audio/amr", "audio/ogg", "audio/opus":
		return "audio"
	}
	return "document"
}

// documentType reports whether the platform takes t as a document; any
// other file goes as application/octet-stream, which it also takes.
func documentType(t string) bool {
	switch t {
	case "text/plain", "application/pdf", "application/msword", "application/vnd.ms-excel", "application/vnd.ms-powerpoint",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return true
	}
	return false
}

// SendFile uploads the file and sends it. A caption the message cannot
// carry (audio has none, and others stop at 1024 characters) follows as
// text.
func (a *Adapter) SendFile(ctx context.Context, to channel.Address, name, mediaType string, data []byte, caption string) error {
	if len(data) > maxMedia {
		return channel.Undeliverable{Err: fmt.Errorf("whatsappagent: %s is over WhatsApp's 16 MB limit", name)}
	}
	if _, err := a.recipient(to.Conversation); err != nil {
		return err
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	if i := strings.IndexByte(mediaType, ';'); i >= 0 {
		mediaType = strings.TrimSpace(mediaType[:i])
	}
	k := kind(mediaType, len(data))
	if k == "document" && !documentType(mediaType) {
		mediaType = "application/octet-stream"
	}
	var up struct {
		ID string `json:"id"`
	}
	if err := a.api.Multipart(ctx, "/media", map[string]string{"messaging_product": "whatsapp", "type": mediaType},
		metagraph.Part{Field: "file", Name: name, MediaType: mediaType, Data: data}, &up); err != nil {
		return err
	}
	if up.ID == "" {
		return errors.New("whatsappagent: upload returned no media id")
	}
	payload := map[string]any{"id": up.ID}
	rest := caption
	if k != "audio" && caption != "" && utf8.RuneCountInString(caption) <= maxCaption {
		payload["caption"], rest = caption, ""
	}
	if k == "document" {
		payload["filename"] = name
	}
	if err := a.send(ctx, to.Conversation, k, payload, ""); err != nil {
		return err
	}
	if rest != "" {
		return a.Send(ctx, to, rest, "")
	}
	return nil
}
