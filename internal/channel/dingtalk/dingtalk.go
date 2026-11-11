// Package dingtalk is the adapter for a DingTalk enterprise app robot in
// HTTP mode. DingTalk posts each message the robot receives (one-to-one
// chats, and group messages that @mention it) to Habakkuk's HTTP server,
// signed with the app secret; replies go out through the robot message API
// with an app access token that is cached and refreshed.
package dingtalk

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
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/apptoken"
	"github.com/habakkuk-ai/habakkuk/internal/channel/upload"
)

const (
	maxBody     = 1 << 20
	maxDownload = 100 << 20
	maxFile     = 20 << 20
	maxSkew     = time.Hour // DingTalk's own limit on the signed timestamp
)

type Config struct {
	ClientID     string // the app's AppKey (Client ID)
	ClientSecret string // AppSecret: signs callbacks and gets tokens
	RobotCode    string // default ClientID
	APIURL       string // overrides both https://api.dingtalk.com and https://oapi.dingtalk.com (tests)
}

type Adapter struct {
	cfg      Config
	api      string
	oapi     string
	HTTP     *http.Client
	Now      func() time.Time
	tokens   apptoken.Cache
	listener channel.Listener
}

func New(cfg Config) *Adapter {
	if cfg.RobotCode == "" {
		cfg.RobotCode = cfg.ClientID
	}
	a := &Adapter{cfg: cfg, api: "https://api.dingtalk.com", oapi: "https://oapi.dingtalk.com", HTTP: &http.Client{Timeout: time.Minute}}
	if cfg.APIURL != "" {
		a.api, a.oapi = strings.TrimRight(cfg.APIURL, "/"), strings.TrimRight(cfg.APIURL, "/")
	}
	a.tokens.Fetch = a.fetchToken
	return a
}

func (a *Adapter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Adapter) MaxLen() int               { return 3000 }
func (a *Adapter) Format(md string) string   { return md } // robot messages are sent as Markdown
func (a *Adapter) StaleAfter() time.Duration { return 3 * time.Minute }
func (a *Adapter) MaxFileSize() int64        { return maxFile }

// Sign is a callback's "sign" header: base64 HMAC-SHA256 of the timestamp,
// a newline and the app secret, keyed with the app secret.
func Sign(secret, timestamp string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "\n" + secret))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// valid checks a callback's signature and that its timestamp is recent.
func (a *Adapter) valid(timestamp, sign string) bool {
	ms, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || a.cfg.ClientSecret == "" {
		return false
	}
	if d := a.now().Sub(time.UnixMilli(ms)); d > maxSkew || d < -maxSkew {
		return false
	}
	return hmac.Equal([]byte(sign), []byte(Sign(a.cfg.ClientSecret, timestamp)))
}

// apiError is an error from either API generation.
type apiError struct {
	Path   string
	Status int
	Code   string
	Msg    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("dingtalk %s: HTTP %d %s %s", e.Path, e.Status, e.Code, e.Msg)
}

func (a *Adapter) fetchToken(ctx context.Context) (string, time.Duration, error) {
	var out struct {
		AccessToken string `json:"accessToken"`
		ExpireIn    int    `json:"expireIn"`
	}
	raw, _ := json.Marshal(map[string]string{"appKey": a.cfg.ClientID, "appSecret": a.cfg.ClientSecret})
	status, body, err := a.post(ctx, a.api+"/v1.0/oauth2/accessToken", "", raw, "application/json")
	if err != nil {
		return "", 0, fmt.Errorf("dingtalk token: %w", err)
	}
	if status >= 500 {
		return "", 0, fmt.Errorf("dingtalk token: HTTP %d", status)
	}
	if json.Unmarshal(body, &out) != nil || out.AccessToken == "" {
		e := parseError("/v1.0/oauth2/accessToken", status, body)
		return "", 0, channel.Fatal{Err: e} // a wrong key or secret
	}
	return out.AccessToken, time.Duration(max(out.ExpireIn, 60)) * time.Second, nil
}

func (a *Adapter) post(ctx context.Context, url, token string, raw []byte, ctype string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", ctype)
	if token != "" {
		req.Header.Set("x-acs-dingtalk-access-token", token)
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, err
}

func parseError(path string, status int, body []byte) *apiError {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	_ = json.Unmarshal(body, &e)
	out := &apiError{Path: path, Status: status, Code: e.Code, Msg: e.Message}
	if e.ErrCode != 0 {
		out.Code, out.Msg = strconv.Itoa(e.ErrCode), e.ErrMsg
	}
	return out
}

// Token errors of either API: fetch a new token and try once more.
func staleToken(e *apiError) bool {
	return e.Status == http.StatusUnauthorized || e.Code == "InvalidAuthentication" || e.Code == "40014" || e.Code == "42001"
}

func classify(e *apiError) error {
	switch {
	case e.Status == http.StatusTooManyRequests || strings.HasPrefix(e.Code, "Throttling") || e.Code == "90018" || e.Code == "90002":
		return channel.RetryAfter{Err: e, After: 2 * time.Second}
	case e.Status >= 500:
		return e
	}
	return channel.Undeliverable{Err: e}
}

// call posts JSON to the new API (api.dingtalk.com) with the access token.
func (a *Adapter) call(ctx context.Context, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		tok, err := a.tokens.Get(ctx)
		if err != nil {
			return err
		}
		status, data, err := a.post(ctx, a.api+path, tok, raw, "application/json")
		if err != nil {
			return fmt.Errorf("dingtalk %s: %w", path, err)
		}
		if status < 300 {
			if out != nil {
				return json.Unmarshal(data, out)
			}
			return nil
		}
		e := parseError(path, status, data)
		if staleToken(e) && attempt == 0 {
			a.tokens.Reset(tok)
			continue
		}
		return classify(e)
	}
}

// Run checks the app credentials, then takes callbacks until ctx ends.
func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	if a.cfg.ClientSecret == "" {
		return channel.Fatal{Err: errors.New("dingtalk: the client secret variable is empty")}
	}
	if _, err := a.tokens.Get(ctx); err != nil {
		return err
	}
	return a.listener.Run(ctx, in, "DingTalk robot "+a.cfg.RobotCode, time.Minute, nil)
}

type callback struct {
	ConversationID   string `json:"conversationId"`
	ConversationType string `json:"conversationType"` // 1 one-to-one, 2 group
	MsgID            string `json:"msgId"`
	MsgType          string `json:"msgtype"`
	SenderNick       string `json:"senderNick"`
	SenderStaffID    string `json:"senderStaffId"`
	SenderID         string `json:"senderId"`
	RobotCode        string `json:"robotCode"`
	IsInAtList       bool   `json:"isInAtList"`
	Text             struct {
		Content string `json:"content"`
	} `json:"text"`
	Content json.RawMessage `json:"content"`
}

// ServeHTTP takes the robot's message callbacks (POST /).
func (a *Adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	body, ok := channel.ReadBody(w, r, maxBody)
	if !ok {
		return
	}
	if !a.valid(r.Header.Get("timestamp"), r.Header.Get("sign")) {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	var cb callback
	if err := json.Unmarshal(body, &cb); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	in := a.listener.Inbox()
	if in == nil {
		http.Error(w, "starting", http.StatusServiceUnavailable)
		return
	}
	if m, ok := a.convert(&cb); ok {
		in.Deliver(context.WithoutCancel(r.Context()), m)
	}
	w.WriteHeader(http.StatusOK)
}

func (a *Adapter) convert(cb *callback) (channel.Message, bool) {
	sender := cb.SenderStaffID
	if sender == "" {
		sender = cb.SenderID
	}
	if sender == "" || cb.MsgID == "" || (cb.RobotCode != "" && cb.RobotCode != a.cfg.RobotCode) {
		return channel.Message{}, false
	}
	m := channel.Message{ID: cb.MsgID, Sender: sender, SenderName: cb.SenderNick}
	if cb.ConversationType == "2" {
		m.Conversation, m.Mentioned = cb.ConversationID, true // groups only deliver @mentions
	} else {
		if cb.SenderStaffID == "" {
			return channel.Message{}, false // not in the organisation: cannot be answered
		}
		m.Conversation, m.Direct = cb.SenderStaffID, true
	}
	var c struct {
		DownloadCode string `json:"downloadCode"`
		FileName     string `json:"fileName"`
		Recognition  string `json:"recognition"`
		RichText     []struct {
			Text         string `json:"text"`
			Type         string `json:"type"`
			DownloadCode string `json:"downloadCode"`
		} `json:"richText"`
	}
	if len(cb.Content) > 0 && cb.Content[0] == '{' {
		_ = json.Unmarshal(cb.Content, &c)
	}
	attach := func(code, name string) {
		if code != "" {
			m.Attachments = append(m.Attachments, channel.Attachment{Name: name,
				Fetch: func(ctx context.Context) ([]byte, error) { return a.download(ctx, code) }})
		}
	}
	switch cb.MsgType {
	case "text":
		m.Text = cb.Text.Content
	case "richText":
		var b strings.Builder
		for _, part := range c.RichText {
			if part.Type == "picture" {
				attach(part.DownloadCode, fmt.Sprintf("image-%d.jpg", len(m.Attachments)+1))
			} else {
				b.WriteString(part.Text)
			}
		}
		m.Text = b.String()
	case "picture":
		attach(c.DownloadCode, "image.jpg")
	case "audio":
		// DingTalk transcribes voice messages itself; its AMR audio is
		// fetched only when it has no transcript.
		if m.Text = c.Recognition; m.Text == "" {
			attach(c.DownloadCode, "voice.amr")
		}
		m.Spoken = true
	case "video":
		attach(c.DownloadCode, "video.mp4")
	case "file":
		attach(c.DownloadCode, c.FileName)
	default:
		return channel.Message{}, false
	}
	m.Text = strings.TrimSpace(m.Text)
	return m, m.Text != "" || len(m.Attachments) > 0
}

// download swaps a download code for a short-lived URL and fetches it.
func (a *Adapter) download(ctx context.Context, code string) ([]byte, error) {
	var out struct {
		DownloadURL string `json:"downloadUrl"`
	}
	if err := a.call(ctx, "/v1.0/robot/messageFiles/download", map[string]string{"downloadCode": code, "robotCode": a.cfg.RobotCode}, &out); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, out.DownloadURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dingtalk download: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
}

// group reports whether a conversation is a group: open conversation ids
// start with "cid"; one-to-one conversations are the user's staff id.
func group(conversation string) bool { return strings.HasPrefix(conversation, "cid") }

// sendMsg posts one robot message to a group or a person.
func (a *Adapter) sendMsg(ctx context.Context, to channel.Address, key string, param any) error {
	raw, err := json.Marshal(param)
	if err != nil {
		return err
	}
	body := map[string]any{"robotCode": a.cfg.RobotCode, "msgKey": key, "msgParam": string(raw)}
	if group(to.Conversation) {
		body["openConversationId"] = to.Conversation
		return a.call(ctx, "/v1.0/robot/groupMessages/send", body, nil)
	}
	body["userIds"] = []string{to.Conversation}
	return a.call(ctx, "/v1.0/robot/oToMessages/batchSend", body, nil)
}

// title is the notification preview of a Markdown message.
func title(md string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(channel.FormatPlain(md)), "\n")
	if utf8.RuneCountInString(line) > 20 {
		line = string([]rune(line)[:19]) + "…"
	}
	if line == "" {
		line = "Habakkuk"
	}
	return line
}

func (a *Adapter) Send(ctx context.Context, to channel.Address, text, _ string) error {
	return a.sendMsg(ctx, to, "sampleMarkdown", map[string]string{"title": title(text), "text": text})
}

// media uploads a file through the older API, which takes the same token.
func (a *Adapter) media(ctx context.Context, kind, name, mediaType string, data []byte) (string, error) {
	raw, ctype, err := upload.Form(nil, upload.File{Field: "media", Name: name, MediaType: mediaType, Data: data})
	if err != nil {
		return "", err
	}
	for attempt := 0; ; attempt++ {
		tok, err := a.tokens.Get(ctx)
		if err != nil {
			return "", err
		}
		status, body, err := a.post(ctx, a.oapi+"/media/upload?access_token="+url.QueryEscape(tok)+"&type="+kind, "", raw, ctype)
		if err != nil {
			return "", fmt.Errorf("dingtalk upload: %w", err)
		}
		var out struct {
			ErrCode int    `json:"errcode"`
			MediaID string `json:"media_id"`
		}
		_ = json.Unmarshal(body, &out)
		if status < 300 && out.ErrCode == 0 && out.MediaID != "" {
			return out.MediaID, nil
		}
		e := parseError("/media/upload", status, body)
		if staleToken(e) && attempt == 0 {
			a.tokens.Reset(tok)
			continue
		}
		return "", classify(e)
	}
}

// SendFile sends JPEG, PNG, GIF and BMP images inline and other files as
// files; a caption follows as text.
func (a *Adapter) SendFile(ctx context.Context, to channel.Address, name, mediaType string, data []byte, caption string) error {
	if len(data) > maxFile {
		return channel.Undeliverable{Err: fmt.Errorf("dingtalk: %s is over the 20 MB limit", name)}
	}
	var err error
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/bmp":
		var id string
		if id, err = a.media(ctx, "image", name, mediaType, data); err == nil {
			err = a.sendMsg(ctx, to, "sampleImageMsg", map[string]string{"photoURL": id})
		}
	default:
		var id string
		if id, err = a.media(ctx, "file", name, mediaType, data); err == nil {
			ext := strings.TrimPrefix(path.Ext(name), ".")
			if ext == "" {
				ext = "file"
			}
			err = a.sendMsg(ctx, to, "sampleFile", map[string]string{"mediaId": id, "fileName": name, "fileType": ext})
		}
	}
	if err != nil || caption == "" {
		return err
	}
	return a.Send(ctx, to, caption, "")
}
