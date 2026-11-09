// Package msteams is the Microsoft Teams adapter, through the Bot Framework.
// Teams posts activities to Habakkuk's HTTP server with a bearer token signed
// by the Bot Framework, checked against its published keys; replies go to
// the conversation's service URL with an app token from client credentials.
package msteams

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/jwt"
)

const (
	issuer       = "https://api.botframework.com"
	openIDURL    = "https://login.botframework.com/v1/.well-known/openidconfiguration"
	botScope     = "https://api.botframework.com/.default"
	maxBody      = 1 << 20
	maxDownload  = 25 << 20
	maxConsent   = 50 << 20 // files offered in one-to-one chats
	maxInline    = 1 << 20  // images shown inline in group chats and channels
	consentTTL   = time.Hour
	maxSavedURLs = 5000
)

type Adapter struct {
	AppID       string
	AppPassword string
	TenantID    string // default botframework.com (multi-tenant)
	StatePath   string
	Log         *slog.Logger
	HTTP        *http.Client
	Keys        *jwt.Keys
	TokenURL    string // default https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token
	Now         func() time.Time

	listener channel.Listener

	mu       sync.Mutex
	token    string
	tokenExp time.Time
	st       state
	loaded   bool
	consents map[string]*consent // file offers waiting for the user to accept
}

// state keeps each conversation's service URL, which a reply needs and a
// scheduled message to the conversation cannot get from anywhere else.
type state struct {
	ServiceURLs map[string]string `json:"service_urls"`
}

type consent struct {
	to      channel.Address
	name    string
	data    []byte
	expires time.Time
}

func New(appID, password, tenant, statePath string, log *slog.Logger) *Adapter {
	if tenant == "" {
		tenant = "botframework.com"
	}
	if log == nil {
		log = slog.Default()
	}
	hc := &http.Client{Timeout: time.Minute}
	return &Adapter{
		AppID: appID, AppPassword: password, TenantID: tenant, StatePath: statePath, Log: log, HTTP: hc,
		Keys:     &jwt.Keys{URL: openIDURL, Source: jwt.OpenID, HTTP: hc},
		TokenURL: "https://login.microsoftonline.com/" + url.PathEscape(tenant) + "/oauth2/v2.0/token",
	}
}

func (a *Adapter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// MaxLen keeps a message well under Teams' 28 KB activity limit.
func (a *Adapter) MaxLen() int               { return 8000 }
func (a *Adapter) Format(md string) string   { return md } // Teams renders Markdown
func (a *Adapter) StaleAfter() time.Duration { return 3 * time.Minute }
func (a *Adapter) MaxFileSize() int64        { return maxConsent }

// Run checks the app credentials, then takes activities until ctx ends.
func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	if err := a.load(); err != nil {
		return channel.Fatal{Err: err}
	}
	if _, err := a.appToken(ctx); err != nil {
		return err
	}
	return a.listener.Run(ctx, in, "bot "+a.AppID, time.Minute, nil)
}

func (a *Adapter) load() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loaded {
		return nil
	}
	if a.StatePath != "" {
		if err := channel.LoadState(a.StatePath, &a.st); err != nil {
			return fmt.Errorf("msteams state: %w", err)
		}
	}
	if a.st.ServiceURLs == nil {
		a.st.ServiceURLs = map[string]string{}
	}
	a.loaded = true
	return nil
}

func (a *Adapter) rememberService(conv, serviceURL string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.st.ServiceURLs[conv] == serviceURL {
		return
	}
	if len(a.st.ServiceURLs) >= maxSavedURLs {
		clear(a.st.ServiceURLs) // rebuilt as conversations write again
	}
	a.st.ServiceURLs[conv] = serviceURL
	if a.StatePath != "" {
		if err := channel.SaveState(a.StatePath, &a.st); err != nil {
			a.Log.Warn("save msteams state failed", "err", err)
		}
	}
}

func (a *Adapter) serviceURL(conv string) (string, error) {
	if err := a.load(); err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if u := a.st.ServiceURLs[conv]; u != "" {
		return u, nil
	}
	return "", channel.Undeliverable{Err: fmt.Errorf("msteams: no service URL known for conversation %s; it must message the bot first", conv)}
}

// appToken returns a cached Bot Framework token, fetching a new one with
// client credentials when it is within five minutes of expiring.
func (a *Adapter) appToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	if a.token != "" && a.now().Before(a.tokenExp) {
		defer a.mu.Unlock()
		return a.token, nil
	}
	a.mu.Unlock()
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {a.AppID},
		"client_secret": {a.AppPassword}, "scope": {botScope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("msteams token: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		e := fmt.Errorf("msteams token: HTTP %d %s %s", resp.StatusCode, out.Error, firstLine(out.Description))
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
			return "", channel.Fatal{Err: e} // wrong app id, secret or tenant
		}
		return "", e
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.token = out.AccessToken
	a.tokenExp = a.now().Add(time.Duration(out.ExpiresIn)*time.Second - 5*time.Minute)
	return a.token, nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	s, _, _ = strings.Cut(s, "\r")
	return s
}

type account struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	AADObjectID string `json:"aadObjectId,omitempty"`
}

type conversation struct {
	ID               string `json:"id"`
	ConversationType string `json:"conversationType,omitempty"` // personal | groupChat | channel
	IsGroup          bool   `json:"isGroup,omitempty"`
}

type attachment struct {
	ContentType string          `json:"contentType"`
	ContentURL  string          `json:"contentUrl,omitempty"`
	Name        string          `json:"name,omitempty"`
	Content     json.RawMessage `json:"content,omitempty"`
}

type entity struct {
	Type      string   `json:"type"`
	Mentioned *account `json:"mentioned,omitempty"`
	Text      string   `json:"text,omitempty"`
}

type activity struct {
	Type         string          `json:"type"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	ServiceURL   string          `json:"serviceUrl,omitempty"`
	ChannelID    string          `json:"channelId,omitempty"`
	From         *account        `json:"from,omitempty"`
	Recipient    *account        `json:"recipient,omitempty"`
	Conversation *conversation   `json:"conversation,omitempty"`
	Text         string          `json:"text,omitempty"`
	TextFormat   string          `json:"textFormat,omitempty"`
	Attachments  []attachment    `json:"attachments,omitempty"`
	Entities     []entity        `json:"entities,omitempty"`
	ReplyToID    string          `json:"replyToId,omitempty"`
	Value        json.RawMessage `json:"value,omitempty"`
}

// ServeHTTP takes activities from the Bot Framework (POST /).
func (a *Adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	body, ok := channel.ReadBody(w, r, maxBody)
	if !ok {
		return
	}
	var act activity
	if err := json.Unmarshal(body, &act); err != nil {
		http.Error(w, "bad activity", http.StatusBadRequest)
		return
	}
	if err := a.authenticate(r.Context(), r.Header.Get("Authorization"), &act); err != nil {
		a.Log.Warn("msteams activity refused", "err", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	in := a.listener.Inbox()
	if in == nil {
		http.Error(w, "starting", http.StatusServiceUnavailable)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	if act.Conversation != nil && act.ServiceURL != "" {
		a.rememberService(act.Conversation.ID, act.ServiceURL)
	}
	switch act.Type {
	case "message":
		if m, ok := a.convert(&act); ok {
			in.Deliver(ctx, m)
		}
	case "invoke":
		if act.Name == "fileConsent/invoke" {
			a.consentAnswer(ctx, &act)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, "{}")
}

// authenticate checks the Bot Framework's bearer token: signed by a current
// key endorsed for the activity's channel, issued by the Bot Framework, for
// this app, and naming the service URL the activity claims.
func (a *Adapter) authenticate(ctx context.Context, header string, act *activity) error {
	tok, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return errors.New("no bearer token")
	}
	claims, key, err := jwt.Verify(ctx, tok, a.Keys, a.now())
	if err != nil {
		return err
	}
	if claims.String("iss") != issuer {
		return fmt.Errorf("issuer %q", claims.String("iss"))
	}
	if !claims.HasAudience(a.AppID) {
		return errors.New("token is for another app")
	}
	if len(key.Endorsements) > 0 && !slices.Contains(key.Endorsements, act.ChannelID) {
		return fmt.Errorf("signing key is not endorsed for channel %q", act.ChannelID)
	}
	if su := claims.String("serviceurl"); su != "" && !sameServiceURL(su, act.ServiceURL) {
		return errors.New("service URL does not match the token")
	}
	return nil
}

func sameServiceURL(x, y string) bool {
	return strings.EqualFold(strings.TrimRight(x, "/"), strings.TrimRight(y, "/"))
}

var tags = regexp.MustCompile(`<[^>]+>`)

func (a *Adapter) convert(act *activity) (channel.Message, bool) {
	if act.From == nil || act.Conversation == nil || act.Recipient == nil || act.From.ID == act.Recipient.ID {
		return channel.Message{}, false
	}
	text := act.Text
	m := channel.Message{
		Conversation: act.Conversation.ID, ID: act.ID, Sender: act.From.ID, SenderName: act.From.Name,
		Direct: act.Conversation.ConversationType == "personal" || (act.Conversation.ConversationType == "" && !act.Conversation.IsGroup),
	}
	if act.From.AADObjectID != "" {
		m.Sender, m.SenderAlt = act.From.AADObjectID, act.From.ID
	}
	for _, e := range act.Entities {
		if e.Type == "mention" && e.Mentioned != nil && e.Mentioned.ID == act.Recipient.ID {
			m.Mentioned = true
			if e.Text != "" {
				text = strings.ReplaceAll(text, e.Text, "")
			}
		}
	}
	text = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`).Replace(tags.ReplaceAllString(text, ""))
	m.Text = strings.TrimSpace(text)
	m.Attachments = a.attachments(act)
	return m, m.Text != "" || len(m.Attachments) > 0
}

// attachments turns files and pasted images into channel attachments.
// Files in one-to-one chats come with a pre-authorized download URL; pasted
// images need the app token, which only goes to Microsoft's hosts.
func (a *Adapter) attachments(act *activity) []channel.Attachment {
	var out []channel.Attachment
	for _, at := range act.Attachments {
		switch {
		case at.ContentType == "application/vnd.microsoft.teams.file.download.info":
			var info struct {
				DownloadURL string `json:"downloadUrl"`
				FileType    string `json:"fileType"`
			}
			if json.Unmarshal(at.Content, &info) != nil || info.DownloadURL == "" {
				continue
			}
			link := info.DownloadURL
			out = append(out, channel.Attachment{Name: at.Name,
				Fetch: func(ctx context.Context) ([]byte, error) { return a.download(ctx, link, false) }})
		case strings.HasPrefix(at.ContentType, "image/") && at.ContentURL != "":
			link, auth := at.ContentURL, trustedHost(at.ContentURL, act.ServiceURL)
			name := at.Name
			if name == "" {
				name = "image." + strings.TrimPrefix(at.ContentType, "image/")
			}
			out = append(out, channel.Attachment{Name: name, MediaType: at.ContentType,
				Fetch: func(ctx context.Context) ([]byte, error) { return a.download(ctx, link, auth) }})
		}
	}
	return out
}

func trustedHost(link, serviceURL string) bool {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if s, err := url.Parse(serviceURL); err == nil && strings.EqualFold(s.Hostname(), h) {
		return true
	}
	for _, suffix := range []string{".botframework.com", ".skype.com", ".teams.microsoft.com", ".trafficmanager.net"} {
		if strings.HasSuffix(h, suffix) {
			return true
		}
	}
	return false
}

func (a *Adapter) download(ctx context.Context, link string, auth bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	if auth {
		tok, err := a.appToken(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("msteams download: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
}

// post sends an activity to a conversation, as a reply when replyTo is set.
func (a *Adapter) post(ctx context.Context, conv, replyTo string, act *activity) error {
	base, err := a.serviceURL(conv)
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(base, "/") + "/v3/conversations/" + url.PathEscape(conv) + "/activities"
	if replyTo != "" {
		endpoint += "/" + url.PathEscape(replyTo)
	}
	act.From = &account{ID: "28:" + a.AppID}
	body, err := json.Marshal(act)
	if err != nil {
		return err
	}
	tok, err := a.appToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("msteams send: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 300 {
		return nil
	}
	e := fmt.Errorf("msteams send: HTTP %d %s", resp.StatusCode, firstLine(string(raw)))
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		a.mu.Lock()
		a.token = "" // fetch a fresh one on the retry
		a.mu.Unlock()
	case http.StatusTooManyRequests:
		after, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return channel.RetryAfter{Err: e, After: time.Duration(max(after, 1)) * time.Second}
	case http.StatusForbidden, http.StatusNotFound, http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return channel.Undeliverable{Err: e} // removed from the conversation, or a message Teams refuses
	}
	return e
}

func (a *Adapter) Send(ctx context.Context, to channel.Address, text, replyTo string) error {
	return a.post(ctx, to.Conversation, replyTo, &activity{Type: "message", Text: text, TextFormat: "markdown"})
}

func (a *Adapter) Typing(ctx context.Context, to channel.Address) error {
	return a.post(ctx, to.Conversation, "", &activity{Type: "typing"})
}

// personal reports whether a conversation is a one-to-one chat: group
// chats and channels have "19:" thread ids.
func personal(conv string) bool { return !strings.HasPrefix(conv, "19:") }

// SendFile offers a file in a one-to-one chat with a consent card; it is
// uploaded to the user's OneDrive when they accept (within an hour, while
// this process runs). Group chats and channels have no bot upload, so only
// images up to 1 MB go there, shown inline.
func (a *Adapter) SendFile(ctx context.Context, to channel.Address, name, mediaType string, data []byte, caption string) error {
	if !personal(to.Conversation) {
		if !strings.HasPrefix(mediaType, "image/") || len(data) > maxInline {
			return channel.Undeliverable{Err: errors.New("msteams: bots can only send files in one-to-one chats; in group chats and channels only images up to 1 MB")}
		}
		return a.post(ctx, to.Conversation, "", &activity{Type: "message", Text: caption, TextFormat: "markdown",
			Attachments: []attachment{{ContentType: mediaType, Name: name,
				ContentURL: "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)}}})
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	a.mu.Lock()
	if a.consents == nil {
		a.consents = map[string]*consent{}
	}
	for k, c := range a.consents {
		if a.now().After(c.expires) {
			delete(a.consents, k)
		}
	}
	a.consents[id] = &consent{to: to, name: name, data: data, expires: a.now().Add(consentTTL)}
	a.mu.Unlock()
	card, _ := json.Marshal(map[string]any{
		"description": caption, "sizeInBytes": len(data),
		"acceptContext": map[string]string{"id": id}, "declineContext": map[string]string{"id": id},
	})
	return a.post(ctx, to.Conversation, "", &activity{Type: "message", Attachments: []attachment{{
		ContentType: "application/vnd.microsoft.teams.card.file.consent", Name: name, Content: card}}})
}

// consentAnswer handles the user's answer to a file consent card: on
// accept, upload to the URL Teams gives, then post the file card.
func (a *Adapter) consentAnswer(ctx context.Context, act *activity) {
	var v struct {
		Action  string `json:"action"`
		Context struct {
			ID string `json:"id"`
		} `json:"context"`
		UploadInfo struct {
			Name       string `json:"name"`
			UploadURL  string `json:"uploadUrl"`
			ContentURL string `json:"contentUrl"`
			UniqueID   string `json:"uniqueId"`
			FileType   string `json:"fileType"`
		} `json:"uploadInfo"`
	}
	if json.Unmarshal(act.Value, &v) != nil {
		return
	}
	a.mu.Lock()
	c := a.consents[v.Context.ID]
	delete(a.consents, v.Context.ID)
	a.mu.Unlock()
	if c == nil || v.Action != "accept" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		if err := a.upload(ctx, v.UploadInfo.UploadURL, c.data); err != nil {
			a.Log.Warn("msteams file upload failed", "name", c.name, "err", err)
			_ = a.Send(ctx, c.to, "Sorry, uploading "+c.name+" failed.", "")
			return
		}
		info, _ := json.Marshal(map[string]string{"uniqueId": v.UploadInfo.UniqueID, "fileType": v.UploadInfo.FileType})
		if err := a.post(ctx, c.to.Conversation, "", &activity{Type: "message", Attachments: []attachment{{
			ContentType: "application/vnd.microsoft.teams.card.file.info", ContentURL: v.UploadInfo.ContentURL,
			Name: v.UploadInfo.Name, Content: info}}}); err != nil {
			a.Log.Warn("msteams file card failed", "err", err)
		}
	}()
}

// upload puts the file at the URL from the (authenticated) consent answer.
func (a *Adapter) upload(ctx context.Context, uploadURL string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(data)-1, len(data)))
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
