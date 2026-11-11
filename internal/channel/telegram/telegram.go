// Package telegram is the Telegram Bot API adapter. It long-polls
// getUpdates, so it needs no public URL.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

const pollTimeout = 50 // seconds the server holds a getUpdates request open

type Adapter struct {
	Token  string
	APIURL string // default https://api.telegram.org
	HTTP   *http.Client

	botID    int64
	username string
}

func New(token, apiURL string) *Adapter {
	if apiURL == "" {
		apiURL = "https://api.telegram.org"
	}
	return &Adapter{Token: token, APIURL: strings.TrimRight(apiURL, "/"),
		HTTP: &http.Client{Timeout: (pollTimeout + 15) * time.Second}}
}

func (a *Adapter) MaxLen() int               { return 4096 }
func (a *Adapter) Format(md string) string   { return channel.FormatTelegram(md) }
func (a *Adapter) StaleAfter() time.Duration { return 3 * pollTimeout * time.Second }
func (a *Adapter) chatID(conv string) (any, error) {
	if id, err := strconv.ParseInt(conv, 10, 64); err == nil {
		return id, nil
	}
	if strings.HasPrefix(conv, "@") {
		return conv, nil
	}
	return nil, channel.Undeliverable{Err: fmt.Errorf("telegram: bad chat id %q", conv)}
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// call posts a Bot API method and classifies failures.
func (a *Adapter) call(ctx context.Context, method string, params any, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return a.post(ctx, a.HTTP, method, "application/json", bytes.NewReader(body), out)
}

func (a *Adapter) post(ctx context.Context, client *http.Client, method, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.APIURL+"/bot"+a.Token+"/"+method, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := client.Do(req)
	if err != nil {
		// The URL holds the token; never let it reach a log line.
		var uerr interface{ Unwrap() error }
		if errors.As(err, &uerr) {
			return fmt.Errorf("telegram %s: %w", method, uerr.Unwrap())
		}
		return fmt.Errorf("telegram %s: request failed", method)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	var r apiResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d, unreadable body", method, resp.StatusCode)
	}
	if !r.OK {
		e := fmt.Errorf("telegram %s: %d %s", method, r.ErrorCode, r.Description)
		switch {
		case r.ErrorCode == 401 || r.ErrorCode == 404:
			return channel.Fatal{Err: e} // bad token
		case r.ErrorCode == 429:
			return channel.RetryAfter{Err: e, After: time.Duration(r.Parameters.RetryAfter) * time.Second}
		case r.ErrorCode == 403 || (r.ErrorCode == 400 && strings.Contains(r.Description, "chat not found")):
			return channel.Undeliverable{Err: e}
		}
		return e
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

type user struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type message struct {
	MessageID       int64 `json:"message_id"`
	MessageThreadID int64 `json:"message_thread_id"`
	IsTopicMessage  bool  `json:"is_topic_message"`
	From            *user `json:"from"`
	Chat            struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Text     string `json:"text"`
	Caption  string `json:"caption"`
	Entities []struct {
		Type   string `json:"type"`
		Offset int    `json:"offset"`
		Length int    `json:"length"`
		User   *user  `json:"user"`
	} `json:"entities"`
	ReplyTo *message `json:"reply_to_message"`
	// Set on a forwarded message (Bot API 7.0 and older versions).
	ForwardOrigin json.RawMessage `json:"forward_origin"`
	ForwardFrom   *user           `json:"forward_from"`
	ForwardDate   int64           `json:"forward_date"`
	// ViaBot: the message was written by an inline bot, not typed.
	ViaBot *user `json:"via_bot"`
	// Quote is the part of the replied-to message the user picked, if any.
	Quote *struct {
		Text string `json:"text"`
	} `json:"quote"`
	Photo     []file `json:"photo"`
	Document  *file  `json:"document"`
	Voice     *file  `json:"voice"`
	Audio     *file  `json:"audio"`
	Video     *file  `json:"video"`
	VideoNote *file  `json:"video_note"`
	Sticker   *file  `json:"sticker"`
}

type file struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

// maxDownload is the Bot API's limit for getFile.
const maxDownload = 20 << 20

type update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *message       `json:"message"`
	CallbackQuery *callbackQuery `json:"callback_query"`
}

func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	var me user
	if err := a.call(ctx, "getMe", struct{}{}, &me); err != nil {
		return err
	}
	a.botID, a.username = me.ID, me.Username
	// A webhook set elsewhere makes getUpdates fail with 409; polling wins.
	if err := a.call(ctx, "deleteWebhook", map[string]bool{"drop_pending_updates": false}, nil); err != nil {
		return err
	}
	in.Connected("@" + me.Username)

	var offset int64
	for ctx.Err() == nil {
		var ups []update
		err := a.call(ctx, "getUpdates", map[string]any{
			"offset": offset, "timeout": pollTimeout, "allowed_updates": []string{"message", "callback_query"},
		}, &ups)
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
		for _, u := range ups {
			offset = u.UpdateID + 1
			if u.Message != nil {
				if m, ok := a.convert(u.Message); ok {
					in.Deliver(ctx, m)
				}
			}
			if u.CallbackQuery != nil {
				if m, ok := a.press(ctx, u.CallbackQuery); ok {
					in.Deliver(ctx, m)
				}
			}
		}
	}
	return nil
}

func (a *Adapter) convert(msg *message) (channel.Message, bool) {
	if msg.From == nil || msg.From.IsBot {
		return channel.Message{}, false
	}
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}
	atts := a.attachments(msg)
	if text == "" && len(atts) == 0 {
		return channel.Message{}, false
	}
	m := channel.Message{
		Conversation: strconv.FormatInt(msg.Chat.ID, 10),
		ID:           strconv.FormatInt(msg.MessageID, 10),
		Sender:       strconv.FormatInt(msg.From.ID, 10),
		SenderName:   strings.TrimSpace(msg.From.FirstName + " " + msg.From.LastName),
		Direct:       msg.Chat.Type == "private",
		Attachments:  atts,
	}
	if msg.IsTopicMessage && msg.MessageThreadID != 0 {
		m.Thread = strconv.FormatInt(msg.MessageThreadID, 10)
	}
	if msg.ReplyTo != nil && msg.ReplyTo.From != nil && msg.ReplyTo.From.ID == a.botID {
		m.Mentioned = true
	}
	m.Quote = a.quote(msg)
	m.Forwarded = len(msg.ForwardOrigin) > 0 || msg.ForwardFrom != nil || msg.ForwardDate != 0
	// Plain text the sender typed: Telegram marks forwards, quotes and
	// inline bots, so the absence of each means something.
	m.Plain = msg.Text != "" && len(atts) == 0 && m.Quote == nil && !m.Forwarded && msg.ViaBot == nil
	mention := "@" + a.username
	if a.username != "" && strings.Contains(strings.ToLower(text), strings.ToLower(mention)) {
		m.Mentioned = true
		text = strings.TrimSpace(replaceFold(text, mention, ""))
	}
	m.Text = text
	return m, true
}

// quote is the message msg replies to: the part the user picked, or the
// whole text. In a forum topic every message "replies" to the topic's
// first message, which is not a quote.
func (a *Adapter) quote(msg *message) *channel.Quote {
	r := msg.ReplyTo
	if r == nil || (msg.IsTopicMessage && r.MessageID == msg.MessageThreadID) {
		return nil
	}
	q := &channel.Quote{Text: r.Text}
	if msg.Quote != nil && msg.Quote.Text != "" {
		q.Text = msg.Quote.Text
	}
	if q.Text == "" {
		q.Text = r.Caption
	}
	if q.Text == "" {
		switch {
		case len(r.Photo) > 0:
			q.Text = "(photo)"
		case r.Voice != nil:
			q.Text = "(voice note)"
		case r.Video != nil || r.VideoNote != nil:
			q.Text = "(video)"
		case r.Document != nil:
			q.Text = "(file " + r.Document.FileName + ")"
		case r.Sticker != nil:
			q.Text = "(sticker)"
		default:
			return nil
		}
	}
	if r.From != nil {
		q.Self = r.From.ID == a.botID
		q.From = strings.TrimSpace(r.From.FirstName + " " + r.From.LastName)
	}
	return q
}

func (a *Adapter) attachments(msg *message) []channel.Attachment {
	var out []channel.Attachment
	add := func(f *file, name, mediaType string) {
		if f == nil || f.FileID == "" {
			return
		}
		if f.FileName != "" {
			name = f.FileName
		}
		if f.MimeType != "" {
			mediaType = f.MimeType
		}
		id, size := f.FileID, f.FileSize
		out = append(out, channel.Attachment{Name: name, MediaType: mediaType, Size: size,
			Fetch: func(ctx context.Context) ([]byte, error) {
				if size > maxDownload {
					return nil, errors.New("telegram bots can only download files up to 20 MB")
				}
				return a.download(ctx, id)
			}})
	}
	if n := len(msg.Photo); n > 0 {
		add(&msg.Photo[n-1], "photo.jpg", "image/jpeg") // sizes ascend; take the largest
	}
	add(msg.Document, "file", "")
	add(msg.Voice, "voice.ogg", "audio/ogg")
	add(msg.Audio, "audio", "")
	add(msg.Video, "video.mp4", "video/mp4")
	add(msg.VideoNote, "video-note.mp4", "video/mp4")
	if msg.Sticker != nil {
		add(msg.Sticker, "sticker.webp", "image/webp")
	}
	return out
}

func (a *Adapter) download(ctx context.Context, fileID string) ([]byte, error) {
	var f struct {
		FilePath string `json:"file_path"`
	}
	if err := a.call(ctx, "getFile", map[string]string{"file_id": fileID}, &f); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.APIURL+"/file/bot"+a.Token+"/"+f.FilePath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, errors.New("telegram download failed") // the URL holds the token
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram download: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
}

func replaceFold(s, old, repl string) string {
	i := strings.Index(strings.ToLower(s), strings.ToLower(old))
	if i < 0 {
		return s
	}
	return s[:i] + repl + s[i+len(old):]
}

func (a *Adapter) Send(ctx context.Context, to channel.Address, text, replyTo string) error {
	chat, err := a.chatID(to.Conversation)
	if err != nil {
		return err
	}
	params := map[string]any{"chat_id": chat, "text": text, "parse_mode": "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true}}
	if to.Thread != "" {
		params["message_thread_id"], _ = strconv.ParseInt(to.Thread, 10, 64)
	}
	if replyTo != "" {
		id, _ := strconv.ParseInt(replyTo, 10, 64)
		params["reply_parameters"] = map[string]any{"message_id": id, "allow_sending_without_reply": true}
	}
	err = a.call(ctx, "sendMessage", params, nil)
	if err != nil && strings.Contains(err.Error(), "can't parse entities") {
		// Markup the formatter got wrong: send it as plain text instead.
		delete(params, "parse_mode")
		params["text"] = stripTags(text)
		err = a.call(ctx, "sendMessage", params, nil)
	}
	return err
}

func (a *Adapter) Typing(ctx context.Context, to channel.Address) error {
	chat, err := a.chatID(to.Conversation)
	if err != nil {
		return err
	}
	params := map[string]any{"chat_id": chat, "action": "typing"}
	if to.Thread != "" {
		params["message_thread_id"], _ = strconv.ParseInt(to.Thread, 10, 64)
	}
	return a.call(ctx, "sendChatAction", params, nil)
}

// Bot API limits for files a bot sends.
const (
	maxUpload  = 50 << 20
	maxPhoto   = 10 << 20
	maxCaption = 1024
)

func (a *Adapter) MaxFileSize() int64 { return maxUpload }

// SendFile sends a JPEG or PNG up to 10 MB as a photo and anything else as
// a document. A photo Telegram refuses (extreme dimensions) goes again as a
// document; a caption it cannot parse goes again as plain text.
func (a *Adapter) SendFile(ctx context.Context, to channel.Address, name, mediaType string, data []byte, caption string) error {
	chat, err := a.chatID(to.Conversation)
	if err != nil {
		return err
	}
	fields := map[string]string{"chat_id": fmt.Sprint(chat)}
	if to.Thread != "" {
		fields["message_thread_id"] = to.Thread
	}
	if caption != "" {
		fields["caption"], fields["parse_mode"] = caption, "HTML"
	}
	method, field := "sendDocument", "document"
	if (mediaType == "image/jpeg" || mediaType == "image/png") && len(data) <= maxPhoto {
		method, field = "sendPhoto", "photo"
	}
	err = a.upload(ctx, method, fields, field, name, data)
	if err != nil && caption != "" && (strings.Contains(err.Error(), "can't parse entities") || strings.Contains(err.Error(), "caption is too long")) {
		delete(fields, "parse_mode")
		fields["caption"] = truncate(stripTags(caption), maxCaption)
		err = a.upload(ctx, method, fields, field, name, data)
	}
	if err != nil && method == "sendPhoto" && (strings.Contains(err.Error(), "PHOTO_") || strings.Contains(err.Error(), "IMAGE_PROCESS")) {
		err = a.upload(ctx, "sendDocument", fields, "document", name, data)
	}
	return err
}

// upload posts a method with one file as multipart form data.
func (a *Adapter) upload(ctx context.Context, method string, fields map[string]string, field, name string, data []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return err
		}
	}
	fw, err := w.CreateFormFile(field, name)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	client := &http.Client{Transport: a.HTTP.Transport, Timeout: 5 * time.Minute}
	return a.post(ctx, client, method, w.FormDataContentType(), &buf, nil)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-3]) + "..."
	}
	return s
}

var htmlTag = regexp.MustCompile(`</?(?:b|i|s|u|a|code|pre|blockquote)(?:\s[^>]*)?>`)

// stripTags undoes FormatTelegram's markup for a plain-text resend.
func stripTags(s string) string {
	s = htmlTag.ReplaceAllString(s, "")
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&amp;", "&").Replace(s)
}
