// Package messenger is the adapter for Facebook Messenger and Instagram
// messaging, both through Meta's Graph API with a Page access token. Meta
// posts webhooks to Habakkuk's HTTP server, signed with the app secret.
// Replies are standard messages inside the 24-hour window after the
// person last wrote; Habakkuk never uses message tags such as human_agent to
// write outside it, so such sends fail with the reason.
package messenger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/metagraph"
)

const (
	maxBody     = 4 << 20
	maxDownload = 25 << 20
	maxFile     = 25 << 20
	maxIGImage  = 8 << 20
	typingEvery = 15 * time.Second // typing_on lasts about 20 seconds
)

// Graph error code for a send outside the messaging window.
const codeWindow = 10

type Config struct {
	Instagram   bool   // Instagram messaging instead of Messenger
	PageID      string // optional: only this Page's (or Instagram account's) events; replies go out as it
	Token       string // Page access token
	AppSecret   string // signs webhooks
	VerifyToken string // hub.verify_token chosen when subscribing the webhook
	APIURL      string // default metagraph.DefaultBase
	WebhookURL  string // <public_url>/channels/<account>; Instagram fetches outbound media below it
}

type Adapter struct {
	cfg      Config
	graph    *metagraph.Client
	Now      func() time.Time
	listener channel.Listener
	files    channel.PublicFiles

	mu    sync.Mutex
	typed map[string]time.Time // conversation -> last typing_on
}

func New(cfg Config) *Adapter {
	if cfg.APIURL == "" {
		cfg.APIURL = metagraph.DefaultBase
	}
	cfg.WebhookURL = strings.TrimRight(cfg.WebhookURL, "/")
	return &Adapter{cfg: cfg, graph: &metagraph.Client{Name: cfg.name(), Base: strings.TrimRight(cfg.APIURL, "/"),
		Token: cfg.Token, HTTP: &http.Client{Timeout: time.Minute}}}
}

func (c Config) name() string {
	if c.Instagram {
		return "instagram"
	}
	return "messenger"
}

func (a *Adapter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Adapter) MaxLen() int {
	if a.cfg.Instagram {
		return 1000
	}
	return 2000
}
func (a *Adapter) Format(md string) string   { return channel.FormatPlain(md) }
func (a *Adapter) StaleAfter() time.Duration { return 3 * time.Minute }
func (a *Adapter) MaxFileSize() int64        { return maxFile }

func (a *Adapter) sender() string {
	if a.cfg.PageID != "" {
		return "/" + a.cfg.PageID
	}
	return "/me"
}

// Run checks the Page token, then takes webhooks until ctx ends.
func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	n := a.cfg.name()
	switch {
	case a.cfg.Token == "":
		return channel.Fatal{Err: fmt.Errorf("%s: the access token variable is empty", n)}
	case a.cfg.AppSecret == "":
		return channel.Fatal{Err: fmt.Errorf("%s: the app secret variable is empty", n)}
	case a.cfg.VerifyToken == "":
		return channel.Fatal{Err: fmt.Errorf("%s: the verify token variable is empty", n)}
	}
	var me struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := a.graph.JSON(ctx, http.MethodGet, "/me?fields=id,name", nil, &me); err != nil {
		return err
	}
	label := "Messenger "
	if a.cfg.Instagram {
		label = "Instagram "
	}
	return a.listener.Run(ctx, in, label+me.Name, time.Minute, nil)
}

type webhook struct {
	Object string `json:"object"`
	Entry  []struct {
		ID        string  `json:"id"`
		Messaging []event `json:"messaging"`
	} `json:"entry"`
}

type event struct {
	Sender struct {
		ID string `json:"id"`
	} `json:"sender"`
	Recipient struct {
		ID string `json:"id"`
	} `json:"recipient"`
	Message *struct {
		MID         string `json:"mid"`
		Text        string `json:"text"`
		IsEcho      bool   `json:"is_echo"`
		IsDeleted   bool   `json:"is_deleted"`
		Attachments []struct {
			Type    string `json:"type"`
			Payload struct {
				URL string `json:"url"`
			} `json:"payload"`
		} `json:"attachments"`
		QuickReply *struct {
			Payload string `json:"payload"`
		} `json:"quick_reply"`
	} `json:"message"`
	Postback *struct {
		MID     string `json:"mid"`
		Title   string `json:"title"`
		Payload string `json:"payload"`
	} `json:"postback"`
}

// ServeHTTP answers the subscription handshake (GET /), takes webhooks
// (POST /) and serves Instagram's outbound media (GET /media/<token>).
func (a *Adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/media/") {
		a.files.ServeHTTP(w, r)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		metagraph.Challenge(w, r, a.cfg.VerifyToken)
		return
	case http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, ok := channel.ReadBody(w, r, maxBody)
	if !ok {
		return
	}
	if !metagraph.ValidSignature(a.cfg.AppSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	var hook webhook
	if err := json.Unmarshal(body, &hook); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	in := a.listener.Inbox()
	if in == nil {
		http.Error(w, "starting", http.StatusServiceUnavailable)
		return
	}
	want := "page"
	if a.cfg.Instagram {
		want = "instagram"
	}
	if hook.Object == want {
		ctx := context.WithoutCancel(r.Context())
		for _, e := range hook.Entry {
			if a.cfg.PageID != "" && e.ID != a.cfg.PageID {
				continue
			}
			for i := range e.Messaging {
				if m, ok := a.convert(&e.Messaging[i]); ok {
					in.Deliver(ctx, m)
				}
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (a *Adapter) convert(ev *event) (channel.Message, bool) {
	from := ev.Sender.ID
	if from == "" || (a.cfg.PageID != "" && from == a.cfg.PageID) {
		return channel.Message{}, false
	}
	m := channel.Message{Sender: from, Conversation: from, Direct: true}
	switch {
	case ev.Postback != nil:
		m.ID, m.Callback = ev.Postback.MID, ev.Postback.Payload
		if m.ID == "" {
			m.ID = "pb:" + from + ":" + ev.Postback.Payload
		}
		return m, m.Callback != ""
	case ev.Message == nil:
		return channel.Message{}, false // reads, deliveries, reactions
	}
	msg := ev.Message
	if msg.IsEcho || msg.IsDeleted || msg.MID == "" {
		return channel.Message{}, false
	}
	m.ID = msg.MID
	if msg.QuickReply != nil && msg.QuickReply.Payload != "" {
		m.Callback = msg.QuickReply.Payload
		return m, true
	}
	m.Text = strings.TrimSpace(msg.Text)
	for i, att := range msg.Attachments {
		name, ok := map[string]string{"image": "image.jpg", "video": "video.mp4", "audio": "audio.mp4", "file": "file"}[att.Type]
		link := att.Payload.URL
		if !ok || link == "" {
			continue // shares, stickers, story mentions, reels
		}
		if i > 0 {
			name = fmt.Sprintf("%d-%s", i+1, name)
		}
		m.Attachments = append(m.Attachments, channel.Attachment{Name: name,
			Fetch: func(ctx context.Context) ([]byte, error) { return a.graph.Download(ctx, link, false, maxDownload) }})
	}
	return m, m.Text != "" || len(m.Attachments) > 0
}

// wrap explains a send refused for the messaging window.
func (a *Adapter) wrap(err error) error {
	if code, _ := metagraph.Code(err); code == codeWindow {
		return channel.Undeliverable{Err: fmt.Errorf("%s: the person last wrote more than 24 hours ago, and Habakkuk does not use message tags to write outside that window: %w", a.cfg.name(), err)}
	}
	return err
}

func (a *Adapter) send(ctx context.Context, to string, message any) error {
	return a.wrap(a.graph.JSON(ctx, http.MethodPost, a.sender()+"/messages", map[string]any{
		"recipient": map[string]string{"id": to}, "messaging_type": "RESPONSE", "message": message}, nil))
}

func (a *Adapter) Send(ctx context.Context, to channel.Address, text, _ string) error {
	return a.send(ctx, to.Conversation, map[string]string{"text": text})
}

// Typing turns on the typing bubble, at most every 15 seconds per person.
func (a *Adapter) Typing(ctx context.Context, to channel.Address) error {
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
	if t, ok := a.typed[to.Conversation]; ok && now.Sub(t) < typingEvery {
		a.mu.Unlock()
		return nil
	}
	a.typed[to.Conversation] = now
	a.mu.Unlock()
	return a.wrap(a.graph.JSON(ctx, http.MethodPost, a.sender()+"/messages", map[string]any{
		"recipient": map[string]string{"id": to.Conversation}, "sender_action": "typing_on"}, nil))
}

// attachmentType is how a file is shown: image, video, audio or file.
func attachmentType(mediaType string) string {
	switch {
	case mediaType == "image/jpeg" || mediaType == "image/png" || mediaType == "image/gif":
		return "image"
	case strings.HasPrefix(mediaType, "video/"):
		return "video"
	case strings.HasPrefix(mediaType, "audio/"):
		return "audio"
	}
	return "file"
}

// SendFile uploads the file with the message on Messenger. Instagram only
// takes media by URL, so it fetches the file from Habakkuk's HTTP server.
// A caption follows as its own message.
func (a *Adapter) SendFile(ctx context.Context, to channel.Address, name, mediaType string, data []byte, caption string) error {
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	t := attachmentType(mediaType)
	var err error
	if a.cfg.Instagram {
		err = a.sendByURL(ctx, to.Conversation, t, mediaType, data)
	} else {
		recipient, _ := json.Marshal(map[string]string{"id": to.Conversation})
		message, _ := json.Marshal(map[string]any{"attachment": map[string]any{"type": t, "payload": map[string]bool{"is_reusable": false}}})
		err = a.wrap(a.graph.Multipart(ctx, a.sender()+"/messages",
			map[string]string{"recipient": string(recipient), "message": string(message), "messaging_type": "RESPONSE"},
			metagraph.Part{Field: "filedata", Name: name, MediaType: mediaType, Data: data}, nil))
	}
	if err != nil || caption == "" {
		return err
	}
	return a.Send(ctx, to, caption, "")
}

func (a *Adapter) sendByURL(ctx context.Context, to, t, mediaType string, data []byte) error {
	switch {
	case t == "file" && mediaType != "application/pdf":
		return channel.Undeliverable{Err: errors.New("instagram: only images, video, audio and PDF files can be sent; share other files as a link")}
	case t == "image" && len(data) > maxIGImage:
		return channel.Undeliverable{Err: errors.New("instagram: images are limited to 8 MB")}
	case !strings.HasPrefix(a.cfg.WebhookURL, "https://"):
		return channel.Undeliverable{Err: errors.New("instagram: sending files needs an https http.public_url, which Instagram fetches them from")}
	}
	link := a.cfg.WebhookURL + "/media/" + a.files.Put(data, mediaType)
	return a.send(ctx, to, map[string]any{"attachment": map[string]any{"type": t, "payload": map[string]string{"url": link}}})
}

// SendButtons posts the choices as quick replies (at most 13), which come
// back as the payload of the reply.
func (a *Adapter) SendButtons(ctx context.Context, to channel.Address, text string, rows [][]channel.Button) error {
	var replies []any
	for _, r := range rows {
		for _, b := range r {
			title := b.Text
			if utf8.RuneCountInString(title) > 20 {
				title = string([]rune(title)[:19]) + "…"
			}
			replies = append(replies, map[string]string{"content_type": "text", "title": title, "payload": b.Data})
		}
	}
	if len(replies) == 0 || len(replies) > 13 {
		return fmt.Errorf("%s: quick replies take 1 to 13 choices", a.cfg.name())
	}
	return a.send(ctx, to.Conversation, map[string]any{"text": text, "quick_replies": replies})
}
