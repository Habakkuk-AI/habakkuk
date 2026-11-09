// Package nextcloudtalk is the adapter for a Nextcloud Talk bot (Talk 17.1
// and later). Talk posts each message in the rooms the bot is enabled in to
// Habakkuk's HTTP server as an Activity Streams object, signed with the bot's
// shared secret; replies go back through the OCS bot API, signed the same
// way. Bots cannot upload files or show typing.
package nextcloudtalk

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
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
)

const maxBody = 1 << 20

type Adapter struct {
	URL     string // the Nextcloud server, e.g. https://cloud.example.com
	Secret  string // the secret given to occ talk:bot:install
	Mention string // a message naming "@" + this is for Habakkuk
	HTTP    *http.Client

	listener channel.Listener

	mu      sync.Mutex
	reacted map[string]string // room/message -> the bot's reaction
}

func New(serverURL, secret, mention string) *Adapter {
	if mention == "" {
		mention = "Habakkuk"
	}
	return &Adapter{URL: strings.TrimRight(serverURL, "/"), Secret: secret, Mention: mention, HTTP: &http.Client{Timeout: time.Minute}}
}

func (a *Adapter) MaxLen() int               { return 32000 }
func (a *Adapter) Format(md string) string   { return md } // Talk renders Markdown
func (a *Adapter) StaleAfter() time.Duration { return 3 * time.Minute }

// Sign is the hex HMAC-SHA256 of random followed by data, keyed with the
// bot secret: Talk signs webhook bodies this way, and bots sign the
// message text they send.
func Sign(secret, random string, data []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(random))
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// Run takes webhooks until ctx ends. Talk has no call to check the secret
// with; a wrong one shows as rejected webhooks and sends.
func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	if len(a.Secret) < 40 {
		return channel.Fatal{Err: errors.New("nextcloudtalk: the secret variable must hold the bot's secret (40 to 128 characters)")}
	}
	return a.listener.Run(ctx, in, "Nextcloud Talk "+a.URL, time.Minute, nil)
}

type activity struct {
	Type  string `json:"type"`
	Actor struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"actor"`
	Object struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Content string `json:"content"`
	} `json:"object"`
	Target struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"target"`
}

// ServeHTTP takes Talk's webhooks (POST /).
func (a *Adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	body, ok := channel.ReadBody(w, r, maxBody)
	if !ok {
		return
	}
	random := r.Header.Get("X-Nextcloud-Talk-Random")
	sig := strings.ToLower(r.Header.Get("X-Nextcloud-Talk-Signature"))
	if a.Secret == "" || random == "" || !hmac.Equal([]byte(sig), []byte(Sign(a.Secret, random, body))) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	var act activity
	if err := json.Unmarshal(body, &act); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	in := a.listener.Inbox()
	if in == nil {
		http.Error(w, "starting", http.StatusServiceUnavailable)
		return
	}
	if m, ok := a.convert(&act); ok {
		in.Deliver(context.WithoutCancel(r.Context()), m)
	}
	w.WriteHeader(http.StatusOK)
}

var placeholder = regexp.MustCompile(`\{([a-z0-9_-]+)\}`)

func (a *Adapter) convert(act *activity) (channel.Message, bool) {
	if act.Type != "Create" || act.Object.Name != "message" || act.Target.ID == "" || act.Object.ID == "" ||
		act.Actor.Type != "Person" {
		return channel.Message{}, false // joins, leaves, reactions, other bots
	}
	var c struct {
		Message    string          `json:"message"`
		Parameters json.RawMessage `json:"parameters"` // an object, or [] when empty
	}
	if err := json.Unmarshal([]byte(act.Object.Content), &c); err != nil {
		return channel.Message{}, false
	}
	var params map[string]struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if len(c.Parameters) > 0 && c.Parameters[0] == '{' {
		_ = json.Unmarshal(c.Parameters, &params)
	}
	text := placeholder.ReplaceAllStringFunc(c.Message, func(s string) string {
		p, ok := params[s[1:len(s)-1]]
		switch {
		case !ok:
			return s
		case p.Type == "file":
			return "[shared file: " + p.Name + "]"
		case strings.HasPrefix(s, "{mention-"):
			return "@" + p.Name
		}
		return p.Name
	})
	m := channel.Message{ID: act.Object.ID, Conversation: act.Target.ID, SenderName: act.Actor.Name,
		Sender: strings.TrimPrefix(act.Actor.ID, "users/")}
	// Bots get every message in the rooms they are enabled in, with no
	// sign of whether the room is one-to-one: "@Habakkuk" marks the ones
	// for Habakkuk when the channel's groups is mention.
	tag := "@" + a.Mention
	if i := strings.Index(strings.ToLower(text), strings.ToLower(tag)); i >= 0 {
		m.Mentioned = true
		text = text[:i] + text[i+len(tag):]
	}
	m.Text = strings.TrimSpace(text)
	return m, m.Text != ""
}

func random() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// do sends a signed request to the bot API; sign is the signed text.
func (a *Adapter) do(ctx context.Context, method, path, sign string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	rnd := random()
	req, err := http.NewRequestWithContext(ctx, method, a.URL+"/ocs/v2.php/apps/spreed/api/v1/bot/"+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("OCS-APIRequest", "true")
	req.Header.Set("X-Nextcloud-Talk-Bot-Random", rnd)
	req.Header.Set("X-Nextcloud-Talk-Bot-Signature", Sign(a.Secret, rnd, []byte(sign)))
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("nextcloudtalk %s: %w", path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 300 {
		return nil
	}
	e := fmt.Errorf("nextcloudtalk %s: HTTP %d", path, resp.StatusCode)
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return channel.RetryAfter{Err: e, After: 10 * time.Second}
	case http.StatusUnauthorized:
		return channel.Undeliverable{Err: fmt.Errorf("%w (the secret does not match the installed bot)", e)}
	case http.StatusBadRequest, http.StatusNotFound, http.StatusRequestEntityTooLarge:
		return channel.Undeliverable{Err: e}
	}
	return e
}

// Send posts a message into the room, as a reply to replyTo when given.
// The reference id makes a retried send show once.
func (a *Adapter) Send(ctx context.Context, to channel.Address, text, replyTo string) error {
	body := map[string]any{"message": text}
	if id, err := strconv.Atoi(replyTo); err == nil {
		body["replyTo"] = id
	}
	if k := channel.SendKey(ctx); k != "" {
		sum := sha256.Sum256([]byte(k))
		body["referenceId"] = hex.EncodeToString(sum[:])
	}
	return a.do(ctx, http.MethodPost, url.PathEscape(to.Conversation)+"/message", text, body)
}

// React replaces the bot's reaction on a message; "" removes it. Talk
// keeps every reaction until it is deleted, so the adapter remembers the
// one it set on each recent message.
func (a *Adapter) React(ctx context.Context, to channel.Address, messageID, emoji string) error {
	if messageID == "" {
		return nil
	}
	path := url.PathEscape(to.Conversation) + "/reaction/" + url.PathEscape(messageID)
	key := to.Conversation + "/" + messageID
	a.mu.Lock()
	old := a.reacted[key]
	a.mu.Unlock()
	if old == emoji {
		return nil
	}
	if old != "" {
		if err := a.do(ctx, http.MethodDelete, path, old, map[string]string{"reaction": old}); err != nil {
			return err
		}
	}
	if emoji != "" {
		if err := a.do(ctx, http.MethodPost, path, emoji, map[string]string{"reaction": emoji}); err != nil {
			return err
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.reacted == nil || len(a.reacted) > 1000 {
		a.reacted = map[string]string{}
	}
	if emoji == "" {
		delete(a.reacted, key)
	} else {
		a.reacted[key] = emoji
	}
	return nil
}
