package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HTTPCommands are the commands an HTTP remote runs. Everything else needs
// an SSH remote.
var HTTPCommands = []string{"status", "chat"}

// Client talks to a Habakkuk server's HTTP API (docs/reference/api.md).
type Client struct {
	Base  *url.URL
	Token string
	HTTP  *http.Client // nil: a client without a timeout (event streams run long)

	// ReplyWait bounds how long chat waits, after its input ends, for the
	// reply to the last message. Zero means ten minutes.
	ReplyWait time.Duration
	// Retry is the pause before an event stream reconnects. Zero means 2s.
	Retry time.Duration
}

// NewClient builds a client for an HTTP remote. The token comes from the
// variable the remote's TokenEnv names, read with getenv.
func NewClient(rem Remote, getenv func(string) string) (*Client, error) {
	t, err := ParseTarget(rem.Target)
	if err != nil {
		return nil, err
	}
	if t.Kind != HTTP {
		return nil, fmt.Errorf("%s is not an http remote", rem.Target)
	}
	c := &Client{Base: t.URL}
	if rem.TokenEnv != "" {
		c.Token = getenv(rem.TokenEnv)
		if c.Token == "" {
			return nil, fmt.Errorf("set %s to the server's token (the value of its http.token_env)", rem.TokenEnv)
		}
	}
	return c, nil
}

// RunHTTP runs one command on an HTTP remote: status or chat. interactive
// says whether the input is a person at a terminal (chat shows a prompt).
func RunHTTP(ctx context.Context, name string, rem Remote, args []string, std Stdio, interactive bool) (int, error) {
	if len(args) == 0 {
		return 2, errors.New("no command")
	}
	switch args[0] {
	case "status", "chat":
	case "tui":
		return 2, fmt.Errorf("tui needs an ssh remote; %s is an http remote: hk -r %s chat is a line-by-line chat", name, name)
	default:
		return 2, fmt.Errorf("%s needs an ssh remote; %s is an http remote, which runs only %s",
			args[0], name, strings.Join(HTTPCommands, " and "))
	}
	c, err := NewClient(rem, os.Getenv)
	if err != nil {
		return 1, err
	}
	switch args[0] {
	case "status":
		if len(args) > 1 {
			return 2, errors.New("usage: hk -r NAME status")
		}
		err = c.Status(ctx, std.Out)
	case "chat":
		var opts ChatOptions
		if opts, err = parseChat(args[1:], std.Err); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0, nil
			}
			return 2, err
		}
		opts.Interactive = interactive
		err = c.Chat(ctx, opts, std)
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// ChatOptions are the flags of chat on an HTTP remote.
type ChatOptions struct {
	Agent        string
	Conversation string
	Interactive  bool
}

func parseChat(args []string, errOut io.Writer) (ChatOptions, error) {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var o ChatOptions
	fs.StringVar(&o.Agent, "agent", "", "agent to talk to (a conversation keeps its agent)")
	fs.StringVar(&o.Conversation, "conversation", "", "web chat conversation id (default cli-<this host>)")
	fs.Bool("plain", true, "always on: an http remote chats line by line")
	session := fs.String("session", "", "not available on an http remote")
	resume := fs.Bool("resume", false, "not available on an http remote")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if *session != "" || *resume {
		return o, errors.New("-session and -resume need an ssh remote; an http remote chats in a web conversation (-conversation ID)")
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("chat takes no arguments, got %q", fs.Arg(0))
	}
	if o.Conversation == "" {
		o.Conversation = DefaultConversation()
	}
	if !conversationRE.MatchString(o.Conversation) {
		return o, fmt.Errorf("-conversation %q: use 1-128 letters, digits, '.', '_', ':' or '-'", o.Conversation)
	}
	return o, nil
}

var conversationRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// DefaultConversation is cli-<hostname>: one web conversation per machine,
// so a chat picks up where it left off.
func DefaultConversation() string {
	host, _ := os.Hostname()
	var b strings.Builder
	for _, r := range strings.ToLower(host) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	h := strings.Trim(b.String(), "-.")
	if h == "" {
		h = "terminal"
	}
	if len(h) > 100 {
		h = h[:100]
	}
	return "cli-" + h
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) url(path string, q url.Values) string {
	u := *c.Base
	u.Path = c.Base.Path + path
	u.RawQuery = q.Encode()
	return u.String()
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path, q), rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, apiError(resp, c.Token != "")
	}
	return resp, nil
}

func apiError(resp *http.Response, sentToken bool) error {
	var e struct {
		Error string `json:"error"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = json.Unmarshal(data, &e)
	switch {
	case resp.StatusCode == http.StatusUnauthorized && sentToken:
		return errors.New("the server refused the token: check the variable -token-env names")
	case resp.StatusCode == http.StatusUnauthorized:
		return errors.New("the server needs a token: add the remote again with -token-env NAME and set NAME")
	case e.Error != "":
		return fmt.Errorf("%s (HTTP %d)", e.Error, resp.StatusCode)
	}
	return fmt.Errorf("HTTP %s", resp.Status)
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

type overview struct {
	Channels []struct {
		Account   string `json:"account"`
		Type      string `json:"type"`
		State     string `json:"state"`
		Detail    string `json:"detail"`
		LastError string `json:"last_error"`
	} `json:"channels"`
	Queue struct {
		Pending int `json:"pending"`
		Running int `json:"running"`
	} `json:"queue"`
	Errors []struct {
		Kind  string `json:"kind"`
		Agent string `json:"agent"`
		Error string `json:"error"`
	} `json:"errors"`
	Approvals int  `json:"approvals"`
	OpenCards *int `json:"open_cards"`
	Schedules *struct {
		Total   int `json:"total"`
		Enabled int `json:"enabled"`
	} `json:"schedules"`
}

// Status prints the server's health and overview (GET /healthz and
// /api/overview).
func (c *Client) Status(ctx context.Context, w io.Writer) error {
	var health struct {
		Status string `json:"status"`
	}
	if err := c.getJSON(ctx, "/healthz", &health); err != nil {
		return fmt.Errorf("health check: %w", err)
	}
	var ov overview
	if err := c.getJSON(ctx, "/api/overview", &ov); err != nil {
		return err
	}
	fmt.Fprintf(w, "Server:    %s\n", c.Base.Redacted())
	fmt.Fprintf(w, "Health:    %s\n", health.Status)
	fmt.Fprintf(w, "Queue:     %d pending, %d running\n", ov.Queue.Pending, ov.Queue.Running)
	fmt.Fprintf(w, "Approvals: %d waiting\n", ov.Approvals)
	if ov.OpenCards != nil {
		fmt.Fprintf(w, "Board:     %d open cards\n", *ov.OpenCards)
	}
	if ov.Schedules != nil {
		fmt.Fprintf(w, "Schedules: %d of %d enabled\n", ov.Schedules.Enabled, ov.Schedules.Total)
	}
	if len(ov.Channels) > 0 {
		fmt.Fprintln(w, "Channels:")
		for _, ch := range ov.Channels {
			line := fmt.Sprintf("  %s (%s): %s", ch.Account, ch.Type, ch.State)
			if ch.Detail != "" {
				line += ", " + ch.Detail
			}
			if ch.LastError != "" {
				line += "; last error: " + firstLine(ch.LastError)
			}
			fmt.Fprintln(w, line)
		}
	}
	fmt.Fprintf(w, "Errors in the last 7 days: %d\n", len(ov.Errors))
	for _, e := range ov.Errors {
		fmt.Fprintf(w, "  %s %s: %s\n", e.Kind, e.Agent, firstLine(e.Error))
	}
	return nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// event is one message of a web chat conversation.
type event struct {
	Seq  int64     `json:"seq"`
	Role string    `json:"role"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// Chat is a line-by-line chat in a web conversation: each line goes to
// POST /api/chat, and the agent's messages arrive on the conversation's
// event stream. When the input ends, it waits for the replies to the
// messages it sent, so `echo hi | hk -r web chat` prints one answer.
func (c *Client) Chat(ctx context.Context, o ChatOptions, std Stdio) error {
	ctx, cancel := context.WithCancel(ctx)
	ch := newChatState(std.Out, o.Interactive)
	connected := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); c.follow(ctx, o.Conversation, ch, connected, std.Err) }()
	defer wg.Wait() // nothing prints after Chat returns
	defer cancel()
	select {
	case err := <-connected:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	if o.Interactive {
		fmt.Fprintf(std.Out, "Conversation %s. Type /exit to quit; other /commands go to the agent.\n", o.Conversation)
	}
	lines := make(chan string)
	readErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(std.In)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
		readErr <- sc.Err()
	}()
	for {
		ch.showPrompt()
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			if err != nil {
				return err
			}
			return ch.waitReplies(ctx, c.replyWait())
		case line := <-lines:
			text := strings.TrimSpace(line)
			switch text {
			case "":
				continue
			case "/exit", "/quit":
				return nil
			}
			if !o.Interactive {
				// Piped lines are turns one after another, as in a local
				// chat, so each reply follows its question.
				if err := ch.waitReplies(ctx, c.replyWait()); err != nil {
					return err
				}
			}
			body := map[string]string{"conversation": o.Conversation, "text": text}
			if o.Agent != "" {
				body["agent"] = o.Agent
			}
			ch.expect(text, 1) // before posting: the stream may show it first
			pctx, pcancel := context.WithTimeout(ctx, 30*time.Second)
			resp, err := c.do(pctx, http.MethodPost, "/api/chat", nil, body)
			pcancel()
			if err != nil {
				ch.expect(text, -1)
				ch.printErr(std.Err, err)
				continue
			}
			resp.Body.Close()
		}
	}
}

func (c *Client) replyWait() time.Duration {
	if c.ReplyWait > 0 {
		return c.ReplyWait
	}
	return 10 * time.Minute
}

// chatState prints the agent's messages and the prompt without mixing
// them, and tracks whether the messages sent have been answered: each one
// is answered once the stream has shown it and an agent message after it.
type chatState struct {
	mu       sync.Mutex
	out      io.Writer
	prompt   bool
	sent     map[string]int // posted, not yet seen on the stream
	awaiting bool           // a message of ours was seen, no agent message since
	changed  chan struct{}  // closed and replaced on every change
}

func newChatState(out io.Writer, prompt bool) *chatState {
	return &chatState{out: out, prompt: prompt, sent: map[string]int{}, changed: make(chan struct{})}
}

func (s *chatState) showPrompt() {
	if !s.prompt {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprint(s.out, "> ")
}

func (s *chatState) printErr(w io.Writer, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintln(w, "error:", err)
}

func (s *chatState) expect(text string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sent[text] += n; s.sent[text] <= 0 {
		delete(s.sent, text)
	}
	s.notify()
}

// notify wakes waitReplies; s.mu is held.
func (s *chatState) notify() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// seen takes one event from the stream that is not history.
func (s *chatState) seen(e event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e.Role {
	case "user":
		if s.sent[e.Text] > 0 {
			if s.sent[e.Text]--; s.sent[e.Text] == 0 {
				delete(s.sent, e.Text)
			}
			s.awaiting = true
		}
	case "assistant":
		s.awaiting = false
		if s.prompt {
			fmt.Fprint(s.out, "\r")
		}
		fmt.Fprintln(s.out, strings.TrimRight(e.Text, "\n"))
		if s.prompt {
			fmt.Fprint(s.out, "> ")
		}
	}
	s.notify()
}

func (s *chatState) waitReplies(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		s.mu.Lock()
		done, changed := len(s.sent) == 0 && !s.awaiting, s.changed
		s.mu.Unlock()
		if done {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil
		case <-t.C:
			return errors.New("no reply yet; it will be in the conversation")
		}
	}
}

// follow reads the conversation's event stream and prints the agent's
// messages, reconnecting after a break. Messages from before the stream
// first opened (by the server's clock) are history and are not shown.
func (c *Client) follow(ctx context.Context, conv string, ch *chatState, connected chan<- error, errOut io.Writer) {
	var after int64
	var since time.Time
	first := true
	for ctx.Err() == nil {
		resp, err := c.do(ctx, http.MethodGet, "/api/chat/"+url.PathEscape(conv)+"/events", url.Values{"after": {strconv.FormatInt(after, 10)}}, nil)
		if first {
			connected <- err
			if err != nil {
				return
			}
			first = false
			since = time.Now()
			if d, perr := http.ParseTime(resp.Header.Get("Date")); perr == nil {
				since = d
			}
			since = since.Add(-2 * time.Second) // Date has whole seconds
		}
		if err == nil {
			after = readEvents(resp.Body, after, func(e event) {
				if !e.At.Before(since) {
					ch.seen(e)
				}
			})
			resp.Body.Close()
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			fmt.Fprintln(errOut, "chat stream:", err, "(reconnecting)")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.retry()):
		}
	}
}

func (c *Client) retry() time.Duration {
	if c.Retry > 0 {
		return c.Retry
	}
	return 2 * time.Second
}

// readEvents parses a Server-Sent Events stream, calls fn for each message
// event, and returns the last sequence number seen.
func readEvents(r io.Reader, after int64, fn func(event)) int64 {
	br := bufio.NewReaderSize(r, 64*1024)
	var name string
	var data strings.Builder
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "" && err == nil:
			if (name == "message" || name == "") && data.Len() > 0 {
				var e event
				if json.Unmarshal([]byte(data.String()), &e) == nil && e.Seq > after {
					after = e.Seq
					fn(e)
				}
			}
			name = ""
			data.Reset()
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		if err != nil {
			return after
		}
	}
}
