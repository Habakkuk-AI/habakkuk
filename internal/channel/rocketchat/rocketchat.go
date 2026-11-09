// Package rocketchat is the Rocket.Chat adapter: a bot user that follows
// its rooms over the realtime API (DDP over a WebSocket, subscribed to
// "__my_messages__") and posts and uploads over the REST API with a
// personal access token.
package rocketchat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/upload"
)

const (
	maxDownload = 50 << 20
	maxUpload   = 50 << 20
	pingEvery   = 30 * time.Second
)

type Adapter struct {
	URL    string // the server, e.g. https://chat.example.com
	UserID string // the bot's user id
	Token  string // a personal access token of the bot
	HTTP   *http.Client

	username string
}

func New(serverURL, userID, token string) *Adapter {
	return &Adapter{URL: strings.TrimRight(serverURL, "/"), UserID: userID, Token: token, HTTP: &http.Client{Timeout: time.Minute}}
}

func (a *Adapter) MaxLen() int               { return 5000 } // Message_MaxAllowedSize default
func (a *Adapter) Format(md string) string   { return md }   // Rocket.Chat renders Markdown
func (a *Adapter) StaleAfter() time.Duration { return 4 * pingEvery }
func (a *Adapter) MaxFileSize() int64        { return maxUpload }

type apiError struct {
	Path   string
	Status int
	Msg    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("rocketchat %s: HTTP %d %s", e.Path, e.Status, e.Msg)
}

func (a *Adapter) call(ctx context.Context, method, path string, body io.Reader, ctype string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, a.URL+"/api/v1"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-User-Id", a.UserID)
	req.Header.Set("X-Auth-Token", a.Token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("rocketchat %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 300 {
		if out != nil {
			return json.Unmarshal(data, out)
		}
		return nil
	}
	var env struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &env)
	e := &apiError{Path: path, Status: resp.StatusCode, Msg: env.Error + env.Message}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return channel.Fatal{Err: e}
	case resp.StatusCode == http.StatusTooManyRequests:
		reset, _ := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
		after := time.Until(time.UnixMilli(reset))
		return channel.RetryAfter{Err: e, After: min(max(after, time.Second), time.Minute)}
	case resp.StatusCode >= 500:
		return e
	}
	return channel.Undeliverable{Err: e}
}

func (a *Adapter) postJSON(ctx context.Context, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return a.call(ctx, http.MethodPost, path, bytes.NewReader(raw), "application/json", out)
}

// Run checks the token, connects to the realtime API, logs in with the
// token and follows every room the bot is in until the socket fails.
func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	if a.Token == "" {
		return channel.Fatal{Err: errors.New("rocketchat: the token variable is empty")}
	}
	var me struct {
		ID       string `json:"_id"`
		Username string `json:"username"`
	}
	if err := a.call(ctx, http.MethodGet, "/me", nil, "", &me); err != nil {
		return err
	}
	a.username = me.Username
	endpoint := "ws" + strings.TrimPrefix(a.URL, "http") + "/websocket"
	conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: a.HTTP})
	if err != nil {
		return fmt.Errorf("rocketchat websocket: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(4 << 20)
	send := func(v any) error {
		raw, _ := json.Marshal(v)
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return conn.Write(wctx, websocket.MessageText, raw)
	}
	steps := []any{
		map[string]any{"msg": "connect", "version": "1", "support": []string{"1"}},
		map[string]any{"msg": "method", "method": "login", "id": "login", "params": []any{map[string]string{"resume": a.Token}}},
		map[string]any{"msg": "sub", "id": "sub", "name": "stream-room-messages", "params": []any{"__my_messages__", false}},
	}
	for _, s := range steps {
		if err := send(s); err != nil {
			return fmt.Errorf("rocketchat websocket: %w", err)
		}
	}
	go func() {
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if send(map[string]string{"msg": "ping"}) != nil {
					_ = conn.CloseNow()
					return
				}
			}
		}
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("rocketchat websocket: %w", err)
		}
		in.Beat()
		var f frame
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		switch f.Msg {
		case "ping":
			if err := send(map[string]string{"msg": "pong"}); err != nil {
				return err
			}
		case "result":
			if f.ID == "login" && f.Error != nil {
				return channel.Fatal{Err: fmt.Errorf("rocketchat login: %s", f.Error.Message)}
			}
		case "ready":
			in.Connected("Rocket.Chat @" + me.Username)
		case "nosub":
			return errors.New("rocketchat: the message subscription was refused")
		case "changed":
			if f.Collection != "stream-room-messages" || len(f.Fields.Args) < 1 {
				continue
			}
			var msg message
			var room roomInfo
			if json.Unmarshal(f.Fields.Args[0], &msg) != nil {
				continue
			}
			if len(f.Fields.Args) > 1 {
				_ = json.Unmarshal(f.Fields.Args[1], &room)
			}
			if m, ok := a.convert(&msg, &room); ok {
				in.Deliver(ctx, m)
			}
		}
	}
}

type frame struct {
	Msg        string `json:"msg"`
	ID         string `json:"id"`
	Collection string `json:"collection"`
	Error      *struct {
		Message string `json:"message"`
	} `json:"error"`
	Fields struct {
		EventName string            `json:"eventName"`
		Args      []json.RawMessage `json:"args"`
	} `json:"fields"`
}

type message struct {
	ID   string `json:"_id"`
	RID  string `json:"rid"`
	Msg  string `json:"msg"`
	TMID string `json:"tmid"`
	T    string `json:"t"` // system messages: joins, renames
	U    struct {
		ID       string `json:"_id"`
		Username string `json:"username"`
		Name     string `json:"name"`
	} `json:"u"`
	Mentions []struct {
		ID       string `json:"_id"`
		Username string `json:"username"`
	} `json:"mentions"`
	EditedAt    json.RawMessage `json:"editedAt"`
	Attachments []struct {
		Title     string `json:"title"`
		TitleLink string `json:"title_link"`
		Type      string `json:"type"`
	} `json:"attachments"`
}

type roomInfo struct {
	RoomType string `json:"roomType"` // d direct, c channel, p private group, l livechat
}

func (a *Adapter) convert(msg *message, room *roomInfo) (channel.Message, bool) {
	if msg.ID == "" || msg.RID == "" || msg.T != "" || len(msg.EditedAt) > 0 || msg.U.ID == "" || msg.U.ID == a.UserID {
		return channel.Message{}, false
	}
	m := channel.Message{ID: msg.ID, Conversation: msg.RID, Thread: msg.TMID, Sender: msg.U.ID,
		SenderAlt: msg.U.Username, SenderName: msg.U.Name, Direct: room.RoomType == "d"}
	text := msg.Msg
	for _, mn := range msg.Mentions {
		if mn.ID == a.UserID {
			m.Mentioned = true
			text = strings.ReplaceAll(text, "@"+a.username, "")
		}
	}
	for _, att := range msg.Attachments {
		link := att.TitleLink
		if att.Type != "file" || !strings.HasPrefix(link, "/file-upload/") {
			continue
		}
		name := att.Title
		if name == "" {
			name = "file"
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
	req.Header.Set("X-User-Id", a.UserID)
	req.Header.Set("X-Auth-Token", a.Token)
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rocketchat file: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
}

// Send posts to the room, in the thread when there is one.
func (a *Adapter) Send(ctx context.Context, to channel.Address, text, _ string) error {
	msg := map[string]string{"rid": to.Conversation, "msg": text}
	if to.Thread != "" {
		msg["tmid"] = to.Thread
	}
	return a.postJSON(ctx, "/chat.sendMessage", map[string]any{"message": msg}, nil)
}

// SendFile uploads the file to the room with the caption.
func (a *Adapter) SendFile(ctx context.Context, to channel.Address, name, mediaType string, data []byte, caption string) error {
	fields := map[string]string{}
	if caption != "" {
		fields["msg"] = caption
	}
	if to.Thread != "" {
		fields["tmid"] = to.Thread
	}
	raw, ctype, err := upload.Form(fields, upload.File{Field: "file", Name: name, MediaType: mediaType, Data: data})
	if err != nil {
		return err
	}
	return a.call(ctx, http.MethodPost, "/rooms.upload/"+url.PathEscape(to.Conversation), bytes.NewReader(raw), ctype, nil)
}
