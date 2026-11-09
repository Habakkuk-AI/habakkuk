// Package matrix is the Matrix adapter. It uses the client-server API with an
// access token: a /sync long-poll loop whose position survives restarts,
// sends whose transaction ids come from the delivery job (so a retried
// delivery never posts twice), and authenticated media. It has no end-to-end
// encryption: an encrypted room is told once that it is not supported.
package matrix

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

const (
	pollTimeout = 30 * time.Second // how long the server holds a /sync open
	maxUpload   = 50 << 20         // Synapse's default upload limit
	maxDownload = 100 << 20
)

// The sync filter: room messages and memberships only, nothing about
// presence, typing or account data.
const syncFilter = `{"presence":{"not_types":["*"]},"account_data":{"not_types":["*"]},` +
	`"room":{"ephemeral":{"not_types":["*"]},"account_data":{"not_types":["*"]},"state":{"lazy_load_members":true},` +
	`"timeline":{"limit":%d,"types":["m.room.message","m.room.encrypted","m.room.member"]}}}`

const e2eeNotice = "I can't read encrypted messages: end-to-end encryption is not supported yet. " +
	"Please talk to me in a room without encryption."

type Adapter struct {
	Homeserver string // e.g. https://matrix.example.org
	Token      string
	UserID     string // the bot's user id; learned from the token when empty
	StatePath  string // the sync position and rooms told about encryption; empty keeps them in memory
	// Allowed reports whether a user may invite the bot into a room. Invites
	// from anyone else are left pending. Nil accepts none.
	Allowed func(ctx context.Context, userID string) bool
	HTTP    *http.Client

	mu       sync.Mutex
	st       state
	loaded   bool
	name     string            // the bot's display name
	members  map[string]int    // room -> joined members, to tell DMs apart
	names    map[string]string // room|user -> display name
	threads  map[string]bool   // room|thread roots the bot replied in
	sentByMe []string          // recent event ids the bot sent, to spot replies to it
}

type state struct {
	Since    string   `json:"since"`
	Notified []string `json:"notified,omitempty"` // encrypted rooms already told
}

func New(homeserver, token, userID, statePath string) *Adapter {
	return &Adapter{Homeserver: strings.TrimRight(homeserver, "/"), Token: token, UserID: userID, StatePath: statePath,
		HTTP: &http.Client{Timeout: pollTimeout + 30*time.Second}, members: map[string]int{},
		names: map[string]string{}, threads: map[string]bool{}}
}

// MaxLen keeps an event, which carries the text twice (plain and HTML),
// well under the 64 KB event limit.
func (a *Adapter) MaxLen() int               { return 8000 }
func (a *Adapter) StaleAfter() time.Duration { return 3 * pollTimeout }

// Format leaves Markdown as it is; Send adds the HTML rendering.
func (a *Adapter) Format(md string) string { return md }

// do calls the API and classifies failures.
func (a *Adapter) do(ctx context.Context, method, path string, query url.Values, contentType string, body io.Reader, out any) error {
	u := a.Homeserver + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	client := a.HTTP
	if strings.Contains(path, "/media/") {
		client = &http.Client{Transport: a.HTTP.Transport, Timeout: 5 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("matrix %s: %w", strings.SplitN(path, "?", 2)[0], err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 {
		if b, ok := out.(*[]byte); ok {
			*b, err = io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
			return err
		}
		if out != nil {
			return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
		}
		return nil
	}
	return apiErr(path, resp)
}

func apiErr(path string, resp *http.Response) error {
	var e struct {
		ErrCode      string `json:"errcode"`
		Error        string `json:"error"`
		RetryAfterMS int64  `json:"retry_after_ms"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e)
	err := &mxError{Status: resp.StatusCode, Code: e.ErrCode, Msg: e.Error, Path: path}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || e.ErrCode == "M_LIMIT_EXCEEDED":
		after := time.Duration(e.RetryAfterMS) * time.Millisecond
		if after <= 0 {
			secs, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			after = time.Duration(secs) * time.Second
		}
		return channel.RetryAfter{Err: err, After: max(after, time.Second)}
	case resp.StatusCode == http.StatusUnauthorized || e.ErrCode == "M_UNKNOWN_TOKEN" || e.ErrCode == "M_MISSING_TOKEN":
		return channel.Fatal{Err: err}
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusNotFound,
		resp.StatusCode == http.StatusRequestEntityTooLarge, resp.StatusCode == http.StatusBadRequest:
		// Not in the room, room gone, too large, refused: retrying cannot help.
		return channel.Undeliverable{Err: err}
	}
	return err
}

type mxError struct {
	Status          int
	Code, Msg, Path string
}

func (e *mxError) Error() string {
	return fmt.Sprintf("matrix %s: HTTP %d %s %s", e.Path, e.Status, e.Code, e.Msg)
}

func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	var who struct {
		UserID string `json:"user_id"`
	}
	if err := a.do(ctx, http.MethodGet, "/_matrix/client/v3/account/whoami", nil, "", nil, &who); err != nil {
		return err
	}
	a.mu.Lock()
	want := a.UserID
	a.UserID = who.UserID
	a.mu.Unlock()
	if want != "" && want != who.UserID {
		return channel.Fatal{Err: fmt.Errorf("matrix: the access token belongs to %s, not %s", who.UserID, want)}
	}
	if err := a.load(); err != nil {
		return err
	}
	a.learnName(ctx)
	if a.since() == "" {
		// First start: take the current position without answering history.
		resp, err := a.sync(ctx, "", 0, 1)
		if err != nil {
			return err
		}
		a.invites(ctx, resp)
		if err := a.setSince(resp.NextBatch); err != nil {
			return err
		}
	}
	in.Connected(who.UserID)
	in.Beat()
	for ctx.Err() == nil {
		resp, err := a.sync(ctx, a.since(), pollTimeout, 50)
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
		a.invites(ctx, resp)
		a.rooms(ctx, in, resp)
		// Saved after handling: a crash in between hands the same events
		// over again, and the gateway drops what it has already seen.
		if err := a.setSince(resp.NextBatch); err != nil {
			return err
		}
	}
	return nil
}

type event struct {
	Type     string          `json:"type"`
	EventID  string          `json:"event_id"`
	Sender   string          `json:"sender"`
	StateKey *string         `json:"state_key"`
	Content  json.RawMessage `json:"content"`
	Unsigned struct {
		RedactedBecause json.RawMessage `json:"redacted_because"`
	} `json:"unsigned"`
}

type syncResponse struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]struct {
			Timeline struct {
				Events []event `json:"events"`
			} `json:"timeline"`
			State struct {
				Events []event `json:"events"`
			} `json:"state"`
			Summary struct {
				Joined *int `json:"m.joined_member_count"`
			} `json:"summary"`
		} `json:"join"`
		Invite map[string]struct {
			InviteState struct {
				Events []event `json:"events"`
			} `json:"invite_state"`
		} `json:"invite"`
	} `json:"rooms"`
}

func (a *Adapter) sync(ctx context.Context, since string, timeout time.Duration, limit int) (*syncResponse, error) {
	q := url.Values{"timeout": {strconv.FormatInt(timeout.Milliseconds(), 10)}, "filter": {fmt.Sprintf(syncFilter, limit)}}
	if since != "" {
		q.Set("since", since)
	}
	var resp syncResponse
	if err := a.do(ctx, http.MethodGet, "/_matrix/client/v3/sync", q, "", nil, &resp); err != nil {
		return nil, err
	}
	if resp.NextBatch == "" {
		return nil, errors.New("matrix sync: no next_batch in the response")
	}
	return &resp, nil
}

// invites joins rooms the bot was invited to by allowed users.
func (a *Adapter) invites(ctx context.Context, resp *syncResponse) {
	me := a.me()
	for room, inv := range resp.Rooms.Invite {
		for _, e := range inv.InviteState.Events {
			var c content
			if e.Type != "m.room.member" || e.StateKey == nil || *e.StateKey != me ||
				json.Unmarshal(e.Content, &c) != nil || c.Membership != "invite" {
				continue
			}
			if a.Allowed != nil && a.Allowed(ctx, e.Sender) {
				// A failed join (room gone) leaves the invite; a new one retries.
				_ = a.do(ctx, http.MethodPost, "/_matrix/client/v3/join/"+url.PathEscape(room), nil, "application/json", strings.NewReader("{}"), nil)
			}
		}
	}
}

func (a *Adapter) rooms(ctx context.Context, in channel.Inbox, resp *syncResponse) {
	for room, j := range resp.Rooms.Join {
		me := a.me()
		a.mu.Lock()
		for _, e := range slices.Concat(j.State.Events, j.Timeline.Events) {
			if e.Type == "m.room.member" {
				delete(a.members, room) // recount on the next message
				if e.StateKey != nil {
					delete(a.names, room+"|"+*e.StateKey)
				}
			}
		}
		if n := j.Summary.Joined; n != nil {
			a.members[room] = *n
		}
		a.mu.Unlock()
		for _, e := range j.Timeline.Events {
			if e.Sender == me || len(e.Unsigned.RedactedBecause) > 0 {
				continue
			}
			switch e.Type {
			case "m.room.encrypted":
				a.encrypted(ctx, room)
			case "m.room.message":
				if m, ok := a.convert(ctx, room, e); ok {
					in.Deliver(ctx, m)
				}
			}
		}
	}
}

// encrypted tells a room once that its messages cannot be read.
func (a *Adapter) encrypted(ctx context.Context, room string) {
	a.mu.Lock()
	told := slices.Contains(a.st.Notified, room)
	a.mu.Unlock()
	if told {
		return
	}
	c := map[string]any{"msgtype": "m.notice", "body": e2eeNotice}
	if _, err := a.send(ctx, room, c, txnID("e2ee\x00"+room)); err != nil {
		return // tried again on the next encrypted message
	}
	a.mu.Lock()
	a.st.Notified = append(a.st.Notified, room)
	a.mu.Unlock()
	_ = a.save()
}

type relation struct {
	RelType   string `json:"rel_type,omitempty"`
	EventID   string `json:"event_id,omitempty"`
	InReplyTo *struct {
		EventID string `json:"event_id"`
	} `json:"m.in_reply_to,omitempty"`
}

type content struct {
	MsgType       string          `json:"msgtype"`
	Body          string          `json:"body"`
	FormattedBody string          `json:"formatted_body"`
	Filename      string          `json:"filename"`
	URL           string          `json:"url"`
	File          json.RawMessage `json:"file"` // encrypted media
	Info          struct {
		MimeType string `json:"mimetype"`
		Size     int64  `json:"size"`
	} `json:"info"`
	Mentions *struct {
		UserIDs []string `json:"user_ids"`
	} `json:"m.mentions"`
	RelatesTo  *relation `json:"m.relates_to"`
	Membership string    `json:"membership"`
}

func (a *Adapter) convert(ctx context.Context, room string, e event) (channel.Message, bool) {
	var c content
	if json.Unmarshal(e.Content, &c) != nil {
		return channel.Message{}, false
	}
	if c.RelatesTo != nil && c.RelatesTo.RelType == "m.replace" {
		return channel.Message{}, false // an edit
	}
	m := channel.Message{Conversation: room, ID: e.EventID, Sender: e.Sender}
	text := c.Body
	switch c.MsgType {
	case "m.text", "m.notice", "m.emote":
	case "m.image", "m.file", "m.audio", "m.video":
		name := c.Filename
		if name == "" {
			name, text = c.Body, ""
		} else if text == name {
			text = "" // no caption, just the file name
		}
		if c.URL == "" || len(c.File) > 0 {
			text = strings.TrimSpace(text + "\n(an encrypted file I cannot open)")
		} else {
			mxc := c.URL
			m.Attachments = []channel.Attachment{{Name: name, MediaType: c.Info.MimeType, Size: c.Info.Size,
				Fetch: func(ctx context.Context) ([]byte, error) { return a.download(ctx, mxc) }}}
		}
	default:
		return channel.Message{}, false
	}
	if r := c.RelatesTo; r != nil {
		if r.RelType == "m.thread" {
			m.Thread = r.EventID
		}
		if r.InReplyTo != nil {
			text = stripReplyFallback(text)
			if a.sentEvent(r.InReplyTo.EventID) && r.RelType != "m.thread" {
				m.Mentioned = true // a reply to the bot
			}
		}
	}
	a.mu.Lock()
	me, name := a.UserID, a.name
	if m.Thread != "" && a.threads[room+"|"+m.Thread] {
		m.Mentioned = true // a thread the bot is already talking in
	}
	a.mu.Unlock()
	if c.Mentions != nil && slices.Contains(c.Mentions.UserIDs, me) {
		m.Mentioned = true
	}
	if strings.Contains(c.FormattedBody, "matrix.to/#/"+me) || strings.Contains(c.FormattedBody, "matrix.to/#/"+url.PathEscape(me)) {
		m.Mentioned = true // a mention pill
	}
	if strings.Contains(text, me) {
		m.Mentioned, text = true, strings.ReplaceAll(text, me, "")
	}
	// Clients write a pill's fallback as "Name: text".
	for _, n := range []string{name, localpart(me)} {
		if rest, ok := cutPrefixFold(strings.TrimSpace(text), n); ok {
			m.Mentioned, text = true, rest
			break
		}
	}
	m.Text = strings.TrimSpace(text)
	if m.Text == "" && len(m.Attachments) == 0 {
		return channel.Message{}, false
	}
	m.Direct = a.direct(ctx, room)
	if !m.Direct {
		m.SenderName = a.displayName(ctx, room, e.Sender)
	}
	return m, true
}

// stripReplyFallback drops the quoted "> <@user> text" lines older clients
// put before a reply.
func stripReplyFallback(body string) string {
	if !strings.HasPrefix(body, "> ") {
		return body
	}
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, ">") {
			return strings.Join(lines[i:], "\n")
		}
	}
	return ""
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if prefix == "" || len(s) <= len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	switch s[len(prefix)] {
	case ':', ',':
		return strings.TrimSpace(s[len(prefix)+1:]), true
	}
	return s, false
}

func localpart(userID string) string {
	l, _, _ := strings.Cut(strings.TrimPrefix(userID, "@"), ":")
	return l
}

// direct reports whether a room has only the bot and one other person.
func (a *Adapter) direct(ctx context.Context, room string) bool {
	a.mu.Lock()
	n, ok := a.members[room]
	a.mu.Unlock()
	if !ok {
		var r struct {
			Joined map[string]json.RawMessage `json:"joined"`
		}
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if a.do(lctx, http.MethodGet, "/_matrix/client/v3/rooms/"+url.PathEscape(room)+"/joined_members", nil, "", nil, &r) != nil {
			return false
		}
		n = len(r.Joined)
		a.mu.Lock()
		a.members[room] = n
		a.mu.Unlock()
	}
	return n <= 2
}

// displayName looks up a sender's name in the room once; failures fall back
// to the user id's local part.
func (a *Adapter) displayName(ctx context.Context, room, user string) string {
	key := room + "|" + user
	a.mu.Lock()
	name, ok := a.names[key]
	a.mu.Unlock()
	if ok {
		return name
	}
	var r struct {
		DisplayName string `json:"displayname"`
	}
	name = localpart(user)
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	path := "/_matrix/client/v3/rooms/" + url.PathEscape(room) + "/state/m.room.member/" + url.PathEscape(user)
	if a.do(lctx, http.MethodGet, path, nil, "", nil, &r) == nil && r.DisplayName != "" {
		name = r.DisplayName
	}
	a.mu.Lock()
	a.names[key] = name
	a.mu.Unlock()
	return name
}

func (a *Adapter) learnName(ctx context.Context) {
	var r struct {
		DisplayName string `json:"displayname"`
	}
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if a.do(lctx, http.MethodGet, "/_matrix/client/v3/profile/"+url.PathEscape(a.me())+"/displayname", nil, "", nil, &r) == nil {
		a.mu.Lock()
		a.name = r.DisplayName
		a.mu.Unlock()
	}
}

// download fetches mxc:// media through the authenticated media endpoint,
// falling back to the legacy one on servers that predate it.
func (a *Adapter) download(ctx context.Context, mxc string) ([]byte, error) {
	rest, ok := strings.CutPrefix(mxc, "mxc://")
	server, id, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || server == "" || id == "" {
		return nil, fmt.Errorf("matrix: bad media url %q", mxc)
	}
	tail := url.PathEscape(server) + "/" + url.PathEscape(id)
	var data []byte
	err := a.do(ctx, http.MethodGet, "/_matrix/client/v1/media/download/"+tail, nil, "", nil, &data)
	var mx *mxError
	if errors.As(err, &mx) && (mx.Status == http.StatusNotFound && mx.Code == "M_UNRECOGNIZED" || mx.Status == http.StatusMethodNotAllowed) {
		err = a.do(ctx, http.MethodGet, "/_matrix/media/v3/download/"+tail, nil, "", nil, &data)
	}
	if err == nil && len(data) > maxDownload {
		err = fmt.Errorf("matrix: file larger than %d MB", maxDownload>>20)
	}
	return data, err
}

// txnID is a transaction id from a delivery's stable key: the server answers
// a repeated PUT with the event it already made.
func txnID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "habakkuk-" + hex.EncodeToString(sum[:16])
}

func sendTxn(ctx context.Context) string {
	if k := channel.SendKey(ctx); k != "" {
		return txnID(k)
	}
	b := make([]byte, 16)
	rand.Read(b)
	return "habakkuk-" + hex.EncodeToString(b)
}

func (a *Adapter) send(ctx context.Context, room string, c map[string]any, txn string) (string, error) {
	body, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	var r struct {
		EventID string `json:"event_id"`
	}
	path := "/_matrix/client/v3/rooms/" + url.PathEscape(room) + "/send/m.room.message/" + url.PathEscape(txn)
	if err := a.do(ctx, http.MethodPut, path, nil, "application/json", bytes.NewReader(body), &r); err != nil {
		var f channel.Fatal
		if errors.As(err, &f) {
			return "", channel.Undeliverable{Err: err} // a dead token will not heal by retrying
		}
		return "", err
	}
	a.mu.Lock()
	a.sentByMe = append(a.sentByMe, r.EventID)
	if len(a.sentByMe) > 500 {
		a.sentByMe = a.sentByMe[len(a.sentByMe)-500:]
	}
	a.mu.Unlock()
	return r.EventID, nil
}

func (a *Adapter) sentEvent(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Contains(a.sentByMe, id)
}

// relate adds the thread and reply relations of an address.
func (a *Adapter) relate(c map[string]any, to channel.Address, replyTo string) {
	switch {
	case to.Thread != "":
		r := map[string]any{"rel_type": "m.thread", "event_id": to.Thread, "is_falling_back": replyTo == ""}
		inReply := replyTo
		if inReply == "" {
			inReply = to.Thread
		}
		r["m.in_reply_to"] = map[string]string{"event_id": inReply}
		c["m.relates_to"] = r
		a.mu.Lock()
		a.threads[to.Conversation+"|"+to.Thread] = true
		a.mu.Unlock()
	case replyTo != "":
		c["m.relates_to"] = map[string]any{"m.in_reply_to": map[string]string{"event_id": replyTo}}
	}
	c["m.mentions"] = map[string]any{} // text the agent echoes pings nobody
}

func (a *Adapter) Send(ctx context.Context, to channel.Address, text, replyTo string) error {
	if to.Conversation == "" {
		return channel.Undeliverable{Err: errors.New("matrix: no room")}
	}
	c := map[string]any{"msgtype": "m.text", "body": text, "format": "org.matrix.custom.html", "formatted_body": toHTML(text)}
	a.relate(c, to, replyTo)
	_, err := a.send(ctx, to.Conversation, c, sendTxn(ctx))
	return err
}

// Typing shows the typing indicator for ten seconds.
func (a *Adapter) Typing(ctx context.Context, to channel.Address) error {
	path := "/_matrix/client/v3/rooms/" + url.PathEscape(to.Conversation) + "/typing/" + url.PathEscape(a.me())
	return a.do(ctx, http.MethodPut, path, nil, "application/json", strings.NewReader(`{"typing":true,"timeout":10000}`), nil)
}

func (a *Adapter) MaxFileSize() int64 { return maxUpload }

// SendFile uploads the file, then posts it as an image, video, audio or
// file message with the caption as its body.
func (a *Adapter) SendFile(ctx context.Context, to channel.Address, name, mediaType string, data []byte, caption string) error {
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	var up struct {
		ContentURI string `json:"content_uri"`
	}
	if err := a.do(ctx, http.MethodPost, "/_matrix/media/v3/upload", url.Values{"filename": {name}}, mediaType, bytes.NewReader(data), &up); err != nil {
		var f channel.Fatal
		if errors.As(err, &f) {
			return channel.Undeliverable{Err: err}
		}
		return err
	}
	msgtype := "m.file"
	for _, kind := range []string{"image", "video", "audio"} {
		if strings.HasPrefix(mediaType, kind+"/") {
			msgtype = "m." + kind
		}
	}
	c := map[string]any{"msgtype": msgtype, "body": name, "filename": name, "url": up.ContentURI,
		"info": map[string]any{"mimetype": mediaType, "size": len(data)}}
	if caption != "" {
		c["body"], c["format"], c["formatted_body"] = caption, "org.matrix.custom.html", toHTML(caption)
	}
	a.relate(c, to, "")
	_, err := a.send(ctx, to.Conversation, c, sendTxn(ctx))
	return err
}

// toHTML renders Markdown as the HTML subset Matrix clients show: the
// Telegram rendering, with line breaks outside code blocks.
func toHTML(md string) string {
	s := channel.FormatTelegram(md)
	var b strings.Builder
	for {
		i := strings.Index(s, "<pre>")
		if i < 0 {
			b.WriteString(strings.ReplaceAll(s, "\n", "<br>"))
			return b.String()
		}
		end := len(s)
		if j := strings.Index(s[i:], "</pre>"); j >= 0 {
			end = i + j + len("</pre>")
		}
		b.WriteString(strings.ReplaceAll(s[:i], "\n", "<br>"))
		b.WriteString(s[i:end])
		s = s[end:]
	}
}

func (a *Adapter) load() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loaded || a.StatePath == "" {
		a.loaded = true
		return nil
	}
	data, err := os.ReadFile(a.StatePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &a.st); err != nil {
			return fmt.Errorf("matrix state %s: %w", a.StatePath, err)
		}
	}
	a.loaded = true
	return nil
}

func (a *Adapter) me() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.UserID
}

func (a *Adapter) since() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st.Since
}

func (a *Adapter) setSince(next string) error {
	a.mu.Lock()
	a.st.Since = next
	a.mu.Unlock()
	return a.save()
}

// save writes the state atomically, so a crash leaves the previous position.
func (a *Adapter) save() error {
	if a.StatePath == "" {
		return nil
	}
	a.mu.Lock()
	data, err := json.Marshal(a.st)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.StatePath), 0o700); err != nil {
		return err
	}
	return store.WriteFileAtomic(a.StatePath, data, 0o600)
}
