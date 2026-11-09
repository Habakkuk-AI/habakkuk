// Package habakkukapi is a small client for the parts of Habakkuk's HTTP API
// (docs/reference/api.md) the desktop app uses on its own: the overview, approvals,
// the web chat's conversations and their event streams.
package habakkukapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to one Habakkuk.
type Client struct {
	BaseURL string // e.g. http://127.0.0.1:8484, no trailing slash
	Token   string
	HTTP    *http.Client // default: one without a timeout (streams are long)
}

// StatusError is a non-2xx answer.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Code, e.Msg)
	}
	return fmt.Sprintf("HTTP %d", e.Code)
}

// IsUnauthorized reports whether err is a 401 (a missing or wrong token).
func IsUnauthorized(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusUnauthorized
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.BaseURL, "/")+path, rd)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		defer res.Body.Close()
		var e struct {
			Error string `json:"error"`
		}
		data, _ := io.ReadAll(io.LimitReader(res.Body, 64*1024))
		_ = json.Unmarshal(data, &e)
		return nil, &StatusError{Code: res.StatusCode, Msg: e.Error}
	}
	return res, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if out == nil || res.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(out)
}

// AppInfo is GET /api/app.
type AppInfo struct {
	Account  string   `json:"account"`
	Agents   []string `json:"agents"`
	Default  string   `json:"default"`
	Timezone string   `json:"timezone"`
}

func (c *Client) App(ctx context.Context) (AppInfo, error) {
	var a AppInfo
	return a, c.do(ctx, http.MethodGet, "/api/app", nil, &a)
}

// Channel is one account in the overview.
type Channel struct {
	Account   string `json:"account"`
	Type      string `json:"type"`
	State     string `json:"state"`
	LastError string `json:"last_error"`
}

// Overview is GET /api/overview (the fields the app shows).
type Overview struct {
	Channels []Channel `json:"channels"`
	Queue    struct {
		Pending int `json:"pending"`
		Running int `json:"running"`
	} `json:"queue"`
	Approvals int `json:"approvals"`
	OpenCards int `json:"open_cards"`
}

func (c *Client) Overview(ctx context.Context) (Overview, error) {
	var o Overview
	return o, c.do(ctx, http.MethodGet, "/api/overview", nil, &o)
}

// Approval is one tool call waiting for a yes or no.
type Approval struct {
	ID string `json:"id"`
	To struct {
		Account      string `json:"account"`
		Conversation string `json:"conversation"`
		Thread       string `json:"thread"`
	} `json:"to"`
	Session string    `json:"session"`
	Tool    string    `json:"tool"`
	Summary string    `json:"summary"`
	Review  string    `json:"review"`
	Since   time.Time `json:"since"`
}

func (c *Client) Approvals(ctx context.Context) ([]Approval, error) {
	var out struct {
		Approvals []Approval `json:"approvals"`
	}
	return out.Approvals, c.do(ctx, http.MethodGet, "/api/approvals", nil, &out)
}

// Answer allows or denies a waiting call.
func (c *Client) Answer(ctx context.Context, id string, approve bool) error {
	return c.do(ctx, http.MethodPost, "/api/approvals/"+url.PathEscape(id), map[string]bool{"approve": approve}, nil)
}

// Conversation is one web chat conversation.
type Conversation struct {
	Conversation string    `json:"conversation"`
	Agent        string    `json:"agent"`
	Session      string    `json:"session"`
	Title        string    `json:"title"`
	LastAt       time.Time `json:"last_at"`
}

func (c *Client) Conversations(ctx context.Context, limit int) ([]Conversation, error) {
	var out struct {
		Conversations []Conversation `json:"conversations"`
	}
	return out.Conversations, c.do(ctx, http.MethodGet, "/api/chat/conversations?limit="+strconv.Itoa(limit), nil, &out)
}

// Send posts a message to a web conversation.
func (c *Client) Send(ctx context.Context, conversation, agent, text string) error {
	body := map[string]string{"conversation": conversation, "text": text, "agent": agent}
	return c.do(ctx, http.MethodPost, "/api/chat", body, nil)
}

// Message is one event of a conversation's stream.
type Message struct {
	Seq  int64     `json:"seq"`
	Role string    `json:"role"` // user | assistant
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// Follow streams a conversation's messages after seq until ctx ends or the
// stream breaks, calling fn for each. It returns the last sequence number
// seen, to resume from.
func (c *Client) Follow(ctx context.Context, conversation string, after int64, fn func(Message)) (int64, error) {
	res, err := c.request(ctx, http.MethodGet,
		"/api/chat/"+url.PathEscape(conversation)+"/events?after="+strconv.FormatInt(after, 10), nil)
	if err != nil {
		return after, err
	}
	defer res.Body.Close()
	err = ReadEvents(res.Body, func(e Event) {
		if e.Name != "message" {
			return
		}
		var m Message
		if json.Unmarshal([]byte(e.Data), &m) != nil {
			return
		}
		if m.Seq > after {
			after = m.Seq
		}
		fn(m)
	})
	if ctx.Err() != nil {
		return after, ctx.Err()
	}
	return after, err
}

// Event is one Server-Sent Event.
type Event struct {
	ID   string
	Name string // "message" when the stream gives none
	Data string
}

// ReadEvents parses a text/event-stream body until it ends, calling fn for
// each complete event. Comments and retry lines are skipped.
func ReadEvents(r io.Reader, fn func(Event)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	var e Event
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if len(data) > 0 {
				e.Data = strings.Join(data, "\n")
				if e.Name == "" {
					e.Name = "message"
				}
				fn(e)
			}
			e, data = Event{}, nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			e.Name = value
		case "data":
			data = append(data, value)
		case "id":
			e.ID = value
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF // the server never ends a stream on purpose
}
