// Package googlechat is the Google Chat adapter for a Chat app with an HTTP
// endpoint. Chat posts events to Habakkuk's HTTP server with a bearer token
// Google signs for the app's audience; replies go through the Chat API,
// authorized as the app's service account.
package googlechat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/jwt"
)

const (
	chatIssuer  = "chat@system.gserviceaccount.com"
	chatCerts   = "https://www.googleapis.com/service_accounts/v1/metadata/x509/chat@system.gserviceaccount.com"
	googleCerts = "https://www.googleapis.com/oauth2/v3/certs"
	botScope    = "https://www.googleapis.com/auth/chat.bot"
	maxBody     = 1 << 20
	maxDownload = 25 << 20
)

// Workspace add-on Chat apps get tokens from their own service agent.
var addonAgent = regexp.MustCompile(`^service-\d+@gcp-sa-gsuiteaddons\.iam\.gserviceaccount\.com$`)

type Adapter struct {
	Audience string // project number, or the endpoint URL
	Log      *slog.Logger
	HTTP     *http.Client
	Keys     *jwt.Keys
	APIURL   string // default https://chat.googleapis.com
	Now      func() time.Time

	sa       serviceAccount
	saErr    error
	listener channel.Listener

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

type serviceAccount struct {
	ClientEmail  string `json:"client_email"`
	PrivateKey   string `json:"private_key"`
	PrivateKeyID string `json:"private_key_id"`
	TokenURI     string `json:"token_uri"`
}

// New builds the adapter. credentials is the service account's JSON key, or
// a path to it; a problem with it is reported when the adapter runs.
func New(audience, credentials, apiURL string, log *slog.Logger) *Adapter {
	if apiURL == "" {
		apiURL = "https://chat.googleapis.com"
	}
	if log == nil {
		log = slog.Default()
	}
	hc := &http.Client{Timeout: time.Minute}
	a := &Adapter{Audience: audience, Log: log, HTTP: hc, APIURL: strings.TrimRight(apiURL, "/")}
	if projectNumber(audience) {
		a.Keys = &jwt.Keys{URL: chatCerts, Source: jwt.X509Map, HTTP: hc}
	} else {
		a.Keys = &jwt.Keys{URL: googleCerts, Source: jwt.JWKS, HTTP: hc}
	}
	a.sa, a.saErr = loadCredentials(credentials)
	return a
}

func projectNumber(aud string) bool {
	_, err := strconv.ParseUint(aud, 10, 64)
	return err == nil
}

func loadCredentials(v string) (serviceAccount, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return serviceAccount{}, errors.New("googlechat: the credentials variable is empty")
	}
	data := []byte(v)
	if !strings.HasPrefix(v, "{") {
		b, err := os.ReadFile(v)
		if err != nil {
			return serviceAccount{}, fmt.Errorf("googlechat credentials: %w", err)
		}
		data = b
	}
	var sa serviceAccount
	if err := json.Unmarshal(data, &sa); err != nil {
		return serviceAccount{}, fmt.Errorf("googlechat credentials: %w", err)
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return serviceAccount{}, errors.New("googlechat credentials: not a service account key (no client_email or private_key)")
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}
	return sa, nil
}

func (a *Adapter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Adapter) MaxLen() int               { return 4000 } // Chat takes 4,096 characters
func (a *Adapter) Format(md string) string   { return channel.FormatGoogleChat(md) }
func (a *Adapter) StaleAfter() time.Duration { return 3 * time.Minute }

// Run checks the service account can get a token, then takes events.
func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	if a.saErr != nil {
		return channel.Fatal{Err: a.saErr}
	}
	if _, err := a.appToken(ctx); err != nil {
		return err
	}
	return a.listener.Run(ctx, in, a.sa.ClientEmail, time.Minute, nil)
}

// appToken trades a JWT signed with the service account key for an access
// token, cached until five minutes before it expires.
func (a *Adapter) appToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	if a.token != "" && a.now().Before(a.tokenExp) {
		defer a.mu.Unlock()
		return a.token, nil
	}
	a.mu.Unlock()
	if a.saErr != nil {
		return "", a.saErr
	}
	key, err := jwt.ParsePrivateKey(a.sa.PrivateKey)
	if err != nil {
		return "", channel.Fatal{Err: fmt.Errorf("googlechat credentials: %w", err)}
	}
	now := a.now()
	assertion, err := jwt.Sign(map[string]any{
		"iss": a.sa.ClientEmail, "scope": botScope, "aud": a.sa.TokenURI,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}, key, a.sa.PrivateKeyID)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.sa.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("googlechat token: %w", err)
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
		e := fmt.Errorf("googlechat token: HTTP %d %s %s", resp.StatusCode, out.Error, out.Description)
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
			return "", channel.Fatal{Err: e} // key revoked or account deleted
		}
		return "", e
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.token = out.AccessToken
	a.tokenExp = now.Add(time.Duration(out.ExpiresIn)*time.Second - 5*time.Minute)
	return a.token, nil
}

// authenticate checks Chat's bearer token. With a project number as the
// audience Chat signs it itself; with the endpoint URL it is a Google ID
// token for Chat's (or the add-on's) service account.
func (a *Adapter) authenticate(ctx context.Context, header string) error {
	tok, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return errors.New("no bearer token")
	}
	claims, _, err := jwt.Verify(ctx, tok, a.Keys, a.now())
	if err != nil {
		return err
	}
	if !claims.HasAudience(a.Audience) {
		return errors.New("token is for another audience")
	}
	if projectNumber(a.Audience) {
		if claims.String("iss") != chatIssuer {
			return fmt.Errorf("issuer %q", claims.String("iss"))
		}
		return nil
	}
	switch claims.String("iss") {
	case "https://accounts.google.com", "accounts.google.com":
	default:
		return fmt.Errorf("issuer %q", claims.String("iss"))
	}
	email := claims.String("email")
	if verified, _ := claims["email_verified"].(bool); !verified || (email != chatIssuer && !addonAgent.MatchString(email)) {
		return fmt.Errorf("token is for %q, not Google Chat", email)
	}
	return nil
}

type user struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	Type        string `json:"type"` // HUMAN | BOT
}

type space struct {
	Name      string `json:"name"`
	Type      string `json:"type"`      // DM | ROOM (legacy)
	SpaceType string `json:"spaceType"` // DIRECT_MESSAGE | SPACE | GROUP_CHAT
}

type message struct {
	Name         string `json:"name"`
	Sender       user   `json:"sender"`
	Text         string `json:"text"`
	ArgumentText string `json:"argumentText"`
	Thread       struct {
		Name string `json:"name"`
	} `json:"thread"`
	Annotations []struct {
		Type        string `json:"type"`
		UserMention struct {
			User user `json:"user"`
		} `json:"userMention"`
	} `json:"annotations"`
	SlashCommand *struct{} `json:"slashCommand"`
	Attachment   []struct {
		ContentName       string `json:"contentName"`
		ContentType       string `json:"contentType"`
		AttachmentDataRef struct {
			ResourceName string `json:"resourceName"`
		} `json:"attachmentDataRef"`
		DriveDataRef *struct {
			DriveFileID string `json:"driveFileId"`
		} `json:"driveDataRef"`
	} `json:"attachment"`
	Space *space `json:"space"`
}

// event covers both shapes Chat posts: the Chat app event, and the
// Workspace add-on event with the message under chat.messagePayload.
type event struct {
	Type    string   `json:"type"`
	Message *message `json:"message"`
	Space   *space   `json:"space"`
	Chat    *struct {
		MessagePayload *struct {
			Message *message `json:"message"`
			Space   *space   `json:"space"`
		} `json:"messagePayload"`
	} `json:"chat"`
}

// ServeHTTP takes Chat events (POST /). Replies go through the API, so the
// synchronous answer is always empty.
func (a *Adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	body, ok := channel.ReadBody(w, r, maxBody)
	if !ok {
		return
	}
	if err := a.authenticate(r.Context(), r.Header.Get("Authorization")); err != nil {
		a.Log.Warn("googlechat event refused", "err", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var ev event
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad event", http.StatusBadRequest)
		return
	}
	in := a.listener.Inbox()
	if in == nil {
		http.Error(w, "starting", http.StatusServiceUnavailable)
		return
	}
	msg, sp := ev.Message, ev.Space
	if ev.Chat != nil && ev.Chat.MessagePayload != nil {
		msg, sp = ev.Chat.MessagePayload.Message, ev.Chat.MessagePayload.Space
	} else if ev.Type != "MESSAGE" {
		msg = nil // added to a space, removed, card clicks
	}
	if msg != nil {
		if sp == nil {
			sp = msg.Space
		}
		if m, ok := a.convert(msg, sp); ok {
			in.Deliver(context.WithoutCancel(r.Context()), m)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, "{}")
}

func (a *Adapter) convert(msg *message, sp *space) (channel.Message, bool) {
	if sp == nil || msg.Name == "" || msg.Sender.Type == "BOT" {
		return channel.Message{}, false
	}
	m := channel.Message{
		Conversation: sp.Name, Thread: msg.Thread.Name, ID: msg.Name,
		Sender: msg.Sender.Name, SenderAlt: strings.ToLower(msg.Sender.Email), SenderName: msg.Sender.DisplayName,
		Direct: sp.Type == "DM" || sp.SpaceType == "DIRECT_MESSAGE",
	}
	for _, an := range msg.Annotations {
		if an.Type == "USER_MENTION" && an.UserMention.User.Type == "BOT" {
			m.Mentioned = true
		}
	}
	if msg.SlashCommand != nil {
		m.Mentioned = true
	}
	m.Text = strings.TrimSpace(msg.Text)
	if m.Mentioned && msg.ArgumentText != "" {
		m.Text = strings.TrimSpace(msg.ArgumentText) // the text without the app's mention
	}
	for _, at := range msg.Attachment {
		name, mediaType := at.ContentName, at.ContentType
		switch {
		case at.AttachmentDataRef.ResourceName != "":
			res := at.AttachmentDataRef.ResourceName
			m.Attachments = append(m.Attachments, channel.Attachment{Name: name, MediaType: mediaType,
				Fetch: func(ctx context.Context) ([]byte, error) { return a.download(ctx, res) }})
		case at.DriveDataRef != nil:
			m.Attachments = append(m.Attachments, channel.Attachment{Name: name, MediaType: mediaType,
				Fetch: func(context.Context) ([]byte, error) {
					return nil, errors.New("a Google Drive file, which the app cannot open; ask for it as an upload")
				}})
		}
	}
	return m, m.Text != "" || len(m.Attachments) > 0
}

func (a *Adapter) download(ctx context.Context, resource string) ([]byte, error) {
	tok, err := a.appToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.APIURL+"/v1/media/"+resource+"?alt=media", nil)
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
		return nil, fmt.Errorf("googlechat download: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
}

var spaceName = regexp.MustCompile(`^spaces/[^/?#]+$`)

// Send posts a message in the space, in the thread when there is one.
func (a *Adapter) Send(ctx context.Context, to channel.Address, text, _ string) error {
	if !spaceName.MatchString(to.Conversation) {
		return channel.Undeliverable{Err: fmt.Errorf("googlechat: bad space %q", to.Conversation)}
	}
	body := map[string]any{"text": text}
	endpoint := a.APIURL + "/v1/" + to.Conversation + "/messages"
	if to.Thread != "" {
		body["thread"] = map[string]string{"name": to.Thread}
		endpoint += "?messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD"
	}
	raw, _ := json.Marshal(body)
	tok, err := a.appToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("googlechat send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 {
		return nil
	}
	var apiErr struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&apiErr)
	e := fmt.Errorf("googlechat send: HTTP %d %s %s", resp.StatusCode, apiErr.Error.Status, apiErr.Error.Message)
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		a.mu.Lock()
		a.token = ""
		a.mu.Unlock()
	case http.StatusTooManyRequests:
		after, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return channel.RetryAfter{Err: e, After: time.Duration(max(after, 1)) * time.Second}
	case http.StatusForbidden, http.StatusNotFound, http.StatusBadRequest:
		return channel.Undeliverable{Err: e} // removed from the space, or a message Chat refuses
	}
	return e
}
