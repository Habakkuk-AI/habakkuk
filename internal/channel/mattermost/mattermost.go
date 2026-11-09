// Package mattermost is the Mattermost adapter: events over the server's
// WebSocket, posts, files and typing over its REST API, as a bot account.
package mattermost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

const (
	pingEvery   = 30 * time.Second
	maxDownload = 50 << 20
	maxUpload   = 50 << 20
)

type Adapter struct {
	URL   string // the server, e.g. https://chat.example.com
	Token string
	HTTP  *http.Client

	mu       sync.Mutex
	me       user
	threads  map[string]bool // "channel/root" threads the bot posted in
	tracking []string        // insertion order of threads, to bound it
}

type user struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

func New(serverURL, token string) *Adapter {
	return &Adapter{URL: strings.TrimRight(serverURL, "/"), Token: token, HTTP: &http.Client{Timeout: time.Minute},
		threads: map[string]bool{}}
}

func (a *Adapter) MaxLen() int               { return 16000 } // posts take 16,383 characters
func (a *Adapter) Format(md string) string   { return md }    // Mattermost renders Markdown
func (a *Adapter) StaleAfter() time.Duration { return 4 * pingEvery }
func (a *Adapter) MaxFileSize() int64        { return maxUpload }

func (a *Adapter) api(ctx context.Context, method, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, a.URL+"/api/v4"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("mattermost %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
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
	e := fmt.Errorf("mattermost %s: HTTP %d %s", path, resp.StatusCode, apiErr.Message)
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return channel.Fatal{Err: e}
	case http.StatusTooManyRequests:
		reset, _ := strconv.Atoi(resp.Header.Get("X-Ratelimit-Reset"))
		return channel.RetryAfter{Err: e, After: time.Duration(max(reset, 1)) * time.Second}
	case http.StatusForbidden, http.StatusNotFound, http.StatusBadRequest:
		return channel.Undeliverable{Err: e}
	}
	return e
}

func (a *Adapter) jsonAPI(ctx context.Context, method, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return a.api(ctx, method, path, "application/json", bytes.NewReader(raw), out)
}

func wsURL(server string) (string, error) {
	u, err := url.Parse(server + "/api/v4/websocket")
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	return u.String(), nil
}

// Run listens on the WebSocket until it fails or ctx ends. Pings every 30
// seconds keep it honest; a ping that goes unanswered ends the run.
func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	var me user
	if err := a.api(ctx, http.MethodGet, "/users/me", "", nil, &me); err != nil {
		return err
	}
	a.mu.Lock()
	a.me = me
	a.mu.Unlock()
	endpoint, err := wsURL(a.URL)
	if err != nil {
		return channel.Fatal{Err: err}
	}
	conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + a.Token}},
	})
	if err != nil {
		return fmt.Errorf("mattermost websocket: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(4 << 20)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Ping(pctx)
				pcancel()
				if err != nil {
					cancel()
					return
				}
				in.Beat()
			}
		}
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return errors.New("mattermost websocket: ping failed or shutting down")
			}
			return fmt.Errorf("mattermost websocket: %w", err)
		}
		in.Beat()
		var ev struct {
			Event string          `json:"event"`
			Data  json.RawMessage `json:"data"`
		}
		if json.Unmarshal(data, &ev) != nil {
			continue
		}
		switch ev.Event {
		case "hello":
			in.Connected("@" + me.Username + " on " + a.URL)
		case "posted":
			if m, ok := a.convert(ev.Data); ok {
				in.Deliver(ctx, m)
			}
		}
	}
}

type post struct {
	ID        string                     `json:"id"`
	UserID    string                     `json:"user_id"`
	ChannelID string                     `json:"channel_id"`
	RootID    string                     `json:"root_id"`
	Message   string                     `json:"message"`
	Type      string                     `json:"type"`
	FileIDs   []string                   `json:"file_ids"`
	Props     map[string]any             `json:"props"`
	Metadata  struct{ Files []fileInfo } `json:"metadata"`
}

type fileInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size"`
}

func (a *Adapter) convert(raw json.RawMessage) (channel.Message, bool) {
	var d struct {
		ChannelType string `json:"channel_type"` // D direct, G group, O public, P private
		SenderName  string `json:"sender_name"`
		Post        string `json:"post"`     // JSON in a string
		Mentions    string `json:"mentions"` // JSON array of user ids, in a string
	}
	if json.Unmarshal(raw, &d) != nil {
		return channel.Message{}, false
	}
	var p post
	if json.Unmarshal([]byte(d.Post), &p) != nil || p.ID == "" {
		return channel.Message{}, false
	}
	a.mu.Lock()
	me := a.me
	a.mu.Unlock()
	if p.UserID == me.ID || p.Type != "" { // own posts and system messages
		return channel.Message{}, false
	}
	var mentions []string
	_ = json.Unmarshal([]byte(d.Mentions), &mentions)
	m := channel.Message{
		Conversation: p.ChannelID, ID: p.ID, Sender: p.UserID,
		SenderAlt: strings.TrimPrefix(d.SenderName, "@"), SenderName: strings.TrimPrefix(d.SenderName, "@"),
		Direct: d.ChannelType == "D", Thread: p.RootID,
	}
	text := p.Message
	handle := "@" + me.Username
	if slices.Contains(mentions, me.ID) || (me.Username != "" && containsFold(text, handle)) {
		m.Mentioned = true
		text = replaceFold(text, handle, "")
	}
	if !m.Direct {
		if m.Thread == "" && m.Mentioned {
			m.Thread = p.ID // answer in a thread under the mention
		} else if m.Thread != "" {
			a.mu.Lock()
			m.Mentioned = m.Mentioned || a.threads[p.ChannelID+"/"+m.Thread]
			a.mu.Unlock()
		}
	}
	m.Text = strings.TrimSpace(text)
	names := map[string]fileInfo{}
	for _, f := range p.Metadata.Files {
		names[f.ID] = f
	}
	for _, id := range p.FileIDs {
		f := names[id]
		if f.Name == "" {
			f.Name = "file"
		}
		fid := id
		m.Attachments = append(m.Attachments, channel.Attachment{Name: f.Name, MediaType: f.MimeType, Size: f.Size,
			Fetch: func(ctx context.Context) ([]byte, error) { return a.download(ctx, fid) }})
	}
	return m, m.Text != "" || len(m.Attachments) > 0
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func replaceFold(s, old, repl string) string {
	i := strings.Index(strings.ToLower(s), strings.ToLower(old))
	if i < 0 {
		return s
	}
	return s[:i] + repl + s[i+len(old):]
}

func (a *Adapter) download(ctx context.Context, id string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL+"/api/v4/files/"+url.PathEscape(id), nil)
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
		return nil, fmt.Errorf("mattermost file: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
}

func (a *Adapter) noteThread(to channel.Address) {
	if to.Thread == "" {
		return
	}
	key := to.Conversation + "/" + to.Thread
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.threads[key] {
		a.threads[key] = true
		a.tracking = append(a.tracking, key)
		if len(a.tracking) > 5000 {
			delete(a.threads, a.tracking[0])
			a.tracking = a.tracking[1:]
		}
	}
}

func (a *Adapter) Send(ctx context.Context, to channel.Address, text, _ string) error {
	a.noteThread(to)
	return a.jsonAPI(ctx, http.MethodPost, "/posts", map[string]any{"channel_id": to.Conversation, "message": text, "root_id": to.Thread}, nil)
}

func (a *Adapter) Typing(ctx context.Context, to channel.Address) error {
	return a.jsonAPI(ctx, http.MethodPost, "/users/me/typing", map[string]string{"channel_id": to.Conversation, "parent_id": to.Thread}, nil)
}

// SendFile uploads the file to the channel and posts it with the caption.
func (a *Adapter) SendFile(ctx context.Context, to channel.Address, name, _ string, data []byte, caption string) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("channel_id", to.Conversation); err != nil {
		return err
	}
	fw, err := w.CreateFormFile("files", name)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	var up struct {
		FileInfos []fileInfo `json:"file_infos"`
	}
	if err := a.api(ctx, http.MethodPost, "/files", w.FormDataContentType(), &buf, &up); err != nil {
		return err
	}
	if len(up.FileInfos) == 0 {
		return errors.New("mattermost: upload returned no file")
	}
	a.noteThread(to)
	return a.jsonAPI(ctx, http.MethodPost, "/posts", map[string]any{"channel_id": to.Conversation, "message": caption,
		"root_id": to.Thread, "file_ids": []string{up.FileInfos[0].ID}}, nil)
}
