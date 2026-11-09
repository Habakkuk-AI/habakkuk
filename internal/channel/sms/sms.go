// Package sms is the Twilio adapter. Twilio posts inbound SMS and MMS to
// Habakkuk's HTTP server, signed with the account's auth token; replies go out
// through the Messages API, with media served from Habakkuk for MMS.
package sms

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

const (
	maxBody     = 64 << 10 // webhook form bodies are small
	maxMedia    = 25 << 20
	maxMMS      = 5 << 20 // Twilio's limit for one MMS
	maxSegments = 1600    // characters Twilio takes in one Body
)

type Adapter struct {
	AccountSID          string
	AuthToken           string
	From                string // E.164 sender, or
	MessagingServiceSID string
	WebhookURL          string // the exact URL Twilio calls: <public_url>/channels/<account>
	APIURL              string // default https://api.twilio.com
	HTTP                *http.Client

	listener channel.Listener
	files    channel.PublicFiles
}

func New(sid, token, from, service, webhookURL, apiURL string) *Adapter {
	if apiURL == "" {
		apiURL = "https://api.twilio.com"
	}
	return &Adapter{AccountSID: sid, AuthToken: token, From: from, MessagingServiceSID: service,
		WebhookURL: strings.TrimRight(webhookURL, "/"), APIURL: strings.TrimRight(apiURL, "/"),
		HTTP: &http.Client{Timeout: time.Minute}}
}

func (a *Adapter) MaxLen() int               { return maxSegments }
func (a *Adapter) Format(md string) string   { return channel.FormatPlain(md) }
func (a *Adapter) StaleAfter() time.Duration { return 3 * time.Minute }
func (a *Adapter) MaxFileSize() int64        { return maxMMS }

// Run checks the credentials once, then takes webhooks until ctx ends.
func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	if a.AuthToken == "" {
		return channel.Fatal{Err: errors.New("sms: the auth token variable is empty")}
	}
	if err := a.call(ctx, http.MethodGet, "/Accounts/"+a.AccountSID+".json", nil, nil); err != nil {
		return err
	}
	who := a.From
	if who == "" {
		who = a.MessagingServiceSID
	}
	return a.listener.Run(ctx, in, "twilio "+who, time.Minute, nil)
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

// Twilio error codes that mean the number can never be reached.
var unreachable = []int{
	21211, // invalid To number
	21408, // region not enabled
	21610, // the recipient replied STOP
	21612, // the To number cannot receive from this From
	21614, // To is not a mobile number
}

func (a *Adapter) call(ctx context.Context, method, path string, form url.Values, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, a.APIURL+"/2010-04-01"+path, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(a.AccountSID, a.AuthToken)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("twilio: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 300 {
		if out != nil {
			return json.Unmarshal(raw, out)
		}
		return nil
	}
	var ae apiError
	_ = json.Unmarshal(raw, &ae)
	e := fmt.Errorf("twilio %s: HTTP %d: %d %s", path, resp.StatusCode, ae.Code, ae.Message)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return channel.Fatal{Err: e}
	case resp.StatusCode == http.StatusTooManyRequests:
		after, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return channel.RetryAfter{Err: e, After: time.Duration(max(after, 1)) * time.Second}
	case slices.Contains(unreachable, ae.Code):
		return channel.Undeliverable{Err: e}
	}
	return e
}

// ServeHTTP takes Twilio's webhook (POST /) and serves MMS media
// (GET /media/<token>).
func (a *Adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/media/") {
		a.files.ServeHTTP(w, r)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	body, ok := channel.ReadBody(w, r, maxBody)
	if !ok {
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	full := a.WebhookURL
	if r.URL.RawQuery != "" {
		full += "?" + r.URL.RawQuery
	}
	if !Valid(a.AuthToken, full, form, r.Header.Get("X-Twilio-Signature")) {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	in := a.listener.Inbox()
	if in == nil {
		http.Error(w, "starting", http.StatusServiceUnavailable)
		return
	}
	if m, ok := a.convert(form); ok {
		in.Deliver(context.WithoutCancel(r.Context()), m)
	}
	w.Header().Set("Content-Type", "text/xml")
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Response></Response>`)
}

// Valid checks X-Twilio-Signature: base64 HMAC-SHA1, keyed with the auth
// token, of the full URL followed by each POST parameter's name and value
// sorted by name.
func Valid(token, fullURL string, form url.Values, signature string) bool {
	if token == "" || signature == "" {
		return false
	}
	got, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return false
	}
	return hmac.Equal(got, sign(token, fullURL, form))
}

func sign(token, fullURL string, form url.Values) []byte {
	mac := hmac.New(sha1.New, []byte(token))
	mac.Write([]byte(fullURL))
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		for _, v := range form[k] {
			mac.Write([]byte(k + v))
		}
	}
	return mac.Sum(nil)
}

func (a *Adapter) convert(form url.Values) (channel.Message, bool) {
	from, sid := form.Get("From"), form.Get("MessageSid")
	if from == "" || sid == "" {
		return channel.Message{}, false
	}
	m := channel.Message{
		Conversation: from, ID: sid, Sender: strings.TrimPrefix(from, "+"),
		Text: form.Get("Body"), Direct: true,
	}
	n, _ := strconv.Atoi(form.Get("NumMedia"))
	for i := range min(n, 10) {
		link, mediaType := form.Get(fmt.Sprintf("MediaUrl%d", i)), form.Get(fmt.Sprintf("MediaContentType%d", i))
		if link == "" {
			continue
		}
		m.Attachments = append(m.Attachments, channel.Attachment{
			Name: fmt.Sprintf("mms-%d%s", i+1, ext(mediaType)), MediaType: mediaType,
			Fetch: func(ctx context.Context) ([]byte, error) { return a.download(ctx, link) },
		})
	}
	return m, m.Text != "" || len(m.Attachments) > 0
}

func ext(mediaType string) string {
	_, sub, ok := strings.Cut(mediaType, "/")
	if !ok || sub == "" || strings.ContainsAny(sub, "+.;") {
		return ""
	}
	return "." + strings.TrimPrefix(sub, "x-")
}

// download fetches inbound media. Twilio's media URLs take the account's
// basic auth and redirect to storage; the redirect drops the credentials.
func (a *Adapter) download(ctx context.Context, link string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	// Only Twilio gets the credentials, whatever URL the webhook named.
	if u, err := url.Parse(link); err == nil && (strings.HasSuffix(u.Hostname(), ".twilio.com") || strings.HasPrefix(link, a.APIURL+"/")) {
		req.SetBasicAuth(a.AccountSID, a.AuthToken)
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("twilio media: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxMedia+1))
}

func (a *Adapter) message(ctx context.Context, to string, text string, media string) error {
	if !strings.HasPrefix(to, "+") {
		return channel.Undeliverable{Err: fmt.Errorf("sms: %q is not an E.164 number", to)}
	}
	form := url.Values{"To": {to}}
	if a.From != "" {
		form.Set("From", a.From)
	} else {
		form.Set("MessagingServiceSid", a.MessagingServiceSID)
	}
	if text != "" {
		form.Set("Body", text)
	}
	if media != "" {
		form.Set("MediaUrl", media)
	}
	return a.call(ctx, http.MethodPost, "/Accounts/"+a.AccountSID+"/Messages.json", form, nil)
}

// Send posts one message; Twilio splits it into segments on the way.
func (a *Adapter) Send(ctx context.Context, to channel.Address, text, _ string) error {
	return a.message(ctx, to.Conversation, text, "")
}

// SendFile sends an MMS. Twilio fetches the media from a one-hour URL on
// Habakkuk's HTTP server, so http.public_url must reach this process.
func (a *Adapter) SendFile(ctx context.Context, to channel.Address, _, mediaType string, data []byte, caption string) error {
	if a.WebhookURL == "" {
		return channel.Undeliverable{Err: errors.New("sms: MMS needs http.public_url")}
	}
	tok := a.files.Put(data, mediaType)
	return a.message(ctx, to.Conversation, caption, a.WebhookURL+"/media/"+tok)
}
