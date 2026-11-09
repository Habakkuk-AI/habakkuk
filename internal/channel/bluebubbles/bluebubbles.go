// Package bluebubbles is the iMessage adapter, through a BlueBubbles server
// running on a Mac. The server posts new messages to Habakkuk's HTTP server
// (the webhook URL carries a secret); replies, files and typing go through
// its REST API.
package bluebubbles

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

const (
	maxBody     = 1 << 20
	maxDownload = 100 << 20
	maxUpload   = 100 << 20
)

type Adapter struct {
	URL        string // the BlueBubbles server
	Password   string
	Secret     string // ?secret= on the webhook URL
	PrivateAPI bool   // typing indicators and threaded replies
	Mention    string // a group message naming this is for Habakkuk
	HTTP       *http.Client

	listener channel.Listener
}

func New(serverURL, password, secret string, privateAPI bool, mention string) *Adapter {
	if mention == "" {
		mention = "Habakkuk"
	}
	return &Adapter{URL: strings.TrimRight(serverURL, "/"), Password: password, Secret: secret,
		PrivateAPI: privateAPI, Mention: mention, HTTP: &http.Client{Timeout: time.Minute}}
}

func (a *Adapter) MaxLen() int               { return 8000 }
func (a *Adapter) Format(md string) string   { return channel.FormatPlain(md) }
func (a *Adapter) StaleAfter() time.Duration { return 3 * time.Minute }
func (a *Adapter) MaxFileSize() int64        { return maxUpload }

// Run pings the server once a minute while taking webhooks, so a Mac that
// is asleep or offline shows as a disconnected channel.
func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	if a.Secret == "" {
		return channel.Fatal{Err: errors.New("bluebubbles: the webhook secret variable is empty")}
	}
	if err := a.ping(ctx); err != nil {
		return err
	}
	return a.listener.Run(ctx, in, "bluebubbles "+a.URL, time.Minute, a.ping)
}

func (a *Adapter) ping(ctx context.Context) error {
	return a.do(ctx, http.MethodGet, "/api/v1/ping", "", nil, nil)
}

// endpoint builds an API URL; the password rides in the query, as the
// server requires, so these URLs never go into an error message.
func (a *Adapter) endpoint(path string) string {
	return a.URL + path + "?password=" + url.QueryEscape(a.Password)
}

func (a *Adapter) do(ctx context.Context, method, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, a.endpoint(path), body)
	if err != nil {
		return fmt.Errorf("bluebubbles %s: bad request", path)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return fmt.Errorf("bluebubbles %s: %w", path, uerr.Err)
		}
		return fmt.Errorf("bluebubbles %s: request failed", path)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var r struct {
		Status  int             `json:"status"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &r)
	if resp.StatusCode >= 300 {
		e := fmt.Errorf("bluebubbles %s: HTTP %d %s %s", path, resp.StatusCode, r.Message, r.Error.Message)
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return channel.Fatal{Err: e} // wrong password
		case http.StatusNotFound, http.StatusBadRequest:
			return channel.Undeliverable{Err: e}
		}
		return e
	}
	if out != nil && len(r.Data) > 0 {
		return json.Unmarshal(r.Data, out)
	}
	return nil
}

type msg struct {
	GUID   string `json:"guid"`
	Text   string `json:"text"`
	IsFrom bool   `json:"isFromMe"`
	Handle *struct {
		Address string `json:"address"`
	} `json:"handle"`
	Chats []struct {
		GUID        string `json:"guid"`
		DisplayName string `json:"displayName"`
	} `json:"chats"`
	Attachments []struct {
		GUID         string `json:"guid"`
		TransferName string `json:"transferName"`
		MimeType     string `json:"mimeType"`
		TotalBytes   int64  `json:"totalBytes"`
	} `json:"attachments"`
	AssociatedType json.RawMessage `json:"associatedMessageType"` // tapbacks
}

// ServeHTTP takes the server's webhook (POST /?secret=...).
func (a *Adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !sameSecret(r.URL.Query().Get("secret"), a.Secret) {
		http.Error(w, "bad secret", http.StatusForbidden)
		return
	}
	body, ok := channel.ReadBody(w, r, maxBody)
	if !ok {
		return
	}
	var ev struct {
		Type string `json:"type"`
		Data msg    `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad event", http.StatusBadRequest)
		return
	}
	in := a.listener.Inbox()
	if in == nil {
		http.Error(w, "starting", http.StatusServiceUnavailable)
		return
	}
	if ev.Type == "new-message" {
		if m, ok := a.convert(&ev.Data); ok {
			in.Deliver(context.WithoutCancel(r.Context()), m)
		}
	}
	w.WriteHeader(http.StatusOK)
}

func sameSecret(x, y string) bool {
	hx, hy := sha256.Sum256([]byte(x)), sha256.Sum256([]byte(y))
	return y != "" && subtle.ConstantTimeCompare(hx[:], hy[:]) == 1
}

func (a *Adapter) convert(d *msg) (channel.Message, bool) {
	// Habakkuk's own messages come back as isFromMe: never answer them.
	if d.IsFrom || d.Handle == nil || d.GUID == "" || len(d.Chats) == 0 {
		return channel.Message{}, false
	}
	if t := strings.TrimSpace(string(d.AssociatedType)); t != "" && t != "null" && t != "0" && t != `""` {
		return channel.Message{}, false
	}
	chat := d.Chats[0].GUID
	addr := strings.ToLower(d.Handle.Address)
	m := channel.Message{
		Conversation: chat, ID: d.GUID, Sender: strings.TrimPrefix(addr, "+"),
		Text:   strings.TrimSpace(strings.ReplaceAll(d.Text, "￼", "")), // object replacement marks attachments
		Direct: strings.Contains(chat, ";-;"),
	}
	if !m.Direct && a.Mention != "" && strings.Contains(strings.ToLower(m.Text), strings.ToLower(a.Mention)) {
		m.Mentioned = true
	}
	for _, at := range d.Attachments {
		guid := at.GUID
		m.Attachments = append(m.Attachments, channel.Attachment{
			Name: at.TransferName, MediaType: at.MimeType, Size: at.TotalBytes,
			Fetch: func(ctx context.Context) ([]byte, error) { return a.download(ctx, guid) },
		})
	}
	return m, m.Text != "" || len(m.Attachments) > 0
}

func (a *Adapter) download(ctx context.Context, guid string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.endpoint("/api/v1/attachment/"+url.PathEscape(guid)+"/download"), nil)
	if err != nil {
		return nil, errors.New("bluebubbles download: bad request")
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, errors.New("bluebubbles download failed") // the URL holds the password
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bluebubbles download: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
}

func tempGUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "habakkuk-" + hex.EncodeToString(b)
}

func (a *Adapter) method() string {
	if a.PrivateAPI {
		return "private-api"
	}
	return "apple-script"
}

// Send posts text to the chat, as a threaded reply when the Private API is
// on.
func (a *Adapter) Send(ctx context.Context, to channel.Address, text, replyTo string) error {
	body := map[string]any{"chatGuid": to.Conversation, "tempGuid": tempGUID(), "message": text, "method": a.method()}
	if a.PrivateAPI && replyTo != "" {
		body["selectedMessageGuid"], body["partIndex"] = replyTo, 0
	}
	raw, _ := json.Marshal(body)
	return a.do(ctx, http.MethodPost, "/api/v1/message/text", "application/json", bytes.NewReader(raw), nil)
}

// SendFile uploads the file into the chat, then the caption as a message.
func (a *Adapter) SendFile(ctx context.Context, to channel.Address, name, _ string, data []byte, caption string) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range map[string]string{"chatGuid": to.Conversation, "tempGuid": tempGUID(), "name": name, "method": a.method()} {
		if err := w.WriteField(k, v); err != nil {
			return err
		}
	}
	fw, err := w.CreateFormFile("attachment", name)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	if err := a.do(ctx, http.MethodPost, "/api/v1/message/attachment", w.FormDataContentType(), &buf, nil); err != nil {
		return err
	}
	if caption != "" {
		return a.Send(ctx, to, caption, "")
	}
	return nil
}

// typer is the adapter with the Private API on, which has typing.
type typer struct{ *Adapter }

// Typing shows the typing bubble in the chat.
func (t typer) Typing(ctx context.Context, to channel.Address) error {
	return t.do(ctx, http.MethodPost, "/api/v1/chat/"+url.PathEscape(to.Conversation)+"/typing", "", nil, nil)
}

// WithTyping returns the adapter to register: one with a typing indicator
// when the server has the Private API, else the plain adapter.
func (a *Adapter) WithTyping() channel.Endpoint {
	if a.PrivateAPI {
		return typer{a}
	}
	return a
}
