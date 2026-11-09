// Package zulip is the Zulip adapter: a bot user that registers an event
// queue and long-polls it for messages, and posts, uploads and shows typing
// over the REST API. Streams map to conversations and topics to threads.
package zulip

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/upload"
)

const (
	maxDownload  = 50 << 20
	maxUpload    = 25 << 20 // Zulip's default max_file_upload_size_mib
	typingEvery  = 10 * time.Second
	defaultTopic = "Habakkuk"
)

type Adapter struct {
	URL    string // the organisation, e.g. https://chat.zulip.org
	Email  string // the bot's email
	APIKey string
	HTTP   *http.Client // REST calls
	Poll   *http.Client // event long-polls, which the server holds for up to 90 seconds
	Now    func() time.Time

	mu    sync.Mutex
	me    int
	name  string
	typed map[string]time.Time
}

func New(serverURL, email, apiKey string) *Adapter {
	return &Adapter{URL: strings.TrimRight(serverURL, "/"), Email: email, APIKey: apiKey,
		HTTP: &http.Client{Timeout: time.Minute}, Poll: &http.Client{Timeout: 2 * time.Minute}}
}

func (a *Adapter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Adapter) MaxLen() int               { return 10000 }
func (a *Adapter) Format(md string) string   { return md } // Zulip renders Markdown
func (a *Adapter) StaleAfter() time.Duration { return 4 * time.Minute }
func (a *Adapter) MaxFileSize() int64        { return maxUpload }

type apiError struct {
	Path   string
	Status int
	Code   string
	Msg    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("zulip %s: HTTP %d %s %s", e.Path, e.Status, e.Code, e.Msg)
}

// call makes one request; form values are sent urlencoded, as Zulip wants.
func (a *Adapter) call(ctx context.Context, client *http.Client, method, path string, form url.Values, body io.Reader, ctype string, out any) error {
	u := a.URL + "/api/v1" + path
	if form != nil && (method == http.MethodGet || method == http.MethodDelete) {
		u += "?" + form.Encode()
	} else if form != nil {
		body, ctype = strings.NewReader(form.Encode()), "application/x-www-form-urlencoded"
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(a.Email, a.APIKey)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("zulip %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var env struct {
		Result string `json:"result"`
		Msg    string `json:"msg"`
		Code   string `json:"code"`
	}
	_ = json.Unmarshal(data, &env)
	if resp.StatusCode < 300 && env.Result != "error" {
		if out != nil {
			return json.Unmarshal(data, out)
		}
		return nil
	}
	e := &apiError{Path: path, Status: resp.StatusCode, Code: env.Code, Msg: env.Msg}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return channel.Fatal{Err: e}
	case resp.StatusCode == http.StatusTooManyRequests || env.Code == "RATE_LIMIT_HIT":
		after, _ := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64)
		return channel.RetryAfter{Err: e, After: time.Duration(max(after, 1) * float64(time.Second))}
	case resp.StatusCode >= 500 || env.Code == "BAD_EVENT_QUEUE_ID":
		return e
	case resp.StatusCode >= 400:
		return channel.Undeliverable{Err: e}
	}
	return e
}

// Run registers an event queue and long-polls it. A queue the server
// forgot (after a long outage) ends the run, and the restart registers a
// new one.
func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	if a.APIKey == "" {
		return channel.Fatal{Err: errors.New("zulip: the API key variable is empty")}
	}
	var me struct {
		UserID   int    `json:"user_id"`
		FullName string `json:"full_name"`
	}
	if err := a.call(ctx, a.HTTP, http.MethodGet, "/users/me", nil, nil, "", &me); err != nil {
		return err
	}
	a.mu.Lock()
	a.me, a.name = me.UserID, me.FullName
	a.mu.Unlock()
	var reg struct {
		QueueID     string `json:"queue_id"`
		LastEventID int64  `json:"last_event_id"`
	}
	form := url.Values{"event_types": {`["message"]`}, "apply_markdown": {"false"}}
	if err := a.call(ctx, a.HTTP, http.MethodPost, "/register", form, nil, "", &reg); err != nil {
		return err
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = a.call(dctx, a.HTTP, http.MethodDelete, "/events", url.Values{"queue_id": {reg.QueueID}}, nil, "", nil)
	}()
	in.Connected("Zulip " + me.FullName)
	last := reg.LastEventID
	for {
		var got struct {
			Events []event `json:"events"`
		}
		q := url.Values{"queue_id": {reg.QueueID}, "last_event_id": {strconv.FormatInt(last, 10)}}
		if err := a.call(ctx, a.Poll, http.MethodGet, "/events", q, nil, "", &got); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		in.Beat()
		for i := range got.Events {
			ev := &got.Events[i]
			last = max(last, ev.ID)
			if ev.Type != "message" || ev.Message == nil {
				continue
			}
			if m, ok := a.convert(ev); ok {
				in.Deliver(ctx, m)
			}
		}
	}
}

type event struct {
	ID      int64    `json:"id"`
	Type    string   `json:"type"`
	Flags   []string `json:"flags"`
	Message *struct {
		ID             int64           `json:"id"`
		SenderID       int             `json:"sender_id"`
		SenderFullName string          `json:"sender_full_name"`
		Type           string          `json:"type"` // private | stream
		StreamID       int             `json:"stream_id"`
		Subject        string          `json:"subject"`
		Content        string          `json:"content"`
		Recipient      json.RawMessage `json:"display_recipient"`
	} `json:"message"`
}

var (
	uploadLink = regexp.MustCompile(`\[([^\]]*)\]\((/user_uploads/[^)\s]+)\)`)
	mention    = regexp.MustCompile(`@_?\*\*([^*|]+)(\|\d+)?\*\*`)
)

func (a *Adapter) convert(ev *event) (channel.Message, bool) {
	msg := ev.Message
	a.mu.Lock()
	me, name := a.me, a.name
	a.mu.Unlock()
	if msg.SenderID == me || msg.SenderID == 0 {
		return channel.Message{}, false
	}
	m := channel.Message{ID: strconv.FormatInt(msg.ID, 10), Sender: strconv.Itoa(msg.SenderID), SenderName: msg.SenderFullName,
		Mentioned: slices.Contains(ev.Flags, "mentioned")}
	switch msg.Type {
	case "stream":
		m.Conversation, m.Thread = "stream:"+strconv.Itoa(msg.StreamID), msg.Subject
	case "private":
		var users []struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(msg.Recipient, &users); err != nil {
			return channel.Message{}, false
		}
		var ids []int
		for _, u := range users {
			if u.ID != me {
				ids = append(ids, u.ID)
			}
		}
		slices.Sort(ids)
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = strconv.Itoa(id)
		}
		m.Conversation, m.Direct = "dm:"+strings.Join(parts, ","), len(ids) == 1
	default:
		return channel.Message{}, false
	}
	text := mention.ReplaceAllStringFunc(msg.Content, func(s string) string {
		if sub := mention.FindStringSubmatch(s); sub[1] == name || sub[2] == "|"+strconv.Itoa(me) {
			return ""
		}
		return s
	})
	for _, l := range uploadLink.FindAllStringSubmatch(text, 10) {
		name, link := l[1], l[2]
		if name == "" {
			name = link[strings.LastIndex(link, "/")+1:]
		}
		m.Attachments = append(m.Attachments, channel.Attachment{Name: name,
			Fetch: func(ctx context.Context) ([]byte, error) { return a.download(ctx, link) }})
	}
	m.Text = strings.TrimSpace(text)
	return m, m.Text != "" || len(m.Attachments) > 0
}

func (a *Adapter) download(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL+path, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(a.Email, a.APIKey)
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zulip upload %s: HTTP %d", path, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
}

// target is the recipient form for a conversation: a stream and topic, or
// the user ids of a direct message.
func target(to channel.Address) (url.Values, error) {
	if id, ok := strings.CutPrefix(to.Conversation, "stream:"); ok {
		topic := to.Thread
		if topic == "" {
			topic = defaultTopic
		}
		return url.Values{"type": {"stream"}, "to": {id}, "topic": {topic}}, nil
	}
	if ids, ok := strings.CutPrefix(to.Conversation, "dm:"); ok && ids != "" {
		return url.Values{"type": {"direct"}, "to": {"[" + ids + "]"}}, nil
	}
	return nil, channel.Undeliverable{Err: fmt.Errorf("zulip: %q is not a stream:<id> or dm:<user ids> conversation", to.Conversation)}
}

func (a *Adapter) Send(ctx context.Context, to channel.Address, text, _ string) error {
	form, err := target(to)
	if err != nil {
		return err
	}
	form.Set("content", text)
	return a.call(ctx, a.HTTP, http.MethodPost, "/messages", form, nil, "", nil)
}

// Typing starts the typing notice, at most every 10 seconds per
// conversation (Zulip shows it for 15).
func (a *Adapter) Typing(ctx context.Context, to channel.Address) error {
	form, err := target(to)
	if err != nil {
		return nil
	}
	key := to.Conversation + "\x00" + to.Thread
	now := a.now()
	a.mu.Lock()
	if a.typed == nil {
		a.typed = map[string]time.Time{}
	}
	if t, ok := a.typed[key]; ok && now.Sub(t) < typingEvery {
		a.mu.Unlock()
		return nil
	}
	for k, t := range a.typed {
		if now.Sub(t) > time.Minute {
			delete(a.typed, k)
		}
	}
	a.typed[key] = now
	a.mu.Unlock()
	if form.Get("type") == "stream" {
		form.Set("stream_id", form.Get("to"))
		form.Del("to")
	}
	form.Set("op", "start")
	return a.call(ctx, a.HTTP, http.MethodPost, "/typing", form, nil, "", nil)
}

// SendFile uploads the file and posts a link to it, which Zulip shows as
// a preview for images.
func (a *Adapter) SendFile(ctx context.Context, to channel.Address, name, mediaType string, data []byte, caption string) error {
	raw, ctype, err := upload.Form(nil, upload.File{Field: "filename", Name: name, MediaType: mediaType, Data: data})
	if err != nil {
		return err
	}
	var out struct {
		URI string `json:"uri"`
		URL string `json:"url"`
	}
	if err := a.call(ctx, a.HTTP, http.MethodPost, "/user_uploads", nil, bytes.NewReader(raw), ctype, &out); err != nil {
		return err
	}
	link := out.URL
	if link == "" {
		link = out.URI
	}
	text := "[" + strings.NewReplacer("[", "(", "]", ")").Replace(name) + "](" + link + ")"
	if caption != "" {
		text = caption + "\n" + text
	}
	return a.Send(ctx, to, text, "")
}
