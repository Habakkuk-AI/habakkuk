// Package whatsappcloud is the adapter for the official WhatsApp Business
// Cloud API. Meta posts webhooks to Habakkuk's HTTP server, signed with the
// app secret; replies go out through the Graph API from a business phone
// number. WhatsApp lets a business write freely only within 24 hours of the
// customer's last message; outside that window only an approved template
// may be sent, so the adapter remembers when each customer last wrote.
package whatsappcloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/metagraph"
)

const (
	maxBody     = 4 << 20
	maxDownload = 100 << 20
	maxImage    = 5 << 20
	maxVideo    = 16 << 20
	maxDocument = 100 << 20
	// Window is how long after a customer's message a business may send
	// free-form messages. A minute is kept back for clock skew and transit.
	Window      = 24*time.Hour - time.Minute
	typingEvery = 20 * time.Second // the indicator lasts 25 seconds or until the reply
	maxParam    = 1000             // a template body is at most 1024 characters
)

// Graph error codes this adapter treats specially.
const (
	codeReengagement = 131047 // more than 24 hours since the customer wrote
)

// Config is the adapter's settings; secrets are values, already resolved.
type Config struct {
	PhoneNumberID string // the business number's id, not the number
	Token         string // system user access token
	AppSecret     string // signs webhooks
	VerifyToken   string // hub.verify_token chosen when subscribing the webhook
	APIURL        string // default metagraph.DefaultBase
	// Template is an approved template sent when the window is closed.
	// With TemplateText its body has one variable, {{1}}, that carries the
	// reply; without, it is sent once as an invitation to write back.
	Template         string
	TemplateLanguage string // default en_US
	TemplateText     bool
	StatePath        string // $HABAKKUK_HOME/channels/whatsappcloud-<account>.json
}

type Adapter struct {
	cfg      Config
	graph    *metagraph.Client
	Now      func() time.Time
	listener channel.Listener

	mu     sync.Mutex
	loaded bool
	state  state
	typed  map[string]time.Time // message id -> last typing indicator
}

// state is what the platform cannot tell the adapter: when each customer
// last wrote, and when a template invitation went out since.
type state struct {
	Seen    map[string]int64 `json:"seen"`    // wa_id -> unix seconds of their last message
	Invited map[string]int64 `json:"invited"` // wa_id -> unix seconds the invitation template went out
}

func New(cfg Config) *Adapter {
	if cfg.APIURL == "" {
		cfg.APIURL = metagraph.DefaultBase
	}
	if cfg.TemplateLanguage == "" {
		cfg.TemplateLanguage = "en_US"
	}
	return &Adapter{cfg: cfg, graph: &metagraph.Client{Name: "whatsappcloud", Base: strings.TrimRight(cfg.APIURL, "/"),
		Token: cfg.Token, HTTP: &http.Client{Timeout: time.Minute}}}
}

func (a *Adapter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Adapter) MaxLen() int               { return 4096 }
func (a *Adapter) Format(md string) string   { return channel.FormatWhatsApp(md) }
func (a *Adapter) StaleAfter() time.Duration { return 3 * time.Minute }
func (a *Adapter) MaxFileSize() int64        { return maxDocument }

// Run checks the token against the phone number, then takes webhooks.
func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	switch {
	case a.cfg.Token == "":
		return channel.Fatal{Err: errors.New("whatsappcloud: the access token variable is empty")}
	case a.cfg.AppSecret == "":
		return channel.Fatal{Err: errors.New("whatsappcloud: the app secret variable is empty")}
	case a.cfg.VerifyToken == "":
		return channel.Fatal{Err: errors.New("whatsappcloud: the verify token variable is empty")}
	}
	if err := a.load(); err != nil {
		return err
	}
	var info struct {
		Display  string `json:"display_phone_number"`
		Verified string `json:"verified_name"`
	}
	if err := a.graph.JSON(ctx, http.MethodGet, "/"+url.PathEscape(a.cfg.PhoneNumberID)+"?fields=display_phone_number,verified_name", nil, &info); err != nil {
		return err
	}
	return a.listener.Run(ctx, in, strings.TrimSpace("WhatsApp "+info.Verified+" "+info.Display), time.Minute, nil)
}

func (a *Adapter) load() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loaded {
		return nil
	}
	if a.cfg.StatePath != "" {
		if err := channel.LoadState(a.cfg.StatePath, &a.state); err != nil {
			return fmt.Errorf("whatsappcloud state: %w", err)
		}
	}
	a.loaded = true
	return nil
}

// saveLocked writes the state, dropping customers silent for 30 days.
func (a *Adapter) saveLocked() {
	cutoff := a.now().Add(-30 * 24 * time.Hour).Unix()
	for k, t := range a.state.Seen {
		if t < cutoff {
			delete(a.state.Seen, k)
		}
	}
	for k, t := range a.state.Invited {
		if t < cutoff {
			delete(a.state.Invited, k)
		}
	}
	if a.cfg.StatePath != "" {
		_ = channel.SaveState(a.cfg.StatePath, a.state)
	}
}

// seen records a customer's message, which opens the 24-hour window.
func (a *Adapter) seen(waID string, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state.Seen == nil {
		a.state.Seen = map[string]int64{}
	}
	if at.Unix() <= a.state.Seen[waID] {
		return
	}
	a.state.Seen[waID] = at.Unix()
	delete(a.state.Invited, waID)
	a.saveLocked()
}

// closeWindow forgets a customer's last message after WhatsApp said the
// window is closed (a clock or state disagreement).
func (a *Adapter) closeWindow(waID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.state.Seen[waID]; ok {
		delete(a.state.Seen, waID)
		a.saveLocked()
	}
}

// InWindow reports whether free-form messages to waID are allowed now.
func (a *Adapter) InWindow(waID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.state.Seen[waID]
	return ok && a.now().Sub(time.Unix(t, 0)) < Window
}

// ServeHTTP answers the subscription handshake (GET /) and takes webhooks
// (POST /).
func (a *Adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		metagraph.Challenge(w, r, a.cfg.VerifyToken)
		return
	case http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, ok := channel.ReadBody(w, r, maxBody)
	if !ok {
		return
	}
	if !metagraph.ValidSignature(a.cfg.AppSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	var hook webhook
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
	for _, e := range hook.Entry {
		for _, ch := range e.Changes {
			v := &ch.Value
			if ch.Field != "messages" || v.Metadata.PhoneNumberID != a.cfg.PhoneNumberID {
				continue
			}
			for _, st := range v.Statuses {
				for _, se := range st.Errors {
					if st.Status == "failed" && se.Code == codeReengagement {
						a.closeWindow(st.RecipientID)
					}
				}
			}
			names := map[string]string{}
			for _, c := range v.Contacts {
				names[c.WaID] = c.Profile.Name
			}
			for i := range v.Messages {
				if m, ok := a.convert(&v.Messages[i]); ok {
					m.SenderName = names[m.Sender]
					in.Deliver(ctx, m)
				}
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

type webhook struct {
	Object string `json:"object"`
	Entry  []struct {
		ID      string `json:"id"`
		Changes []struct {
			Field string `json:"field"`
			Value value  `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

type value struct {
	Metadata struct {
		PhoneNumberID string `json:"phone_number_id"`
	} `json:"metadata"`
	Contacts []struct {
		Profile struct {
			Name string `json:"name"`
		} `json:"profile"`
		WaID string `json:"wa_id"`
	} `json:"contacts"`
	Messages []message `json:"messages"`
	Statuses []struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		RecipientID string `json:"recipient_id"`
		Errors      []struct {
			Code int `json:"code"`
		} `json:"errors"`
	} `json:"statuses"`
}

type mediaRef struct {
	ID       string `json:"id"`
	MimeType string `json:"mime_type"`
	Caption  string `json:"caption"`
	Filename string `json:"filename"`
	Voice    bool   `json:"voice"`
}

type message struct {
	From      string `json:"from"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Text      *struct {
		Body string `json:"body"`
	} `json:"text"`
	Image       *mediaRef `json:"image"`
	Audio       *mediaRef `json:"audio"`
	Video       *mediaRef `json:"video"`
	Document    *mediaRef `json:"document"`
	Sticker     *mediaRef `json:"sticker"`
	Interactive *struct {
		Type        string `json:"type"`
		ButtonReply *reply `json:"button_reply"`
		ListReply   *reply `json:"list_reply"`
	} `json:"interactive"`
	Button *struct {
		Payload string `json:"payload"`
		Text    string `json:"text"`
	} `json:"button"`
	// Context is set on a reply (id: the message it quotes) and on a
	// forward.
	Context *struct {
		ID                  string `json:"id"`
		Forwarded           bool   `json:"forwarded"`
		FrequentlyForwarded bool   `json:"frequently_forwarded"`
	} `json:"context"`
}

type reply struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func (a *Adapter) convert(msg *message) (channel.Message, bool) {
	if msg.From == "" || msg.ID == "" {
		return channel.Message{}, false
	}
	at := a.now()
	if sec, err := parseUnix(msg.Timestamp); err == nil && sec.Before(at) {
		at = sec
	}
	a.seen(msg.From, at) // every customer message opens the window, even ones Habakkuk skips
	m := channel.Message{ID: msg.ID, Sender: msg.From, Conversation: msg.From, Direct: true}
	var ref *mediaRef
	name := ""
	switch msg.Type {
	case "text":
		if msg.Text != nil {
			m.Text = msg.Text.Body
		}
		// Plain text the sender typed: the Cloud API marks forwards and
		// replies in context. A template button's text is not typed.
		m.Forwarded = msg.Context != nil && (msg.Context.Forwarded || msg.Context.FrequentlyForwarded)
		m.Plain = msg.Context == nil
	case "interactive":
		if it := msg.Interactive; it != nil {
			switch {
			case it.ButtonReply != nil:
				m.Callback = it.ButtonReply.ID
			case it.ListReply != nil:
				m.Callback = it.ListReply.ID
			}
		}
		return m, m.Callback != ""
	case "button": // a quick-reply button on a template
		if msg.Button != nil {
			m.Text = msg.Button.Text
		}
	case "image":
		ref, name = msg.Image, "image.jpg"
	case "audio":
		ref, name = msg.Audio, "audio.ogg"
		if ref != nil && ref.Voice {
			name = "voice.ogg"
		}
	case "video":
		ref, name = msg.Video, "video.mp4"
	case "document":
		ref, name = msg.Document, "document"
	case "sticker":
		ref, name = msg.Sticker, "sticker.webp"
	default:
		return channel.Message{}, false // locations, contacts, reactions
	}
	if ref != nil && ref.ID != "" {
		if ref.Filename != "" {
			name = ref.Filename
		}
		m.Text = ref.Caption
		id := ref.ID
		m.Attachments = []channel.Attachment{{Name: name, MediaType: ref.MimeType,
			Fetch: func(ctx context.Context) ([]byte, error) { return a.download(ctx, id) }}}
	}
	m.Text = strings.TrimSpace(m.Text)
	return m, m.Text != "" || len(m.Attachments) > 0
}

// download resolves a media id to its short-lived URL, then fetches it with
// the token.
func (a *Adapter) download(ctx context.Context, id string) ([]byte, error) {
	var meta struct {
		URL string `json:"url"`
	}
	if err := a.graph.JSON(ctx, http.MethodGet, "/"+url.PathEscape(id), nil, &meta); err != nil {
		return nil, err
	}
	if meta.URL == "" {
		return nil, errors.New("whatsappcloud: media has no URL")
	}
	return a.graph.Download(ctx, meta.URL, true, maxDownload)
}

func (a *Adapter) messagesPath() string {
	return "/" + url.PathEscape(a.cfg.PhoneNumberID) + "/messages"
}

// post sends one message object to a customer, honouring the window.
// text is what the message says, used for the template outside it.
func (a *Adapter) post(ctx context.Context, to string, kind string, payload any, replyTo, text string) error {
	if !a.InWindow(to) {
		return a.outsideWindow(ctx, to, text)
	}
	msg := map[string]any{"messaging_product": "whatsapp", "recipient_type": "individual", "to": to, "type": kind, kind: payload}
	if replyTo != "" {
		msg["context"] = map[string]string{"message_id": replyTo}
	}
	err := a.graph.JSON(ctx, http.MethodPost, a.messagesPath(), msg, nil)
	if code, _ := metagraph.Code(err); code == codeReengagement {
		a.closeWindow(to)
		return a.outsideWindow(ctx, to, text)
	}
	return err
}

var spaces = regexp.MustCompile(`[ \t\r\n]+`)

// outsideWindow sends the configured template instead of a free-form
// message, or fails the send with the reason.
func (a *Adapter) outsideWindow(ctx context.Context, to, text string) error {
	if a.cfg.Template == "" {
		return channel.Undeliverable{Err: fmt.Errorf("whatsappcloud: %s last wrote more than 24 hours ago, and outside that window WhatsApp only delivers approved templates; set whatsappcloud.template to send one", to)}
	}
	tmpl := map[string]any{"name": a.cfg.Template, "language": map[string]string{"code": a.cfg.TemplateLanguage}}
	if a.cfg.TemplateText && strings.TrimSpace(text) != "" {
		// Template variables may not hold newlines, tabs or runs of spaces.
		p := strings.TrimSpace(spaces.ReplaceAllString(text, " "))
		if utf8.RuneCountInString(p) > maxParam {
			p = string([]rune(p)[:maxParam-1]) + "…"
		}
		tmpl["components"] = []any{map[string]any{"type": "body", "parameters": []any{map[string]string{"type": "text", "text": p}}}}
		return a.sendTemplate(ctx, to, tmpl)
	}
	a.mu.Lock()
	_, invited := a.state.Invited[to]
	a.mu.Unlock()
	if !invited {
		if err := a.sendTemplate(ctx, to, tmpl); err != nil {
			return err
		}
		a.mu.Lock()
		if a.state.Invited == nil {
			a.state.Invited = map[string]int64{}
		}
		a.state.Invited[to] = a.now().Unix()
		a.saveLocked()
		a.mu.Unlock()
	}
	return channel.Undeliverable{Err: fmt.Errorf("whatsappcloud: %s last wrote more than 24 hours ago, so WhatsApp only allows a template; sent template %q inviting them to write back, and this message is dropped", to, a.cfg.Template)}
}

func (a *Adapter) sendTemplate(ctx context.Context, to string, tmpl map[string]any) error {
	return a.graph.JSON(ctx, http.MethodPost, a.messagesPath(), map[string]any{
		"messaging_product": "whatsapp", "recipient_type": "individual", "to": to, "type": "template", "template": tmpl}, nil)
}

func (a *Adapter) Send(ctx context.Context, to channel.Address, text, replyTo string) error {
	return a.post(ctx, to.Conversation, "text", map[string]any{"body": text, "preview_url": false}, replyTo, text)
}

// Typing marks the customer's message read and shows the typing
// indicator, which WhatsApp ties to that message. It is refreshed at most
// every 20 seconds, since it lasts 25.
func (a *Adapter) Typing(ctx context.Context, to channel.Address) error {
	id := to.ReplyTo
	if id == "" || !a.InWindow(to.Conversation) {
		return nil
	}
	now := a.now()
	a.mu.Lock()
	if a.typed == nil {
		a.typed = map[string]time.Time{}
	}
	for k, t := range a.typed {
		if now.Sub(t) > time.Minute {
			delete(a.typed, k)
		}
	}
	if t, ok := a.typed[id]; ok && now.Sub(t) < typingEvery {
		a.mu.Unlock()
		return nil
	}
	a.typed[id] = now
	a.mu.Unlock()
	return a.graph.JSON(ctx, http.MethodPost, a.messagesPath(), map[string]any{
		"messaging_product": "whatsapp", "status": "read", "message_id": id,
		"typing_indicator": map[string]string{"type": "text"}}, nil)
}

// kind picks how WhatsApp shows a file: images, video and audio inline
// when their type and size allow it, everything else as a document.
func kind(mediaType string, size int) string {
	switch mediaType {
	case "image/jpeg", "image/png":
		if size <= maxImage {
			return "image"
		}
	case "video/mp4", "video/3gpp":
		if size <= maxVideo {
			return "video"
		}
	case "audio/aac", "audio/amr", "audio/mpeg", "audio/mp4", "audio/ogg":
		if size <= maxVideo {
			return "audio"
		}
	}
	return "document"
}

func (a *Adapter) upload(ctx context.Context, name, mediaType string, data []byte) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := a.graph.Multipart(ctx, "/"+url.PathEscape(a.cfg.PhoneNumberID)+"/media",
		map[string]string{"messaging_product": "whatsapp", "type": mediaType},
		metagraph.Part{Field: "file", Name: name, MediaType: mediaType, Data: data}, &out)
	if err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("whatsappcloud: upload returned no media id")
	}
	return out.ID, nil
}

// SendFile uploads the file and sends it; an audio file's caption follows
// as text, since audio messages have none.
func (a *Adapter) SendFile(ctx context.Context, to channel.Address, name, mediaType string, data []byte, caption string) error {
	if len(data) > maxDocument {
		return channel.Undeliverable{Err: fmt.Errorf("whatsappcloud: %s is over WhatsApp's 100 MB limit", name)}
	}
	if !a.InWindow(to.Conversation) {
		return a.outsideWindow(ctx, to.Conversation, "")
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	k := kind(mediaType, len(data))
	if k == "document" && !documentType(mediaType) {
		mediaType = "application/octet-stream"
	}
	id, err := a.upload(ctx, name, mediaType, data)
	if err != nil {
		return err
	}
	payload := map[string]any{"id": id}
	switch k {
	case "document":
		payload["filename"] = name
		fallthrough
	case "image", "video":
		if caption != "" {
			payload["caption"] = caption
		}
	}
	if err := a.post(ctx, to.Conversation, k, payload, "", caption); err != nil {
		return err
	}
	if k == "audio" && caption != "" {
		return a.Send(ctx, to, caption, "")
	}
	return nil
}

// SendVoice sends Ogg Opus audio, which WhatsApp shows as a voice note.
func (a *Adapter) SendVoice(ctx context.Context, to channel.Address, data []byte, _ int, caption string) error {
	return a.SendFile(ctx, to, "voice.ogg", "audio/ogg", data, caption)
}

// Document types the Cloud API accepts; anything else is sent as
// application/octet-stream, which it also takes.
func documentType(t string) bool {
	switch t {
	case "text/plain", "application/pdf", "application/msword", "application/vnd.ms-excel", "application/vnd.ms-powerpoint",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return true
	}
	return false
}

// SendButtons posts up to three reply buttons, or a list of up to ten
// choices. Larger sets fail, and the gateway sends the prompt as text.
func (a *Adapter) SendButtons(ctx context.Context, to channel.Address, text string, rows [][]channel.Button) error {
	var all []channel.Button
	for _, r := range rows {
		all = append(all, r...)
	}
	if utf8.RuneCountInString(text) > 1024 || len(all) == 0 || len(all) > 10 {
		return errors.New("whatsappcloud: WhatsApp takes 1 to 10 choices under at most 1024 characters")
	}
	body := map[string]string{"text": text}
	var interactive map[string]any
	if len(all) <= 3 {
		var buttons []any
		for _, b := range all {
			buttons = append(buttons, map[string]any{"type": "reply", "reply": map[string]string{"id": b.Data, "title": clip(b.Text, 20)}})
		}
		interactive = map[string]any{"type": "button", "body": body, "action": map[string]any{"buttons": buttons}}
	} else {
		var list []any
		for _, b := range all {
			list = append(list, map[string]string{"id": b.Data, "title": clip(b.Text, 24)})
		}
		interactive = map[string]any{"type": "list", "body": body,
			"action": map[string]any{"button": "Choose", "sections": []any{map[string]any{"rows": list}}}}
	}
	return a.post(ctx, to.Conversation, "interactive", interactive, "", text)
}

func parseUnix(s string) (time.Time, error) {
	sec, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(sec, 0), nil
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
