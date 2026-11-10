// Package slack is the Slack adapter. One Socket Mode connection (the app
// token) receives events from every workspace the app is installed in; each
// workspace has its own bot token for sending.
package slack

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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

const pingEvery = 20 * time.Second

type Adapter struct {
	AppToken  string
	BotTokens []string
	APIURL    string // default https://slack.com/api
	HTTP      *http.Client
	// Listen holds the channel ids (C0123ABC, or TEAM/C0123ABC) where every
	// message is for the bot, with no mention needed. Set before Run.
	Listen []string

	mu      sync.RWMutex
	teams   map[string]*team // team id -> workspace
	threads map[string]bool  // "team/channel/thread" threads the bot replied in
	names   map[string]string
	reacted map[string]string // "team/channel/ts" -> the bot's reaction there
}

type team struct {
	id, name, token, botUser string
}

func New(appToken string, botTokens []string, apiURL string) *Adapter {
	if apiURL == "" {
		apiURL = "https://slack.com/api"
	}
	return &Adapter{AppToken: appToken, BotTokens: botTokens, APIURL: strings.TrimRight(apiURL, "/"),
		HTTP: &http.Client{Timeout: 30 * time.Second}, teams: map[string]*team{},
		threads: map[string]bool{}, names: map[string]string{}}
}

func (a *Adapter) MaxLen() int               { return 3900 }
func (a *Adapter) Format(md string) string   { return channel.FormatSlack(md) }
func (a *Adapter) StaleAfter() time.Duration { return 4 * pingEvery }

// api calls a Web API method with a JSON body.
func (a *Adapter) api(ctx context.Context, token, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return a.post(ctx, token, method, "application/json; charset=utf-8", bytes.NewReader(body), out)
}

// post calls a Web API method and classifies failures.
func (a *Adapter) post(ctx context.Context, token, method, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.APIURL+"/"+method, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("slack %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		secs, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return channel.RetryAfter{Err: fmt.Errorf("slack %s: rate limited", method), After: time.Duration(max(secs, 1)) * time.Second}
	}
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("slack %s: HTTP %d, unreadable body", method, resp.StatusCode)
	}
	if !r.OK {
		e := fmt.Errorf("slack %s: %s", method, r.Error)
		switch r.Error {
		case "invalid_auth", "not_authed", "token_revoked", "account_inactive", "token_expired":
			return channel.Fatal{Err: e}
		case "channel_not_found", "not_in_channel", "is_archived", "msg_too_long", "no_text", "cannot_dm_bot":
			return channel.Undeliverable{Err: e}
		case "ratelimited":
			return channel.RetryAfter{Err: e, After: time.Second}
		}
		return e
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	if err := a.loadTeams(ctx); err != nil {
		return err
	}
	var open struct {
		URL string `json:"url"`
	}
	if err := a.api(ctx, a.AppToken, "apps.connections.open", struct{}{}, &open); err != nil {
		return err
	}
	conn, _, err := websocket.Dial(ctx, open.URL, nil)
	if err != nil {
		return fmt.Errorf("slack socket: %w", err)
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
					cancel() // a dead socket: stop reading and reconnect
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
				return errors.New("slack socket: ping failed or shutting down")
			}
			return fmt.Errorf("slack socket: %w", err)
		}
		in.Beat()
		var env struct {
			Type       string          `json:"type"`
			EnvelopeID string          `json:"envelope_id"`
			Payload    json.RawMessage `json:"payload"`
			Reason     string          `json:"reason"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.EnvelopeID != "" {
			ack, _ := json.Marshal(map[string]string{"envelope_id": env.EnvelopeID})
			if err := conn.Write(ctx, websocket.MessageText, ack); err != nil {
				return fmt.Errorf("slack ack: %w", err)
			}
		}
		switch env.Type {
		case "hello":
			in.Connected(a.describe())
		case "disconnect":
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return fmt.Errorf("slack asked to reconnect (%s)", env.Reason)
		case "events_api":
			if m, ok := a.convert(ctx, env.Payload); ok {
				in.Deliver(ctx, m)
			}
		case "interactive":
			if m, ok := a.press(ctx, env.Payload); ok {
				in.Deliver(ctx, m)
			}
		}
	}
}

// loadTeams learns which workspace each bot token belongs to.
func (a *Adapter) loadTeams(ctx context.Context) error {
	teams := map[string]*team{}
	for _, tok := range a.BotTokens {
		var r struct {
			TeamID string `json:"team_id"`
			Team   string `json:"team"`
			UserID string `json:"user_id"`
		}
		if err := a.api(ctx, tok, "auth.test", struct{}{}, &r); err != nil {
			return err
		}
		teams[r.TeamID] = &team{id: r.TeamID, name: r.Team, token: tok, botUser: r.UserID}
	}
	a.mu.Lock()
	a.teams = teams
	a.mu.Unlock()
	return nil
}

func (a *Adapter) describe() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	names := make([]string, 0, len(a.teams))
	for _, t := range a.teams {
		names = append(names, t.name)
	}
	return strings.Join(names, ", ")
}

func (a *Adapter) team(id string) *team {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.teams[id]
}

type event struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	Channel     string `json:"channel"`
	ChannelType string `json:"channel_type"`
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Text        string `json:"text"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
	Files       []struct {
		Name     string `json:"name"`
		Mimetype string `json:"mimetype"`
		Size     int64  `json:"size"`
		URL      string `json:"url_private_download"`
	} `json:"files"`
}

func (a *Adapter) convert(ctx context.Context, payload json.RawMessage) (channel.Message, bool) {
	var p struct {
		TeamID string `json:"team_id"`
		Event  event  `json:"event"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return channel.Message{}, false
	}
	e := p.Event
	t := a.team(p.TeamID)
	if t == nil || e.BotID != "" || e.User == "" || e.User == t.botUser {
		return channel.Message{}, false
	}
	if e.Subtype != "" && e.Subtype != "file_share" && e.Subtype != "thread_broadcast" {
		return channel.Message{}, false // edits, deletes, joins
	}
	m := channel.Message{
		Conversation: t.id + "/" + e.Channel,
		ID:           e.TS,
		Sender:       e.User,
		Text:         cleanText(e.Text, t.botUser),
	}
	for _, f := range e.Files {
		url, token := f.URL, t.token
		m.Attachments = append(m.Attachments, channel.Attachment{Name: f.Name, MediaType: f.Mimetype, Size: f.Size,
			Fetch: func(ctx context.Context) ([]byte, error) { return a.download(ctx, token, url) }})
	}
	switch {
	case e.Type == "message" && e.ChannelType == "im":
		// A direct chat is one conversation: answer at the top level, even
		// a message the user wrote inside a thread.
		m.Direct = true
	case e.Type == "message" && a.listening(t.id, e.Channel):
		if strings.Contains(e.Text, "<@"+t.botUser+">") {
			return channel.Message{}, false // the app_mention event carries it
		}
		// A listen channel: every message is for the bot. A top-level one
		// starts a thread under it, as a mention does.
		m.Mentioned = true
		m.Thread = e.ThreadTS
		if m.Thread == "" {
			m.Thread = e.TS
		}
	case e.Type == "app_mention":
		m.Mentioned = true
		m.Thread = e.ThreadTS
		if m.Thread == "" {
			m.Thread = e.TS // answer in a thread under the mention
		}
	case e.Type == "message" && e.ThreadTS != "":
		// A follow-up in a thread the bot is already part of.
		m.Thread = e.ThreadTS
		a.mu.RLock()
		m.Mentioned = a.threads[m.Conversation+"/"+m.Thread]
		a.mu.RUnlock()
		if strings.Contains(e.Text, "<@"+t.botUser+">") {
			return channel.Message{}, false // the app_mention event carries it
		}
	case e.Type == "message":
		if strings.Contains(e.Text, "<@"+t.botUser+">") {
			return channel.Message{}, false
		}
	default:
		return channel.Message{}, false
	}
	if !m.Direct {
		m.SenderName = a.userName(ctx, t, e.User)
	}
	return m, true
}

// listening reports whether a channel is one where every message is for
// the bot (Listen).
func (a *Adapter) listening(teamID, ch string) bool {
	for _, id := range a.Listen {
		if id == ch || id == teamID+"/"+ch {
			return true
		}
	}
	return false
}

var (
	slackMention = regexp.MustCompile(`<@([A-Z0-9]+)(?:\|[^>]*)?>`)
	slackLink    = regexp.MustCompile(`<((?:https?|mailto):[^|>]+)(?:\|([^>]+))?>`)
	slackChannel = regexp.MustCompile(`<#[A-Z0-9]+\|([^>]*)>`)
)

func cleanText(s, botUser string) string {
	s = strings.ReplaceAll(s, "<@"+botUser+">", "")
	s = slackMention.ReplaceAllString(s, "@$1")
	s = slackChannel.ReplaceAllString(s, "#$1")
	s = slackLink.ReplaceAllStringFunc(s, func(x string) string {
		m := slackLink.FindStringSubmatch(x)
		if m[2] == "" || m[2] == m[1] {
			return m[1]
		}
		return m[2] + " (" + m[1] + ")"
	})
	s = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
	return strings.TrimSpace(s)
}

// download fetches a private file with the workspace's bot token.
func (a *Adapter) download(ctx context.Context, token, fileURL string) ([]byte, error) {
	if fileURL == "" {
		return nil, errors.New("slack: file has no download URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slack download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("slack download: HTTP %d", resp.StatusCode)
	}
	// Without the files:read scope Slack answers with its sign-in page.
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		return nil, errors.New("slack download returned a web page: the app needs the files:read scope")
	}
	return io.ReadAll(io.LimitReader(resp.Body, 100<<20))
}

// userName looks up a display name once per user; failures fall back to the id.
func (a *Adapter) userName(ctx context.Context, t *team, user string) string {
	key := t.id + "/" + user
	a.mu.RLock()
	name, ok := a.names[key]
	a.mu.RUnlock()
	if ok {
		return name
	}
	var r struct {
		User struct {
			RealName string `json:"real_name"`
			Profile  struct {
				DisplayName string `json:"display_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	name = user
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if a.apiForm(lctx, t.token, "users.info", url.Values{"user": {user}}, &r) == nil {
		if r.User.Profile.DisplayName != "" {
			name = r.User.Profile.DisplayName
		} else if r.User.RealName != "" {
			name = r.User.RealName
		}
	}
	a.mu.Lock()
	a.names[key] = name
	a.mu.Unlock()
	return name
}

// apiForm calls a read method that only accepts form or query arguments.
func (a *Adapter) apiForm(ctx context.Context, token, method string, args url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.APIURL+"/"+method, strings.NewReader(args.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &r); err != nil || !r.OK {
		return fmt.Errorf("slack %s: %s", method, r.Error)
	}
	return json.Unmarshal(raw, out)
}

// target finds the workspace and channel id of a conversation.
func (a *Adapter) target(ctx context.Context, to channel.Address) (*team, string, error) {
	teamID, ch, ok := strings.Cut(to.Conversation, "/")
	if !ok {
		return nil, "", channel.Undeliverable{Err: fmt.Errorf("slack: bad conversation %q", to.Conversation)}
	}
	t := a.team(teamID)
	if t == nil {
		if err := a.loadTeams(ctx); err != nil {
			return nil, "", err
		}
		if t = a.team(teamID); t == nil {
			return nil, "", channel.Undeliverable{Err: fmt.Errorf("slack: no bot token for workspace %s", teamID)}
		}
	}
	return t, ch, nil
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

func (a *Adapter) replied(to channel.Address) {
	if to.Thread != "" {
		a.mu.Lock()
		a.threads[to.Conversation+"/"+to.Thread] = true
		a.mu.Unlock()
	}
}

func (a *Adapter) Send(ctx context.Context, to channel.Address, text, _ string) error {
	t, ch, err := a.target(ctx, to)
	if err != nil {
		return err
	}
	params := map[string]any{"channel": ch, "text": text, "mrkdwn": true, "unfurl_links": false}
	if to.Thread != "" {
		params["thread_ts"] = to.Thread
	}
	if err := a.api(ctx, t.token, "chat.postMessage", params, nil); err != nil {
		return sendErr(err)
	}
	a.replied(to)
	return nil
}

// MaxFileSize is Habakkuk's cap; Slack itself takes up to 1 GB.
func (a *Adapter) MaxFileSize() int64 { return 100 << 20 }

// SendFile uploads in Slack's three steps: get an upload URL, post the
// bytes to it, then complete the upload into the channel (and thread).
// It needs the files:write scope.
func (a *Adapter) SendFile(ctx context.Context, to channel.Address, name, _ string, data []byte, caption string) error {
	t, ch, err := a.target(ctx, to)
	if err != nil {
		return err
	}
	var up struct {
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
	}
	args := url.Values{"filename": {name}, "length": {strconv.Itoa(len(data))}}
	if err := a.post(ctx, t.token, "files.getUploadURLExternal", "application/x-www-form-urlencoded",
		strings.NewReader(args.Encode()), &up); err != nil {
		return fileErr(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, up.UploadURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	client := &http.Client{Transport: a.HTTP.Transport, Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("slack upload: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack upload: HTTP %d", resp.StatusCode)
	}
	params := map[string]any{"files": []map[string]string{{"id": up.FileID, "title": name}}, "channel_id": ch}
	if to.Thread != "" {
		params["thread_ts"] = to.Thread
	}
	if caption != "" {
		params["initial_comment"] = caption
	}
	if err := a.api(ctx, t.token, "files.completeUploadExternal", params, nil); err != nil {
		return fileErr(err)
	}
	a.replied(to)
	return nil
}

func fileErr(err error) error {
	if strings.Contains(err.Error(), "missing_scope") {
		return channel.Undeliverable{Err: fmt.Errorf("%w (the Slack app needs the files:write scope)", err)}
	}
	return sendErr(err)
}
