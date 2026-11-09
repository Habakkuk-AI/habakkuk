// Package ntfy is the ntfy adapter: replies and notifications are
// published to a topic, and with a subscribe topic set, messages published
// there become chat messages, read from ntfy's JSON stream.
package ntfy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

const (
	beatEvery   = time.Minute
	maxDownload = 25 << 20
	maxUpload   = 15 << 20 // ntfy.sh's attachment limit
	// outTag marks what Habakkuk publishes (ntfy shows it as a robot icon),
	// so a topic that is both read and written does not answer itself.
	outTag = "robot"
)

type Adapter struct {
	URL       string // the server, e.g. https://ntfy.sh
	Topic     string // published to
	Subscribe string // read from; empty for send-only
	Token     string
	StatePath string
	HTTP      *http.Client // for publishing and downloads
	Stream    *http.Client // for the subscription; no overall timeout

	mu     sync.Mutex
	lastID string
	loaded bool
}

func New(server, topic, subscribe, token, statePath string) *Adapter {
	return &Adapter{URL: strings.TrimRight(server, "/"), Topic: topic, Subscribe: subscribe, Token: token,
		StatePath: statePath, HTTP: &http.Client{Timeout: time.Minute}, Stream: &http.Client{}}
}

func (a *Adapter) MaxLen() int               { return 3000 } // ntfy turns messages over 4 KB into attachments
func (a *Adapter) Format(md string) string   { return md }   // published with Markdown: yes
func (a *Adapter) StaleAfter() time.Duration { return 3 * beatEvery }
func (a *Adapter) MaxFileSize() int64        { return maxUpload }

func (a *Adapter) auth(req *http.Request) {
	if a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
}

type state struct {
	LastID string `json:"last_id"`
}

func (a *Adapter) since() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.loaded && a.StatePath != "" {
		var st state
		_ = channel.LoadState(a.StatePath, &st)
		a.lastID, a.loaded = st.LastID, true
	}
	return a.lastID
}

func (a *Adapter) seen(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastID = id
	if a.StatePath != "" {
		_ = channel.SaveState(a.StatePath, state{LastID: id})
	}
}

// Run, send-only, just beats. With a subscribe topic it reads the topic's
// JSON stream from the last message seen, so nothing published while
// Habakkuk was down is missed; the server's keepalives keep it beating.
func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	if a.Subscribe == "" {
		in.Connected("ntfy " + a.URL + "/" + a.Topic + " (send only)")
		t := time.NewTicker(beatEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
				in.Beat()
			}
		}
	}
	q := url.Values{}
	if id := a.since(); id != "" {
		q.Set("since", id)
	}
	endpoint := a.URL + "/" + url.PathEscape(a.Subscribe) + "/json"
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return channel.Fatal{Err: err}
	}
	a.auth(req)
	resp, err := a.Stream.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy subscribe: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return channel.Fatal{Err: fmt.Errorf("ntfy subscribe %s: HTTP %d (check token_env)", a.Subscribe, resp.StatusCode)}
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("ntfy subscribe %s: HTTP %d", a.Subscribe, resp.StatusCode)
	}
	in.Connected("ntfy " + a.URL + "/" + a.Subscribe)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		in.Beat()
		var ev event
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Event != "message" {
			continue
		}
		if m, ok := a.convert(&ev); ok {
			in.Deliver(ctx, m)
		}
		a.seen(ev.ID)
	}
	if ctx.Err() != nil {
		return nil
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("ntfy subscribe: %w", err)
	}
	return errors.New("ntfy subscribe: stream ended")
}

type event struct {
	ID         string   `json:"id"`
	Event      string   `json:"event"` // open | keepalive | message
	Topic      string   `json:"topic"`
	Title      string   `json:"title"`
	Message    string   `json:"message"`
	Tags       []string `json:"tags"`
	Attachment *struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Size int64  `json:"size"`
		URL  string `json:"url"`
	} `json:"attachment"`
}

func (a *Adapter) convert(ev *event) (channel.Message, bool) {
	if slices.Contains(ev.Tags, outTag) {
		return channel.Message{}, false
	}
	text := ev.Message
	if ev.Title != "" {
		text = ev.Title + "\n\n" + text
	}
	// ntfy has no sender identity: whoever can publish to the topic can
	// talk, so protect the topic and allow "ntfy" (or "*").
	m := channel.Message{Conversation: a.Topic, ID: ev.ID, Sender: "ntfy", Direct: true, Text: strings.TrimSpace(text)}
	if at := ev.Attachment; at != nil && at.URL != "" {
		link := at.URL
		m.Attachments = append(m.Attachments, channel.Attachment{Name: at.Name, MediaType: at.Type, Size: at.Size,
			Fetch: func(ctx context.Context) ([]byte, error) { return a.download(ctx, link) }})
		if m.Text == "You received a file: "+at.Name {
			m.Text = "" // ntfy's placeholder for a file without a message
		}
	}
	return m, m.Text != "" || len(m.Attachments) > 0
}

func (a *Adapter) download(ctx context.Context, link string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(link, a.URL+"/") {
		a.auth(req) // the token only goes to the configured server
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ntfy attachment: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
}

func (a *Adapter) publish(ctx context.Context, method, topic string, body []byte, header http.Header) error {
	if topic == "" || strings.ContainsAny(topic, "/?#") {
		return channel.Undeliverable{Err: fmt.Errorf("ntfy: bad topic %q", topic)}
	}
	req, err := http.NewRequestWithContext(ctx, method, a.URL+"/"+url.PathEscape(topic), bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Tags", outTag)
	a.auth(req)
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy publish: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 {
		return nil
	}
	var apiErr struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&apiErr)
	e := fmt.Errorf("ntfy publish %s: HTTP %d %s", topic, resp.StatusCode, apiErr.Error)
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		after, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return channel.RetryAfter{Err: e, After: time.Duration(max(after, 10)) * time.Second}
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestEntityTooLarge, http.StatusBadRequest:
		return channel.Undeliverable{Err: e}
	}
	return e
}

// Send publishes the text to the conversation's topic.
func (a *Adapter) Send(ctx context.Context, to channel.Address, text, _ string) error {
	return a.publish(ctx, http.MethodPost, to.Conversation, []byte(text), http.Header{"Markdown": {"yes"}})
}

// SendFile publishes the file as an attachment, with the caption as the
// message.
func (a *Adapter) SendFile(ctx context.Context, to channel.Address, name, _ string, data []byte, caption string) error {
	h := http.Header{"Filename": {mime.QEncoding.Encode("utf-8", name)}}
	if caption != "" {
		h.Set("Message", mime.QEncoding.Encode("utf-8", strings.ReplaceAll(caption, "\n", " ")))
	}
	return a.publish(ctx, http.MethodPut, to.Conversation, data, h)
}
