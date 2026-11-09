// Package discord is the Discord adapter. It receives over the Gateway
// websocket (identify, heartbeats with ack tracking, resume) and sends over
// the REST API, so it needs no public URL.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

// Intents: guilds (thread events), guild and direct messages, and their
// content. MESSAGE_CONTENT is privileged: it must be switched on for the bot
// in the developer portal or the gateway closes with 4014.
const intents = 1<<0 | 1<<9 | 1<<12 | 1<<15

// Gateway opcodes.
const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opResume         = 6
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatAck   = 11
)

const (
	maxUpload          = 10 << 20 // what a bot may upload to a server without boosts
	flagSuppressEmbeds = 1 << 2
)

type Adapter struct {
	Token  string
	APIURL string // default https://discord.com/api/v10
	HTTP   *http.Client

	mu        sync.Mutex
	botID     string
	username  string
	session   string // gateway session to resume, kept across runs
	resumeURL string
	seq       atomic.Int64
	threads   map[string]bool // channel ids known to be threads
	replied   map[string]bool // threads the bot has posted in

	reacted map[string]string // "channel/message" -> the bot's reaction there (rich.go)
}

func New(token, apiURL string) *Adapter {
	if apiURL == "" {
		apiURL = "https://discord.com/api/v10"
	}
	return &Adapter{Token: token, APIURL: strings.TrimRight(apiURL, "/"),
		HTTP: &http.Client{Timeout: 30 * time.Second}, threads: map[string]bool{}, replied: map[string]bool{}}
}

func (a *Adapter) MaxLen() int               { return 2000 }
func (a *Adapter) StaleAfter() time.Duration { return 3 * time.Minute } // heartbeats come every ~41s

var deepHeader = regexp.MustCompile(`(?m)^#{4,6}[ \t]+(.+?)[ \t#]*$`)

// Format leaves Markdown as it is: Discord renders it. Only headings deeper
// than ### (which Discord shows literally) become bold lines.
func (a *Adapter) Format(md string) string { return deepHeader.ReplaceAllString(md, "**$1**") }

// do calls a REST endpoint and classifies failures.
func (a *Adapter) do(ctx context.Context, method, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, a.APIURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+a.Token)
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/habakkuk-ai/habakkuk, 1)")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	client := a.HTTP
	if strings.HasPrefix(contentType, "multipart/") {
		client = &http.Client{Transport: a.HTTP.Transport, Timeout: 5 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("discord %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 300 {
		if out != nil && len(raw) > 0 {
			return json.Unmarshal(raw, out)
		}
		return nil
	}
	var r struct {
		Message    string  `json:"message"`
		Code       int     `json:"code"`
		RetryAfter float64 `json:"retry_after"`
	}
	_ = json.Unmarshal(raw, &r)
	e := fmt.Errorf("discord %s %s: HTTP %d %s", method, path, resp.StatusCode, r.Message)
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		after := time.Duration(r.RetryAfter * float64(time.Second))
		if after <= 0 {
			secs, _ := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64)
			after = time.Duration(secs * float64(time.Second))
		}
		return channel.RetryAfter{Err: e, After: max(after, time.Second)}
	case http.StatusUnauthorized:
		return channel.Fatal{Err: e} // bad token
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusRequestEntityTooLarge:
		// Missing access, unknown channel, a body Discord refuses: the same
		// request will never succeed.
		return channel.Undeliverable{Err: e}
	}
	return e
}

func (a *Adapter) call(ctx context.Context, method, path string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return a.do(ctx, method, path, "application/json", bytes.NewReader(body), out)
}

type frame struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

var errZombie = errors.New("discord gateway: no heartbeat ack; reconnecting")

func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	wsURL, resuming, err := a.gatewayURL(ctx)
	if err != nil {
		return err
	}
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("discord gateway: %w", err)
	}
	// Closing without a close frame keeps the session resumable.
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(16 << 20) // GUILD_CREATE for a big server

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var hello frame
	if err := read(ctx, conn, &hello); err != nil {
		return err
	}
	var h struct {
		Interval int64 `json:"heartbeat_interval"`
	}
	if hello.Op != opHello || json.Unmarshal(hello.D, &h) != nil || h.Interval <= 0 {
		return fmt.Errorf("discord gateway: expected hello, got op %d", hello.Op)
	}
	in.Beat()
	if resuming {
		a.mu.Lock()
		d := map[string]any{"token": a.Token, "session_id": a.session, "seq": a.seq.Load()}
		a.mu.Unlock()
		err = send(ctx, conn, opResume, d)
	} else {
		err = send(ctx, conn, opIdentify, map[string]any{
			"token": a.Token, "intents": intents,
			"properties": map[string]string{"os": "linux", "browser": "habakkuk", "device": "habakkuk"},
		})
	}
	if err != nil {
		return err
	}

	var acked atomic.Bool
	acked.Store(true)
	go func() {
		interval := time.Duration(h.Interval) * time.Millisecond
		t := time.NewTimer(time.Duration(rand.Float64() * float64(interval))) // spread the first beat
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			// No ack since the last beat: the connection is a zombie.
			if !acked.Swap(false) {
				cancel(errZombie)
				return
			}
			if err := a.heartbeat(ctx, conn); err != nil {
				cancel(err)
				return
			}
			t.Reset(interval)
		}
	}()

	for {
		var f frame
		if err := read(ctx, conn, &f); err != nil {
			if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
				return cause
			}
			return a.closeErr(err)
		}
		in.Beat()
		if f.S != nil {
			a.seq.Store(*f.S)
		}
		switch f.Op {
		case opDispatch:
			a.dispatch(ctx, in, f)
		case opHeartbeat:
			if err := a.heartbeat(ctx, conn); err != nil {
				return err
			}
		case opHeartbeatAck:
			acked.Store(true)
		case opReconnect:
			return errors.New("discord asked to reconnect")
		case opInvalidSession:
			var resumable bool
			_ = json.Unmarshal(f.D, &resumable)
			if !resumable {
				a.forget()
			}
			return errors.New("discord gateway: invalid session")
		}
	}
}

// gatewayURL is the resume URL when a session can be resumed, else a fresh
// one from /gateway/bot.
func (a *Adapter) gatewayURL(ctx context.Context) (string, bool, error) {
	a.mu.Lock()
	session, resume := a.session, a.resumeURL
	a.mu.Unlock()
	base := resume
	if session == "" || base == "" {
		var r struct {
			URL string `json:"url"`
		}
		if err := a.do(ctx, http.MethodGet, "/gateway/bot", "", nil, &r); err != nil {
			return "", false, err
		}
		base, session = r.URL, ""
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", false, fmt.Errorf("discord gateway: bad url %q", base)
	}
	if u.Path == "" {
		u.Path = "/"
	}
	u.RawQuery = url.Values{"v": {"10"}, "encoding": {"json"}}.Encode()
	return u.String(), session != "", nil
}

func (a *Adapter) forget() {
	a.mu.Lock()
	a.session, a.resumeURL = "", ""
	a.mu.Unlock()
	a.seq.Store(0)
}

func (a *Adapter) heartbeat(ctx context.Context, conn *websocket.Conn) error {
	var d any // null until the first sequence number
	if s := a.seq.Load(); s > 0 {
		d = s
	}
	return send(ctx, conn, opHeartbeat, d)
}

func read(ctx context.Context, conn *websocket.Conn, f *frame) error {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, f); err != nil {
		return fmt.Errorf("discord gateway: bad frame: %w", err)
	}
	return nil
}

func send(ctx context.Context, conn *websocket.Conn, op int, d any) error {
	data, err := json.Marshal(map[string]any{"op": op, "d": d})
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.Write(wctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("discord gateway: %w", err)
	}
	return nil
}

// closeErr classifies the gateway's close codes.
func (a *Adapter) closeErr(err error) error {
	code := websocket.CloseStatus(err)
	e := fmt.Errorf("discord gateway closed (%d): %w", code, err)
	switch code {
	case 4004:
		return channel.Fatal{Err: fmt.Errorf("discord: authentication failed (check the bot token): %w", err)}
	case 4014:
		return channel.Fatal{Err: fmt.Errorf("discord: the Message Content intent is not enabled for this bot in the developer portal: %w", err)}
	case 4010, 4011, 4012, 4013:
		return channel.Fatal{Err: e}
	case 4007, 4009: // bad sequence, session timed out
		a.forget()
	}
	return e
}

type user struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
	Bot        bool   `json:"bot"`
}

type message struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
	Type      int    `json:"type"`
	Author    user   `json:"author"`
	Member    *struct {
		Nick string `json:"nick"`
	} `json:"member"`
	Content     string `json:"content"`
	WebhookID   string `json:"webhook_id"`
	Mentions    []user `json:"mentions"`
	Attachments []struct {
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Size        int64  `json:"size"`
		URL         string `json:"url"`
	} `json:"attachments"`
	Referenced *struct {
		Author user `json:"author"`
	} `json:"referenced_message"`
}

type thread struct {
	ID   string `json:"id"`
	Type int    `json:"type"`
}

// isThread: announcement, public and private threads.
func (t thread) isThread() bool { return t.Type == 10 || t.Type == 11 || t.Type == 12 }

func (a *Adapter) dispatch(ctx context.Context, in channel.Inbox, f frame) {
	switch f.T {
	case "READY":
		var r struct {
			SessionID string `json:"session_id"`
			ResumeURL string `json:"resume_gateway_url"`
			User      user   `json:"user"`
		}
		if json.Unmarshal(f.D, &r) != nil {
			return
		}
		a.mu.Lock()
		a.session, a.resumeURL, a.botID, a.username = r.SessionID, r.ResumeURL, r.User.ID, r.User.Username
		a.mu.Unlock()
		in.Connected(r.User.Username)
	case "RESUMED":
		a.mu.Lock()
		name := a.username
		a.mu.Unlock()
		in.Connected(name)
	case "GUILD_CREATE", "THREAD_LIST_SYNC":
		var g struct {
			Threads []thread `json:"threads"`
		}
		if json.Unmarshal(f.D, &g) == nil {
			a.markThreads(g.Threads...)
		}
	case "THREAD_CREATE", "THREAD_UPDATE":
		var t thread
		if json.Unmarshal(f.D, &t) == nil {
			a.markThreads(t)
		}
	case "MESSAGE_CREATE":
		var m message
		if json.Unmarshal(f.D, &m) != nil {
			return
		}
		if msg, ok := a.convert(&m); ok {
			in.Deliver(ctx, msg)
		}
	case "INTERACTION_CREATE":
		if msg, ok := a.press(ctx, f.D); ok {
			in.Deliver(ctx, msg)
		}
	}
}

func (a *Adapter) markThreads(ts ...thread) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range ts {
		if t.isThread() {
			a.threads[t.ID] = true
		}
	}
}

var userMention = regexp.MustCompile(`<@!?(\d+)>`)

func (a *Adapter) convert(m *message) (channel.Message, bool) {
	a.mu.Lock()
	botID := a.botID
	inThread := a.threads[m.ChannelID]
	followUp := inThread && a.replied[m.ChannelID]
	a.mu.Unlock()
	// Default messages and replies only: not joins, pins or boosts.
	if m.Author.Bot || m.WebhookID != "" || m.Author.ID == botID || (m.Type != 0 && m.Type != 19) {
		return channel.Message{}, false
	}
	out := channel.Message{
		Conversation: m.ChannelID, // a thread is its own channel, so its own conversation
		ID:           m.ID,
		Sender:       m.Author.ID,
		SenderName:   m.Author.GlobalName,
		Direct:       m.GuildID == "",
	}
	if out.SenderName == "" {
		out.SenderName = m.Author.Username
	}
	if m.Member != nil && m.Member.Nick != "" {
		out.SenderName = m.Member.Nick
	}
	names := map[string]string{}
	for _, u := range m.Mentions {
		names[u.ID] = u.Username
		if u.ID == botID {
			out.Mentioned = true
		}
	}
	if m.Referenced != nil && m.Referenced.Author.ID == botID {
		out.Mentioned = true // a reply to the bot
	}
	if followUp {
		out.Mentioned = true // a thread the bot is already talking in
	}
	out.Text = strings.TrimSpace(userMention.ReplaceAllStringFunc(m.Content, func(s string) string {
		id := userMention.FindStringSubmatch(s)[1]
		if id == botID {
			out.Mentioned = true
			return ""
		}
		if n := names[id]; n != "" {
			return "@" + n
		}
		return s
	}))
	for _, att := range m.Attachments {
		link := att.URL
		out.Attachments = append(out.Attachments, channel.Attachment{Name: att.Filename, MediaType: att.ContentType, Size: att.Size,
			Fetch: func(ctx context.Context) ([]byte, error) { return a.download(ctx, link) }})
	}
	if out.Text == "" && len(out.Attachments) == 0 {
		return channel.Message{}, false
	}
	return out, true
}

// download fetches an attachment from Discord's CDN; its URLs are signed.
func (a *Adapter) download(ctx context.Context, link string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: a.HTTP.Transport, Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discord download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discord download: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 100<<20))
}

// target is the channel a send goes to: the thread when the address names
// one, else the conversation.
func target(to channel.Address) (string, error) {
	id := to.Conversation
	if to.Thread != "" {
		id = to.Thread
	}
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return "", channel.Undeliverable{Err: fmt.Errorf("discord: bad channel id %q", id)}
	}
	return id, nil
}

// sendErr turns a dead token into an undeliverable send: it will not heal
// by retrying.
func sendErr(err error) error {
	var f channel.Fatal
	if errors.As(err, &f) {
		return channel.Undeliverable{Err: err}
	}
	return err
}

func (a *Adapter) posted(id string) {
	a.mu.Lock()
	if a.threads[id] {
		a.replied[id] = true
	}
	a.mu.Unlock()
}

// noPings keeps text the agent echoes (@everyone, user ids) from notifying
// anyone; a reply still notifies the person it answers.
var noPings = map[string]any{"parse": []string{}, "replied_user": true}

func (a *Adapter) Send(ctx context.Context, to channel.Address, text, replyTo string) error {
	id, err := target(to)
	if err != nil {
		return err
	}
	params := map[string]any{"content": text, "allowed_mentions": noPings, "flags": flagSuppressEmbeds}
	if replyTo != "" {
		params["message_reference"] = map[string]any{"message_id": replyTo, "fail_if_not_exists": false}
	}
	if err := a.call(ctx, http.MethodPost, "/channels/"+id+"/messages", params, nil); err != nil {
		return sendErr(err)
	}
	a.posted(id)
	return nil
}

// Typing shows "Habakkuk is typing..." for about ten seconds.
func (a *Adapter) Typing(ctx context.Context, to channel.Address) error {
	id, err := target(to)
	if err != nil {
		return err
	}
	return a.do(ctx, http.MethodPost, "/channels/"+id+"/typing", "", nil, nil)
}

func (a *Adapter) MaxFileSize() int64 { return maxUpload }

// SendFile uploads the file with the message as multipart form data; images
// show inline.
func (a *Adapter) SendFile(ctx context.Context, to channel.Address, name, mediaType string, data []byte, caption string) error {
	id, err := target(to)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"attachments":      []map[string]any{{"id": 0, "filename": name}},
		"allowed_mentions": noPings,
	}
	if caption != "" {
		payload["content"] = caption
	}
	pj, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part := func(field, filename, contentType string, body []byte) error {
		h := textproto.MIMEHeader{}
		disp := `form-data; name="` + field + `"`
		if filename != "" {
			disp += `; filename="` + strings.NewReplacer(`"`, "", "\\", "", "\r", "", "\n", "").Replace(filename) + `"`
		}
		h.Set("Content-Disposition", disp)
		h.Set("Content-Type", contentType)
		pw, err := w.CreatePart(h)
		if err == nil {
			_, err = pw.Write(body)
		}
		return err
	}
	if err := part("payload_json", "", "application/json", pj); err != nil {
		return err
	}
	if err := part("files[0]", name, mediaType, data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	if err := a.do(ctx, http.MethodPost, "/channels/"+id+"/messages", w.FormDataContentType(), &buf, nil); err != nil {
		return sendErr(err)
	}
	a.posted(id)
	return nil
}
