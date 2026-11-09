// Package wecom is the adapter for a WeCom (WeChat Work) self-built
// application. WeCom posts each message a member sends the app to Habakkuk's
// HTTP server, encrypted and signed (see Crypt); replies go out through the
// application message API with an access token that is cached and
// refreshed. WeCom only accepts API calls from the app's trusted IPs.
package wecom

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/apptoken"
	"github.com/habakkuk-ai/habakkuk/internal/channel/upload"
)

const (
	maxBody     = 1 << 20
	maxDownload = 20 << 20
	maxImage    = 10 << 20
	maxFile     = 20 << 20
	maxBytes    = 2048 // a text or markdown message, in bytes
)

type Config struct {
	CorpID         string
	AgentID        int
	Secret         string // the application's secret
	Token          string // callback Token
	EncodingAESKey string // callback EncodingAESKey (43 characters)
	// Markdown sends replies as markdown messages, which the WeCom app
	// renders but WeChat (for external contacts) does not.
	Markdown bool
	APIURL   string // default https://qyapi.weixin.qq.com
}

type Adapter struct {
	cfg      Config
	crypt    *Crypt
	cryptErr error
	HTTP     *http.Client
	tokens   apptoken.Cache
	listener channel.Listener
}

func New(cfg Config) *Adapter {
	if cfg.APIURL == "" {
		cfg.APIURL = "https://qyapi.weixin.qq.com"
	}
	cfg.APIURL = strings.TrimRight(cfg.APIURL, "/")
	a := &Adapter{cfg: cfg, HTTP: &http.Client{Timeout: time.Minute}}
	a.crypt, a.cryptErr = NewCrypt(cfg.Token, cfg.EncodingAESKey, cfg.CorpID)
	a.tokens.Fetch = a.fetchToken
	return a
}

func (a *Adapter) MaxLen() int { return maxBytes }
func (a *Adapter) Format(md string) string {
	if a.cfg.Markdown {
		return md
	}
	return channel.FormatPlain(md)
}
func (a *Adapter) StaleAfter() time.Duration { return 3 * time.Minute }
func (a *Adapter) MaxFileSize() int64        { return maxFile }

type apiError struct {
	Path    string
	Code    int
	Msg     string
	Invalid string
}

func (e *apiError) Error() string {
	s := fmt.Sprintf("wecom %s: errcode %d %s", e.Path, e.Code, e.Msg)
	if e.Code == 60020 {
		s += " (add this server's public IP to the app's trusted IPs)"
	}
	if e.Invalid != "" {
		s += "; not delivered to " + e.Invalid
	}
	return s
}

// Token errors: fetch a new token and try once more.
var staleToken = map[int]bool{40014: true, 42001: true, 41001: true}

func classify(e *apiError, status int) error {
	switch {
	case e.Code == 45009 || e.Code == 45033 || e.Code == 45011 || status == http.StatusTooManyRequests:
		return channel.RetryAfter{Err: e, After: 5 * time.Second}
	case e.Code == -1 || status >= 500:
		return e // system busy
	}
	return channel.Undeliverable{Err: e}
}

func (a *Adapter) fetchToken(ctx context.Context) (string, time.Duration, error) {
	u := a.cfg.APIURL + "/cgi-bin/gettoken?corpid=" + url.QueryEscape(a.cfg.CorpID) + "&corpsecret=" + url.QueryEscape(a.cfg.Secret)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		// The URL holds the secret: keep it out of the message.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", 0, fmt.Errorf("wecom token: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", 0, fmt.Errorf("wecom token: HTTP %d: %w", resp.StatusCode, err)
	}
	switch {
	case out.ErrCode == -1:
		return "", 0, &apiError{Path: "/cgi-bin/gettoken", Code: -1, Msg: out.ErrMsg}
	case out.ErrCode != 0 || out.AccessToken == "":
		return "", 0, channel.Fatal{Err: &apiError{Path: "/cgi-bin/gettoken", Code: out.ErrCode, Msg: out.ErrMsg}}
	}
	return out.AccessToken, time.Duration(max(out.ExpiresIn, 60)) * time.Second, nil
}

// call posts a body to the API with the access token in the query, as
// WeCom wants, and checks errcode (and invaliduser for sends).
func (a *Adapter) call(ctx context.Context, path, query string, raw []byte, ctype string, out any) error {
	for attempt := 0; ; attempt++ {
		tok, err := a.tokens.Get(ctx)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.APIURL+path+"?access_token="+url.QueryEscape(tok)+query, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", ctype)
		resp, err := a.HTTP.Do(req)
		if err != nil {
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err // keep the token out of logs
			}
			return fmt.Errorf("wecom %s: %w", path, err)
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		var env struct {
			ErrCode     int    `json:"errcode"`
			ErrMsg      string `json:"errmsg"`
			InvalidUser string `json:"invaliduser"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			return fmt.Errorf("wecom %s: HTTP %d: %w", path, resp.StatusCode, err)
		}
		if env.ErrCode == 0 && env.InvalidUser == "" && resp.StatusCode < 300 {
			if out != nil {
				return json.Unmarshal(data, out)
			}
			return nil
		}
		if staleToken[env.ErrCode] && attempt == 0 {
			a.tokens.Reset(tok)
			continue
		}
		return classify(&apiError{Path: path, Code: env.ErrCode, Msg: env.ErrMsg, Invalid: env.InvalidUser}, resp.StatusCode)
	}
}

// Run checks the callback key and the app secret, then takes callbacks.
func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	switch {
	case a.cryptErr != nil:
		return channel.Fatal{Err: a.cryptErr}
	case a.cfg.Secret == "":
		return channel.Fatal{Err: errors.New("wecom: the secret variable is empty")}
	case a.cfg.Token == "":
		return channel.Fatal{Err: errors.New("wecom: the callback token variable is empty")}
	}
	if _, err := a.tokens.Get(ctx); err != nil {
		return err
	}
	return a.listener.Run(ctx, in, fmt.Sprintf("WeCom app %d", a.cfg.AgentID), time.Minute, nil)
}

type envelope struct {
	Encrypt string `xml:"Encrypt"`
}

type message struct {
	ToUserName   string `xml:"ToUserName"`
	FromUserName string `xml:"FromUserName"`
	MsgType      string `xml:"MsgType"`
	Content      string `xml:"Content"`
	MediaID      string `xml:"MediaId"`
	Format       string `xml:"Format"`
	Recognition  string `xml:"Recognition"`
	MsgID        string `xml:"MsgId"`
	AgentID      int    `xml:"AgentID"`
}

// ServeHTTP answers the callback URL verification (GET /) and takes
// messages (POST /).
func (a *Adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" || a.crypt == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	sig, ts, nonce := q.Get("msg_signature"), q.Get("timestamp"), q.Get("nonce")
	if r.Method == http.MethodGet {
		echo := q.Get("echostr")
		if !a.crypt.Verify(sig, ts, nonce, echo) {
			http.Error(w, "bad signature", http.StatusForbidden)
			return
		}
		plain, err := a.crypt.Decrypt(echo)
		if err != nil {
			http.Error(w, "cannot decrypt", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(plain)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, ok := channel.ReadBody(w, r, maxBody)
	if !ok {
		return
	}
	var env envelope
	if err := xml.Unmarshal(body, &env); err != nil || env.Encrypt == "" {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if !a.crypt.Verify(sig, ts, nonce, env.Encrypt) {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	plain, err := a.crypt.Decrypt(env.Encrypt)
	if err != nil {
		http.Error(w, "cannot decrypt", http.StatusForbidden)
		return
	}
	var msg message
	if err := xml.Unmarshal(plain, &msg); err != nil {
		http.Error(w, "bad message", http.StatusBadRequest)
		return
	}
	in := a.listener.Inbox()
	if in == nil {
		http.Error(w, "starting", http.StatusServiceUnavailable)
		return
	}
	if m, ok := a.convert(&msg); ok {
		in.Deliver(context.WithoutCancel(r.Context()), m)
	}
	w.WriteHeader(http.StatusOK) // an empty body: no passive reply
}

func (a *Adapter) convert(msg *message) (channel.Message, bool) {
	if msg.FromUserName == "" || msg.MsgID == "" || (a.cfg.AgentID != 0 && msg.AgentID != a.cfg.AgentID) {
		return channel.Message{}, false
	}
	m := channel.Message{ID: msg.MsgID, Sender: msg.FromUserName, Conversation: msg.FromUserName, Direct: true}
	attach := func(name string) {
		id := msg.MediaID
		if id != "" {
			m.Attachments = []channel.Attachment{{Name: name,
				Fetch: func(ctx context.Context) ([]byte, error) { return a.download(ctx, id) }}}
		}
	}
	switch msg.MsgType {
	case "text":
		m.Text = msg.Content
	case "image":
		attach("image.jpg")
	case "voice":
		if m.Text = msg.Recognition; m.Text == "" {
			attach("voice." + strings.ToLower(cmp.Or(msg.Format, "amr")))
		}
	case "video":
		attach("video.mp4")
	case "file":
		attach("file")
	default:
		return channel.Message{}, false // events, locations, links
	}
	m.Text = strings.TrimSpace(m.Text)
	return m, m.Text != "" || len(m.Attachments) > 0
}

// download fetches a member's media by id; WeCom answers errors as JSON.
func (a *Adapter) download(ctx context.Context, id string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		tok, err := a.tokens.Get(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.APIURL+"/cgi-bin/media/get?access_token="+
			url.QueryEscape(tok)+"&media_id="+url.QueryEscape(id), nil)
		if err != nil {
			return nil, err
		}
		resp, err := a.HTTP.Do(req)
		if err != nil {
			return nil, errors.New("wecom media: request failed")
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") && resp.StatusCode == http.StatusOK {
			return data, nil
		}
		var env struct {
			ErrCode int    `json:"errcode"`
			ErrMsg  string `json:"errmsg"`
		}
		_ = json.Unmarshal(data, &env)
		if staleToken[env.ErrCode] && attempt == 0 {
			a.tokens.Reset(tok)
			continue
		}
		return nil, &apiError{Path: "/cgi-bin/media/get", Code: env.ErrCode, Msg: env.ErrMsg}
	}
}

func (a *Adapter) sendMsg(ctx context.Context, to string, msgType string, payload any) error {
	raw, err := json.Marshal(map[string]any{"touser": to, "msgtype": msgType, "agentid": a.cfg.AgentID, msgType: payload})
	if err != nil {
		return err
	}
	return a.call(ctx, "/cgi-bin/message/send", "", raw, "application/json", nil)
}

// Send posts text (or markdown), split further where it runs past WeCom's
// 2048-byte limit.
func (a *Adapter) Send(ctx context.Context, to channel.Address, text, _ string) error {
	kind := "text"
	if a.cfg.Markdown {
		kind = "markdown"
	}
	for _, part := range splitBytes(text, maxBytes) {
		if err := a.sendMsg(ctx, to.Conversation, kind, map[string]string{"content": part}); err != nil {
			return err
		}
	}
	return nil
}

// splitBytes cuts s into pieces of at most n bytes on rune boundaries,
// preferring a line break in the second half of a piece.
func splitBytes(s string, n int) []string {
	var out []string
	for len(s) > n {
		cut := n
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if i := strings.LastIndexByte(s[:cut], '\n'); i > n/2 {
			cut = i + 1
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	return append(out, s)
}

// SendFile uploads the file as temporary media and sends it: JPEG and PNG
// as images, MP4 as video, everything else as a file. A caption follows.
func (a *Adapter) SendFile(ctx context.Context, to channel.Address, name, mediaType string, data []byte, caption string) error {
	kind := "file"
	switch {
	case (mediaType == "image/jpeg" || mediaType == "image/png") && len(data) <= maxImage:
		kind = "image"
	case mediaType == "video/mp4" && len(data) <= maxImage:
		kind = "video"
	}
	if len(data) > maxFile {
		return channel.Undeliverable{Err: fmt.Errorf("wecom: %s is over the 20 MB limit", name)}
	}
	raw, ctype, err := upload.Form(nil, upload.File{Field: "media", Name: name, MediaType: mediaType, Data: data})
	if err != nil {
		return err
	}
	var out struct {
		MediaID string `json:"media_id"`
	}
	if err := a.call(ctx, "/cgi-bin/media/upload", "&type="+kind, raw, ctype, &out); err != nil {
		return err
	}
	if err := a.sendMsg(ctx, to.Conversation, kind, map[string]string{"media_id": out.MediaID}); err != nil {
		return err
	}
	if caption == "" {
		return nil
	}
	return a.Send(ctx, to, caption, "")
}
