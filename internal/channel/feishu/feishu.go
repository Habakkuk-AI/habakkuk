// Package feishu is the adapter for Feishu and its international edition,
// Lark: a custom app's bot. Feishu posts event subscriptions to Habakkuk's
// HTTP server (encrypted with the app's Encrypt Key and signed when one is
// set, and always carrying the Verification Token); replies go out through
// the IM API with a tenant access token that is cached and refreshed.
package feishu

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/apptoken"
	"github.com/habakkuk-ai/habakkuk/internal/channel/upload"
)

// API bases of the two editions.
const (
	FeishuURL = "https://open.feishu.cn"
	LarkURL   = "https://open.larksuite.com"
)

const (
	maxBody     = 1 << 20
	maxDownload = 100 << 20
	maxImage    = 10 << 20
	maxFile     = 30 << 20
)

type Config struct {
	AppID             string
	AppSecret         string
	VerificationToken string // carried in every event
	EncryptKey        string // optional: events arrive encrypted and signed
	APIURL            string // FeishuURL (default) or LarkURL
	// ReplyInThread answers a group mention in a thread under it, so each
	// mention starts its own conversation.
	ReplyInThread bool
}

type Adapter struct {
	cfg      Config
	HTTP     *http.Client
	tokens   apptoken.Cache
	listener channel.Listener

	mu    sync.Mutex
	botID string // the bot's open_id, for spotting mentions
}

func New(cfg Config) *Adapter {
	if cfg.APIURL == "" {
		cfg.APIURL = FeishuURL
	}
	cfg.APIURL = strings.TrimRight(cfg.APIURL, "/")
	a := &Adapter{cfg: cfg, HTTP: &http.Client{Timeout: time.Minute}}
	a.tokens.Fetch = a.fetchToken
	return a
}

func (a *Adapter) MaxLen() int               { return 4000 }
func (a *Adapter) Format(md string) string   { return Format(md) }
func (a *Adapter) StaleAfter() time.Duration { return 3 * time.Minute }
func (a *Adapter) MaxFileSize() int64        { return maxFile }

var heading = regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.+?)[ \t]*#*$`)

// Format keeps Markdown, which post messages render through their "md"
// element, but turns headings (which it lacks) into bold lines.
func Format(md string) string {
	return heading.ReplaceAllString(md, "**$1**")
}

// apiError is a Feishu error: an HTTP status and the body's code.
type apiError struct {
	Path   string
	Status int
	Code   int
	Msg    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("feishu %s: HTTP %d code %d %s", e.Path, e.Status, e.Code, e.Msg)
}

// Codes of a bad or expired tenant access token: fetch a new one.
var staleToken = map[int]bool{99991661: true, 99991663: true, 99991668: true, 99991671: true}

// Codes that mean slow down.
var rateLimited = map[int]bool{99991400: true, 230020: true, 11232: true}

func classify(e *apiError, h http.Header) error {
	switch {
	case rateLimited[e.Code] || e.Status == http.StatusTooManyRequests:
		after, _ := strconv.Atoi(h.Get("x-ogw-ratelimit-reset"))
		return channel.RetryAfter{Err: e, After: time.Duration(max(after, 1)) * time.Second}
	case e.Status >= 500 || e.Status == 0:
		return e
	case e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden || e.Status == http.StatusBadRequest ||
		e.Status == http.StatusNotFound || e.Code != 0:
		return channel.Undeliverable{Err: e}
	}
	return e
}

func (a *Adapter) fetchToken(ctx context.Context) (string, time.Duration, error) {
	raw, _ := json.Marshal(map[string]string{"app_id": a.cfg.AppID, "app_secret": a.cfg.AppSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.APIURL+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(raw))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("feishu token: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Code   int    `json:"code"`
		Msg    string `json:"msg"`
		Token  string `json:"tenant_access_token"`
		Expire int    `json:"expire"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil && resp.StatusCode < 500 {
		return "", 0, fmt.Errorf("feishu token: HTTP %d: %w", resp.StatusCode, err)
	}
	if resp.StatusCode >= 500 {
		return "", 0, fmt.Errorf("feishu token: HTTP %d", resp.StatusCode)
	}
	if out.Code != 0 || out.Token == "" {
		// A wrong app id or secret: a restart cannot fix it.
		return "", 0, channel.Fatal{Err: fmt.Errorf("feishu token: code %d %s", out.Code, out.Msg)}
	}
	return out.Token, time.Duration(max(out.Expire, 60)) * time.Second, nil
}

// call makes one API request with the tenant token, refreshing it once if
// the platform calls it stale. raw is reread on the retry.
func (a *Adapter) call(ctx context.Context, method, path string, raw []byte, ctype string, out any) error {
	for attempt := 0; ; attempt++ {
		tok, err := a.tokens.Get(ctx)
		if err != nil {
			return err
		}
		var body io.Reader
		if raw != nil {
			body = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, a.cfg.APIURL+path, body)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := a.HTTP.Do(req)
		if err != nil {
			return fmt.Errorf("feishu %s: %w", path, err)
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		var env struct {
			Code int             `json:"code"`
			Msg  string          `json:"msg"`
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(data, &env)
		if resp.StatusCode < 300 && env.Code == 0 {
			if out != nil {
				return json.Unmarshal(data, out)
			}
			return nil
		}
		if staleToken[env.Code] && attempt == 0 {
			a.tokens.Reset(tok)
			continue
		}
		return classify(&apiError{Path: path, Status: resp.StatusCode, Code: env.Code, Msg: env.Msg}, resp.Header)
	}
}

func (a *Adapter) callJSON(ctx context.Context, method, path string, body, out any) error {
	var raw []byte
	ctype := ""
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return err
		}
		ctype = "application/json; charset=utf-8"
	}
	return a.call(ctx, method, path, raw, ctype, out)
}

// Run fetches a token and the bot's identity, then takes events.
func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	if a.cfg.AppSecret == "" {
		return channel.Fatal{Err: errors.New("feishu: the app secret variable is empty")}
	}
	if a.cfg.VerificationToken == "" {
		return channel.Fatal{Err: errors.New("feishu: the verification token variable is empty")}
	}
	var info struct {
		Bot struct {
			AppName string `json:"app_name"`
			OpenID  string `json:"open_id"`
		} `json:"bot"`
	}
	if err := a.callJSON(ctx, http.MethodGet, "/open-apis/bot/v3/info", nil, &info); err != nil {
		if errors.As(err, new(channel.Undeliverable)) {
			return channel.Fatal{Err: err} // the app has no bot capability
		}
		return err
	}
	a.mu.Lock()
	a.botID = info.Bot.OpenID
	a.mu.Unlock()
	edition := "Feishu "
	if strings.Contains(a.cfg.APIURL, "larksuite") {
		edition = "Lark "
	}
	return a.listener.Run(ctx, in, edition+info.Bot.AppName, time.Minute, nil)
}

type envelope struct {
	Encrypt   string `json:"encrypt"`
	Challenge string `json:"challenge"`
	Token     string `json:"token"`
	Type      string `json:"type"`
	Schema    string `json:"schema"`
	Header    struct {
		EventID   string `json:"event_id"`
		EventType string `json:"event_type"`
		Token     string `json:"token"`
		AppID     string `json:"app_id"`
	} `json:"header"`
	Event json.RawMessage `json:"event"`
}

type userID struct {
	OpenID  string `json:"open_id"`
	UserID  string `json:"user_id"`
	UnionID string `json:"union_id"`
}

type receive struct {
	Sender struct {
		SenderID   userID `json:"sender_id"`
		SenderType string `json:"sender_type"`
	} `json:"sender"`
	Message struct {
		MessageID   string `json:"message_id"`
		RootID      string `json:"root_id"`
		ThreadID    string `json:"thread_id"`
		ChatID      string `json:"chat_id"`
		ChatType    string `json:"chat_type"` // p2p | group
		MessageType string `json:"message_type"`
		Content     string `json:"content"`
		Mentions    []struct {
			Key  string `json:"key"`
			ID   userID `json:"id"`
			Name string `json:"name"`
		} `json:"mentions"`
	} `json:"message"`
}

func equal(a, b string) bool { return hmac.Equal([]byte(a), []byte(b)) }

// ServeHTTP takes event subscriptions (POST /), including the URL
// verification challenge.
func (a *Adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	body, ok := channel.ReadBody(w, r, maxBody)
	if !ok {
		return
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	signed := false
	if a.cfg.EncryptKey != "" {
		if sig := r.Header.Get("X-Lark-Signature"); sig != "" {
			want := Signature(r.Header.Get("X-Lark-Request-Timestamp"), r.Header.Get("X-Lark-Request-Nonce"), a.cfg.EncryptKey, body)
			if !equal(strings.ToLower(sig), want) {
				http.Error(w, "bad signature", http.StatusForbidden)
				return
			}
			signed = true
		}
		if env.Encrypt == "" {
			http.Error(w, "events must be encrypted", http.StatusForbidden)
			return
		}
		plain, err := Decrypt(a.cfg.EncryptKey, env.Encrypt)
		if err != nil {
			http.Error(w, "cannot decrypt", http.StatusForbidden)
			return
		}
		env = envelope{}
		if err := json.Unmarshal(plain, &env); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
	}
	if env.Type == "url_verification" {
		if !equal(env.Token, a.cfg.VerificationToken) {
			http.Error(w, "bad token", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": env.Challenge})
		return
	}
	// Events must carry the token, and when an Encrypt Key is set, a
	// signature as well.
	if !equal(env.Header.Token, a.cfg.VerificationToken) || (a.cfg.EncryptKey != "" && !signed) {
		http.Error(w, "bad token", http.StatusForbidden)
		return
	}
	in := a.listener.Inbox()
	if in == nil {
		http.Error(w, "starting", http.StatusServiceUnavailable)
		return
	}
	if env.Header.EventType == "im.message.receive_v1" {
		var ev receive
		if json.Unmarshal(env.Event, &ev) == nil {
			if m, ok := a.convert(&ev); ok {
				in.Deliver(context.WithoutCancel(r.Context()), m)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, "{}")
}

func (a *Adapter) convert(ev *receive) (channel.Message, bool) {
	msg := &ev.Message
	if ev.Sender.SenderType != "user" || ev.Sender.SenderID.OpenID == "" || msg.MessageID == "" {
		return channel.Message{}, false
	}
	m := channel.Message{ID: msg.MessageID, Sender: ev.Sender.SenderID.OpenID, Conversation: msg.ChatID,
		Direct: msg.ChatType == "p2p"}
	if msg.ThreadID != "" && msg.RootID != "" {
		m.Thread = msg.RootID
	} else if a.cfg.ReplyInThread && !m.Direct {
		m.Thread = msg.MessageID
	}
	a.mu.Lock()
	bot := a.botID
	a.mu.Unlock()
	names := map[string]string{} // mention key -> how it reads in the text
	for _, mn := range msg.Mentions {
		if bot != "" && mn.ID.OpenID == bot {
			names[mn.Key] = ""
			m.Mentioned = true
		} else {
			names[mn.Key] = "@" + mn.Name
		}
	}
	unmention := func(s string) string {
		for k, v := range names {
			s = strings.ReplaceAll(s, k, v)
		}
		return s
	}
	var c struct {
		Text     string          `json:"text"`
		Title    string          `json:"title"`
		Content  [][]postElement `json:"content"`
		ImageKey string          `json:"image_key"`
		FileKey  string          `json:"file_key"`
		FileName string          `json:"file_name"`
	}
	if err := json.Unmarshal([]byte(msg.Content), &c); err != nil {
		return channel.Message{}, false
	}
	id := msg.MessageID
	attach := func(key, kind, name string) {
		m.Attachments = append(m.Attachments, channel.Attachment{Name: name,
			Fetch: func(ctx context.Context) ([]byte, error) { return a.resource(ctx, id, key, kind) }})
	}
	switch msg.MessageType {
	case "text":
		m.Text = unmention(c.Text)
	case "post":
		var b strings.Builder
		if c.Title != "" {
			b.WriteString(c.Title + "\n")
		}
		for _, line := range c.Content {
			for _, el := range line {
				switch el.Tag {
				case "text", "a", "md":
					b.WriteString(el.Text)
				case "at":
					b.WriteString(unmention(el.UserID))
				case "code_block":
					b.WriteString("\n```\n" + el.Text + "\n```\n")
				case "img":
					attach(el.ImageKey, "image", fmt.Sprintf("image-%d.png", len(m.Attachments)+1))
				case "media":
					attach(el.FileKey, "file", "video.mp4")
				}
			}
			b.WriteString("\n")
		}
		m.Text = b.String()
	case "image":
		attach(c.ImageKey, "image", "image.png")
	case "file":
		attach(c.FileKey, "file", c.FileName)
	case "audio":
		attach(c.FileKey, "file", "audio.opus")
	case "media":
		name := c.FileName
		if name == "" {
			name = "video.mp4"
		}
		attach(c.FileKey, "file", name)
	default:
		return channel.Message{}, false // stickers, cards, shared chats
	}
	m.Text = strings.TrimSpace(m.Text)
	return m, m.Text != "" || len(m.Attachments) > 0
}

type postElement struct {
	Tag      string `json:"tag"`
	Text     string `json:"text"`
	UserID   string `json:"user_id"`
	ImageKey string `json:"image_key"`
	FileKey  string `json:"file_key"`
}

// resource downloads an image or file attached to a message.
func (a *Adapter) resource(ctx context.Context, messageID, key, kind string) ([]byte, error) {
	tok, err := a.tokens.Get(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.APIURL+"/open-apis/im/v1/messages/"+
		url.PathEscape(messageID)+"/resources/"+url.PathEscape(key)+"?type="+kind, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feishu resource: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
}

// uuid makes a send idempotent for an hour: Feishu drops a second message
// with the same uuid, so a retried delivery job posts once.
func uuid(ctx context.Context, salt string) string {
	k := channel.SendKey(ctx)
	if k == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(k + "\x00" + salt))
	return hex.EncodeToString(sum[:16])
}

// post sends one message: in a thread (replying to its root, or to the
// message being answered), or new in the chat.
func (a *Adapter) post(ctx context.Context, to channel.Address, replyTo, msgType string, content any, salt string) error {
	raw, err := json.Marshal(content)
	if err != nil {
		return err
	}
	body := map[string]any{"msg_type": msgType, "content": string(raw)}
	if u := uuid(ctx, salt); u != "" {
		body["uuid"] = u
	}
	target := replyTo
	if to.Thread != "" && target == "" {
		target = to.Thread
	}
	if to.Thread != "" {
		body["reply_in_thread"] = true
		return a.callJSON(ctx, http.MethodPost, "/open-apis/im/v1/messages/"+url.PathEscape(target)+"/reply", body, nil)
	}
	body["receive_id"] = to.Conversation
	return a.callJSON(ctx, http.MethodPost, "/open-apis/im/v1/messages?receive_id_type=chat_id", body, nil)
}

// Send posts text as a rich-text post whose "md" element renders Markdown.
func (a *Adapter) Send(ctx context.Context, to channel.Address, text, replyTo string) error {
	content := map[string]any{"zh_cn": map[string]any{"content": [][]map[string]string{{{"tag": "md", "text": text}}}}}
	return a.post(ctx, to, replyTo, "post", content, "text")
}

func (a *Adapter) uploadForm(ctx context.Context, path string, fields map[string]string, f upload.File, out any) error {
	raw, ctype, err := upload.Form(fields, f)
	if err != nil {
		return err
	}
	return a.call(ctx, http.MethodPost, path, raw, ctype, out)
}

// fileType is Feishu's file_type for an upload.
func fileType(name, mediaType string) string {
	switch mediaType {
	case "audio/ogg", "audio/opus":
		return "opus"
	case "video/mp4":
		return "mp4"
	case "application/pdf":
		return "pdf"
	}
	switch strings.ToLower(name[strings.LastIndex(name, ".")+1:]) {
	case "doc", "docx":
		return "doc"
	case "xls", "xlsx", "csv":
		return "xls"
	case "ppt", "pptx":
		return "ppt"
	}
	return "stream"
}

// SendFile uploads an image (shown inline) or a file, then posts it; a
// caption follows as text. Ogg Opus audio is sent as a voice message.
func (a *Adapter) SendFile(ctx context.Context, to channel.Address, name, mediaType string, data []byte, caption string) error {
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/bmp", "image/tiff":
		if len(data) <= maxImage {
			var out struct {
				Data struct {
					ImageKey string `json:"image_key"`
				} `json:"data"`
			}
			if err := a.uploadForm(ctx, "/open-apis/im/v1/images", map[string]string{"image_type": "message"},
				upload.File{Field: "image", Name: name, MediaType: mediaType, Data: data}, &out); err != nil {
				return err
			}
			if err := a.post(ctx, to, "", "image", map[string]string{"image_key": out.Data.ImageKey}, "image"); err != nil {
				return err
			}
			return a.caption(ctx, to, caption)
		}
	}
	if len(data) > maxFile {
		return channel.Undeliverable{Err: fmt.Errorf("feishu: %s is over the 30 MB file limit", name)}
	}
	ft := fileType(name, mediaType)
	var out struct {
		Data struct {
			FileKey string `json:"file_key"`
		} `json:"data"`
	}
	if err := a.uploadForm(ctx, "/open-apis/im/v1/files", map[string]string{"file_type": ft, "file_name": name},
		upload.File{Field: "file", Name: name, MediaType: mediaType, Data: data}, &out); err != nil {
		return err
	}
	msgType := "file"
	if ft == "opus" {
		msgType = "audio"
	}
	if err := a.post(ctx, to, "", msgType, map[string]string{"file_key": out.Data.FileKey}, "file"); err != nil {
		return err
	}
	return a.caption(ctx, to, caption)
}

func (a *Adapter) caption(ctx context.Context, to channel.Address, caption string) error {
	if caption == "" {
		return nil
	}
	return a.post(ctx, to, "", "text", map[string]string{"text": caption}, "caption")
}

// SendVoice sends Ogg Opus audio as a voice message.
func (a *Adapter) SendVoice(ctx context.Context, to channel.Address, data []byte, _ int, caption string) error {
	return a.SendFile(ctx, to, "voice.opus", "audio/ogg", data, caption)
}
