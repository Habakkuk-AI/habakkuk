// Package line is the LINE Messaging API adapter. LINE posts webhook events
// to Habakkuk's HTTP server, signed with the channel secret; replies use the
// event's reply token while it is fresh (free) and push messages after.
package line

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

const (
	maxBody     = 1 << 20
	maxDownload = 25 << 20
	maxImage    = 10 << 20
	replyWindow = 50 * time.Second // reply tokens last about a minute
)

type Adapter struct {
	Secret     string // channel secret: signs webhooks
	Token      string // channel access token
	APIURL     string // default https://api.line.me
	DataURL    string // default https://api-data.line.me
	WebhookURL string // <public_url>/channels/<account>; images are served below it
	HTTP       *http.Client
	Now        func() time.Time

	listener channel.Listener
	files    channel.PublicFiles

	mu      sync.Mutex
	replies map[string]replyToken // message id -> its reply token
}

type replyToken struct {
	token string
	at    time.Time
}

func New(secret, token, webhookURL, apiURL string) *Adapter {
	a := &Adapter{Secret: secret, Token: token, WebhookURL: strings.TrimRight(webhookURL, "/"),
		APIURL: "https://api.line.me", DataURL: "https://api-data.line.me", HTTP: &http.Client{Timeout: time.Minute}}
	if apiURL != "" {
		a.APIURL, a.DataURL = strings.TrimRight(apiURL, "/"), strings.TrimRight(apiURL, "/")
	}
	return a
}

func (a *Adapter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Adapter) MaxLen() int               { return 5000 }
func (a *Adapter) Format(md string) string   { return channel.FormatPlain(md) }
func (a *Adapter) StaleAfter() time.Duration { return 3 * time.Minute }
func (a *Adapter) MaxFileSize() int64        { return maxImage }

// Run checks the access token, then takes webhooks until ctx ends.
func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	if a.Secret == "" {
		return channel.Fatal{Err: errors.New("line: the channel secret variable is empty")}
	}
	var info struct {
		DisplayName string `json:"displayName"`
	}
	if err := a.call(ctx, http.MethodGet, "/v2/bot/info", nil, &info); err != nil {
		return err
	}
	return a.listener.Run(ctx, in, "LINE "+info.DisplayName, time.Minute, nil)
}

func (a *Adapter) call(ctx context.Context, method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.APIURL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("line %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 300 {
		if out != nil {
			return json.Unmarshal(raw, out)
		}
		return nil
	}
	var apiErr struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &apiErr)
	e := fmt.Errorf("line %s: HTTP %d %s", path, resp.StatusCode, apiErr.Message)
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return channel.Fatal{Err: e}
	case http.StatusTooManyRequests:
		after, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return channel.RetryAfter{Err: e, After: time.Duration(max(after, 1)) * time.Second}
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound:
		return channel.Undeliverable{Err: e}
	}
	return e
}

type mentionee struct {
	Index  int  `json:"index"`
	Length int  `json:"length"`
	IsSelf bool `json:"isSelf"`
}

type event struct {
	Type       string `json:"type"`
	Mode       string `json:"mode"`
	ReplyToken string `json:"replyToken"`
	Source     struct {
		Type    string `json:"type"` // user | group | room
		UserID  string `json:"userId"`
		GroupID string `json:"groupId"`
		RoomID  string `json:"roomId"`
	} `json:"source"`
	Message *struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Text     string `json:"text"`
		FileName string `json:"fileName"`
		FileSize int64  `json:"fileSize"`
		Mention  *struct {
			Mentionees []mentionee `json:"mentionees"`
		} `json:"mention"`
		ContentProvider struct {
			Type string `json:"type"`
		} `json:"contentProvider"`
	} `json:"message"`
}

// Valid checks X-Line-Signature: base64 HMAC-SHA256 of the body, keyed with
// the channel secret.
func Valid(secret string, body []byte, signature string) bool {
	got, err := base64.StdEncoding.DecodeString(signature)
	if err != nil || secret == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// ServeHTTP takes LINE's webhook (POST /) and serves outbound images
// (GET /media/<token>).
func (a *Adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/media/") {
		a.files.ServeHTTP(w, r)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	body, ok := channel.ReadBody(w, r, maxBody)
	if !ok {
		return
	}
	if !Valid(a.Secret, body, r.Header.Get("X-Line-Signature")) {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	var hook struct {
		Events []event `json:"events"`
	}
	if err := json.Unmarshal(body, &hook); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	in := a.listener.Inbox()
	if in == nil {
		http.Error(w, "starting", http.StatusServiceUnavailable)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	for i := range hook.Events {
		if m, ok := a.convert(&hook.Events[i]); ok {
			in.Deliver(ctx, m)
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (a *Adapter) convert(ev *event) (channel.Message, bool) {
	msg := ev.Message
	if ev.Type != "message" || ev.Mode == "standby" || msg == nil || ev.Source.UserID == "" {
		return channel.Message{}, false
	}
	m := channel.Message{ID: msg.ID, Sender: ev.Source.UserID}
	switch ev.Source.Type {
	case "group":
		m.Conversation = ev.Source.GroupID
	case "room":
		m.Conversation = ev.Source.RoomID
	default:
		m.Conversation, m.Direct = ev.Source.UserID, true
	}
	switch msg.Type {
	case "text":
		m.Text = msg.Text
		if msg.Mention != nil {
			m.Text, m.Mentioned = stripSelf(msg.Text, msg.Mention.Mentionees)
		}
	case "image", "video", "audio", "file":
		if msg.ContentProvider.Type != "" && msg.ContentProvider.Type != "line" {
			return channel.Message{}, false // hosted elsewhere
		}
		name := msg.FileName
		if name == "" {
			name = map[string]string{"image": "image.jpg", "video": "video.mp4", "audio": "audio.m4a"}[msg.Type]
		}
		id := msg.ID
		m.Attachments = []channel.Attachment{{Name: name, Size: msg.FileSize,
			Fetch: func(ctx context.Context) ([]byte, error) { return a.download(ctx, id) }}}
	default:
		return channel.Message{}, false // stickers, locations
	}
	if ev.ReplyToken != "" {
		a.mu.Lock()
		if a.replies == nil {
			a.replies = map[string]replyToken{}
		}
		for k, t := range a.replies {
			if a.now().Sub(t.at) > replyWindow {
				delete(a.replies, k)
			}
		}
		a.replies[msg.ID] = replyToken{token: ev.ReplyToken, at: a.now()}
		a.mu.Unlock()
	}
	m.Text = strings.TrimSpace(m.Text)
	return m, m.Text != "" || len(m.Attachments) > 0
}

// stripSelf removes mentions of the bot from the text. LINE's mention
// offsets count UTF-16 code units.
func stripSelf(text string, ms []mentionee) (string, bool) {
	u := utf16.Encode([]rune(text))
	self := false
	slices.SortFunc(ms, func(x, y mentionee) int { return y.Index - x.Index })
	for _, m := range ms {
		if !m.IsSelf || m.Index < 0 || m.Index+m.Length > len(u) {
			continue
		}
		self = true
		u = append(u[:m.Index], u[m.Index+m.Length:]...)
	}
	return string(utf16.Decode(u)), self
}

func (a *Adapter) download(ctx context.Context, id string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.DataURL+"/v2/bot/message/"+url.PathEscape(id)+"/content", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("line content: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
}

// takeReply returns the fresh reply token for a message, once.
func (a *Adapter) takeReply(replyTo string) string {
	if replyTo == "" {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.replies[replyTo]
	delete(a.replies, replyTo)
	if !ok || a.now().Sub(t.at) > replyWindow {
		return ""
	}
	return t.token
}

// send uses the reply token when the message answers a fresh one, which
// costs nothing, and a push message otherwise.
func (a *Adapter) send(ctx context.Context, to channel.Address, replyTo string, msgs []map[string]string) error {
	if tok := a.takeReply(replyTo); tok != "" {
		err := a.call(ctx, http.MethodPost, "/v2/bot/message/reply", map[string]any{"replyToken": tok, "messages": msgs}, nil)
		if err == nil || !errors.As(err, new(channel.Undeliverable)) {
			return err
		}
		// An expired or used token: push instead.
	}
	return a.call(ctx, http.MethodPost, "/v2/bot/message/push", map[string]any{"to": to.Conversation, "messages": msgs}, nil)
}

func (a *Adapter) Send(ctx context.Context, to channel.Address, text, replyTo string) error {
	return a.send(ctx, to, replyTo, []map[string]string{{"type": "text", "text": text}})
}

// Typing shows LINE's loading animation, which exists in one-to-one chats
// only (user ids start with U).
func (a *Adapter) Typing(ctx context.Context, to channel.Address) error {
	if !strings.HasPrefix(to.Conversation, "U") {
		return nil
	}
	return a.call(ctx, http.MethodPost, "/v2/bot/chat/loading/start", map[string]any{"chatId": to.Conversation, "loadingSeconds": 10}, nil)
}

// SendFile sends a JPEG or PNG image, which LINE fetches from a one-hour
// URL on Habakkuk's HTTP server. LINE bots cannot send other files.
func (a *Adapter) SendFile(ctx context.Context, to channel.Address, _, mediaType string, data []byte, caption string) error {
	if mediaType != "image/jpeg" && mediaType != "image/png" {
		return channel.Undeliverable{Err: errors.New("line: bots can only send JPEG and PNG images; share other files as a link")}
	}
	if !strings.HasPrefix(a.WebhookURL, "https://") {
		return channel.Undeliverable{Err: errors.New("line: sending images needs an https http.public_url")}
	}
	link := a.WebhookURL + "/media/" + a.files.Put(data, mediaType)
	msgs := []map[string]string{{"type": "image", "originalContentUrl": link, "previewImageUrl": link}}
	if caption != "" {
		msgs = append(msgs, map[string]string{"type": "text", "text": caption})
	}
	return a.send(ctx, to, "", msgs)
}
