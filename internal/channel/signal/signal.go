// Package signal is the Signal adapter. It talks to a signal-cli daemon in
// HTTP mode (signal-cli -a +NUMBER daemon --http 127.0.0.1:8080): messages
// arrive over the daemon's Server-Sent Events stream and go out through its
// JSON-RPC endpoint. The daemon holds the account's keys; Habakkuk holds none.
package signal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
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
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

// groupPrefix marks a group conversation; anything else is a number or a
// Signal account id.
const groupPrefix = "group."

const maxUpload = 100 << 20

type Adapter struct {
	APIURL  string // the signal-cli daemon, default http://127.0.0.1:8080
	Account string // the bot's number, +15551234567
	// Name addresses the bot in a group when a message starts with it
	// ("habakkuk, what's on today?"), besides a real @mention.
	Name string
	HTTP *http.Client

	ids  atomic.Int64
	mu   sync.Mutex
	uuid string // the bot's own account id, for mentions that carry only it
}

func New(apiURL, account, name string) *Adapter {
	if apiURL == "" {
		apiURL = "http://127.0.0.1:8080"
	}
	return &Adapter{APIURL: strings.TrimRight(apiURL, "/"), Account: account, Name: name,
		HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// MaxLen keeps a message under Signal's 2000-character body; signal-cli
// would otherwise turn it into a text attachment.
func (a *Adapter) MaxLen() int               { return 2000 }
func (a *Adapter) StaleAfter() time.Duration { return 2 * time.Minute } // the daemon sends a keepalive every 15s

// Format leaves Markdown as it is; Send turns it into Signal text styles.
func (a *Adapter) Format(md string) string { return md }

// Check asks the daemon at apiURL whether it is up, for habakkuk doctor.
func Check(ctx context.Context, apiURL string) error {
	a := New(apiURL, "", "")
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.APIURL+"/api/v1/check", nil)
	if err != nil {
		return err
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("signal-cli daemon not reachable at %s: %w", a.APIURL, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("signal-cli daemon at %s: HTTP %d", a.APIURL, resp.StatusCode)
	}
	return nil
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// call makes one JSON-RPC request and classifies failures.
func (a *Adapter) call(ctx context.Context, method string, params map[string]any, out any) error {
	if params == nil {
		params = map[string]any{}
	}
	if a.Account != "" {
		params["account"] = a.Account // required when the daemon serves several accounts
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": a.ids.Add(1), "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.APIURL+"/api/v1/rpc", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := a.HTTP
	if method == "send" || method == "getAttachment" {
		client = &http.Client{Transport: a.HTTP.Transport, Timeout: 5 * time.Minute} // attachments
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("signal %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 150<<20))
	if err != nil {
		return err
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("signal %s: HTTP %d, unreadable body", method, resp.StatusCode)
	}
	if r.Error != nil {
		return classify(method, r.Error)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("signal %s: HTTP %d", method, resp.StatusCode)
	}
	if out != nil && len(r.Result) > 0 {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func classify(method string, e *rpcError) error {
	err := fmt.Errorf("signal %s: %s (%d)", method, e.Message, e.Code)
	var data struct {
		Response sendResult `json:"response"`
	}
	if json.Unmarshal(e.Data, &data) == nil && len(data.Response.Results) > 0 {
		if rerr := data.Response.check(method); rerr != nil {
			return rerr
		}
	}
	msg := strings.ToLower(e.Message)
	switch {
	case strings.Contains(msg, "rate limit"):
		return channel.RetryAfter{Err: err, After: time.Minute}
	case e.Code == -32602, strings.Contains(msg, "unregistered"), strings.Contains(msg, "not registered"),
		strings.Contains(msg, "invalid group"), strings.Contains(msg, "group not found"), strings.Contains(msg, "not a member"):
		return channel.Undeliverable{Err: err}
	}
	return err
}

type sendResult struct {
	Timestamp int64 `json:"timestamp"`
	Results   []struct {
		Type              string `json:"type"`
		RetryAfterSeconds int    `json:"retryAfterSeconds"`
	} `json:"results"`
}

// check fails a send that reached nobody. A group send that reached some
// members succeeded; the rest is between their phones and Signal.
func (r sendResult) check(method string) error {
	if len(r.Results) == 0 {
		return nil
	}
	for _, x := range r.Results {
		if x.Type == "SUCCESS" {
			return nil
		}
	}
	x := r.Results[0]
	err := fmt.Errorf("signal %s: %s", method, x.Type)
	switch x.Type {
	case "RATE_LIMIT_FAILURE", "PROOF_REQUIRED_FAILURE":
		return channel.RetryAfter{Err: err, After: time.Duration(max(x.RetryAfterSeconds, 60)) * time.Second}
	case "UNREGISTERED_FAILURE", "IDENTITY_FAILURE":
		return channel.Undeliverable{Err: err}
	}
	return err
}

func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	var v struct {
		Version string `json:"version"`
	}
	if err := a.call(ctx, "version", nil, &v); err != nil {
		return fmt.Errorf("signal-cli daemon at %s: %w", a.APIURL, err)
	}
	a.learnUUID(ctx)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.APIURL+"/api/v1/events?"+url.Values{"account": {a.Account}}.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{Transport: a.HTTP.Transport}).Do(req) // no timeout: the stream stays open
	if err != nil {
		return fmt.Errorf("signal events: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("signal events: HTTP %d", resp.StatusCode)
	}
	detail := a.Account
	if v.Version != "" {
		detail += " (signal-cli " + v.Version + ")"
	}
	in.Connected(detail)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var data strings.Builder
	for sc.Scan() {
		in.Beat() // events and keepalive comments alike
		line := sc.Text()
		switch {
		case line == "":
			if data.Len() > 0 {
				if m, ok := a.convert(json.RawMessage(data.String())); ok {
					in.Deliver(ctx, m)
				}
				data.Reset()
			}
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("signal events: %w", err)
	}
	return errors.New("signal events: stream ended")
}

// learnUUID finds the bot's own account id; a mention may carry only that.
func (a *Adapter) learnUUID(ctx context.Context) {
	var st []struct {
		Number string `json:"number"`
		UUID   string `json:"uuid"`
	}
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if a.call(lctx, "getUserStatus", map[string]any{"recipient": []string{a.Account}}, &st) == nil {
		for _, s := range st {
			if s.UUID != "" {
				a.mu.Lock()
				a.uuid = s.UUID
				a.mu.Unlock()
			}
		}
	}
}

type mention struct {
	Name   string `json:"name"`
	Number string `json:"number"`
	UUID   string `json:"uuid"`
	Start  int    `json:"start"`
	Length int    `json:"length"`
}

type envelope struct {
	SourceNumber string `json:"sourceNumber"`
	SourceUUID   string `json:"sourceUuid"`
	SourceName   string `json:"sourceName"`
	Timestamp    int64  `json:"timestamp"`
	DataMessage  *struct {
		Message   string `json:"message"`
		GroupInfo *struct {
			GroupID string `json:"groupId"`
		} `json:"groupInfo"`
		Mentions    []mention `json:"mentions"`
		Attachments []struct {
			ContentType string `json:"contentType"`
			Filename    string `json:"filename"`
			ID          string `json:"id"`
			Size        int64  `json:"size"`
		} `json:"attachments"`
		Quote *struct {
			AuthorNumber string `json:"authorNumber"`
			AuthorUUID   string `json:"authorUuid"`
		} `json:"quote"`
		Reaction     json.RawMessage `json:"reaction"`
		RemoteDelete json.RawMessage `json:"remoteDelete"`
	} `json:"dataMessage"`
}

// convert reads one receive event. The stream carries the notification's
// params ({"envelope": ..., "account": ...}); a whole JSON-RPC notification
// is accepted too.
func (a *Adapter) convert(raw json.RawMessage) (channel.Message, bool) {
	var ev struct {
		Envelope *envelope `json:"envelope"`
		Params   *struct {
			Envelope *envelope `json:"envelope"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return channel.Message{}, false
	}
	e := ev.Envelope
	if e == nil && ev.Params != nil {
		e = ev.Params.Envelope
	}
	if e == nil || e.DataMessage == nil || len(e.DataMessage.Reaction) > 0 || len(e.DataMessage.RemoteDelete) > 0 {
		return channel.Message{}, false // receipts, typing, sync messages, reactions
	}
	a.mu.Lock()
	self := a.uuid
	a.mu.Unlock()
	if (e.SourceNumber != "" && e.SourceNumber == a.Account) || (self != "" && e.SourceUUID == self) {
		return channel.Message{}, false
	}
	author := e.SourceNumber
	if author == "" {
		author = e.SourceUUID
	}
	if author == "" {
		return channel.Message{}, false
	}
	d := e.DataMessage
	m := channel.Message{
		ID:         strconv.FormatInt(e.Timestamp, 10) + ":" + author, // quoting needs both
		SenderName: e.SourceName,
		Direct:     d.GroupInfo == nil,
	}
	// Sender is the stable account id; the number, which people write in
	// allow_from, is the alternative (it may be hidden).
	number := strings.TrimPrefix(e.SourceNumber, "+")
	m.Sender, m.SenderAlt = e.SourceUUID, number
	if m.Sender == "" {
		m.Sender, m.SenderAlt = number, ""
	}
	if m.Direct {
		m.Conversation = author
	} else {
		m.Conversation = groupPrefix + d.GroupInfo.GroupID
	}
	isSelf := func(number, uuid string) bool {
		return (number != "" && number == a.Account) || (self != "" && uuid == self)
	}
	text, mentioned := replaceMentions(d.Message, d.Mentions, isSelf)
	if d.Quote != nil && isSelf(d.Quote.AuthorNumber, d.Quote.AuthorUUID) {
		mentioned = true // a reply to the bot
	}
	if rest, ok := cutName(text, a.Name); ok {
		text, mentioned = rest, true
	}
	m.Text, m.Mentioned = strings.TrimSpace(text), mentioned
	for _, att := range d.Attachments {
		id, conv := att.ID, m.Conversation
		name := att.Filename
		if name == "" {
			name = "attachment"
		}
		m.Attachments = append(m.Attachments, channel.Attachment{Name: name, MediaType: att.ContentType, Size: att.Size,
			Fetch: func(ctx context.Context) ([]byte, error) { return a.download(ctx, id, conv) }})
	}
	if m.Text == "" && len(m.Attachments) == 0 {
		return channel.Message{}, false
	}
	return m, true
}

// replaceMentions swaps each mention's placeholder for "@name", dropping the
// bot's own. Offsets count UTF-16 code units.
func replaceMentions(text string, ms []mention, isSelf func(number, uuid string) bool) (string, bool) {
	units := utf16.Encode([]rune(text))
	mentioned := false
	// Back to front, so earlier offsets stay valid.
	for i := len(ms) - 1; i >= 0; i-- {
		mn := ms[i]
		if mn.Start < 0 || mn.Length < 0 || mn.Start+mn.Length > len(units) {
			continue
		}
		repl := "@" + mn.Name
		if isSelf(mn.Number, mn.UUID) {
			mentioned, repl = true, ""
		}
		units = append(units[:mn.Start], append(utf16.Encode([]rune(repl)), units[mn.Start+mn.Length:]...)...)
	}
	return string(utf16.Decode(units)), mentioned
}

// cutName strips a leading "Name," or "Name:" (any case) that addresses the bot.
func cutName(text, name string) (string, bool) {
	t := strings.TrimSpace(text)
	if name == "" || len(t) < len(name) || !strings.EqualFold(t[:len(name)], name) {
		return text, false
	}
	rest := t[len(name):]
	if r, _ := firstRune(rest); r != 0 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
		return text, false // "habakkuks" is not "habakkuk"
	}
	return strings.TrimLeft(rest, " ,:;!-"), true
}

func firstRune(s string) (rune, int) {
	for i, r := range s {
		return r, i
	}
	return 0, 0
}

// target is the recipient or group of a conversation, as send and
// sendTyping take it.
func target(conv string) (map[string]any, error) {
	if g, ok := strings.CutPrefix(conv, groupPrefix); ok && g != "" {
		return map[string]any{"groupId": g}, nil
	}
	if conv == "" || strings.HasPrefix(conv, groupPrefix) {
		return nil, channel.Undeliverable{Err: fmt.Errorf("signal: bad conversation %q", conv)}
	}
	return map[string]any{"recipient": []string{conv}}, nil
}

func (a *Adapter) download(ctx context.Context, id, conv string) ([]byte, error) {
	params, err := target(conv)
	if err != nil {
		return nil, err
	}
	params["id"] = id
	if r, ok := params["recipient"].([]string); ok {
		params["recipient"] = r[0] // getAttachment takes one recipient, not a list
	}
	var res json.RawMessage
	if err := a.call(ctx, "getAttachment", params, &res); err != nil {
		return nil, err
	}
	var b64 string
	if json.Unmarshal(res, &b64) != nil {
		var obj struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal(res, &obj); err != nil {
			return nil, fmt.Errorf("signal getAttachment: unexpected result")
		}
		b64 = obj.Data
	}
	return base64.StdEncoding.DecodeString(b64)
}

func (a *Adapter) Send(ctx context.Context, to channel.Address, text, replyTo string) error {
	params, err := target(to.Conversation)
	if err != nil {
		return err
	}
	plain, styles := styled(text)
	params["message"] = plain
	if len(styles) > 0 {
		params["textStyle"] = styles
	}
	if ts, author, ok := strings.Cut(replyTo, ":"); ok {
		if n, err := strconv.ParseInt(ts, 10, 64); err == nil {
			params["quoteTimestamp"], params["quoteAuthor"] = n, author
		}
	}
	var res sendResult
	if err := a.call(ctx, "send", params, &res); err != nil {
		return err
	}
	return res.check("send")
}

// Typing shows the typing indicator; Signal clears it after 15 seconds.
func (a *Adapter) Typing(ctx context.Context, to channel.Address) error {
	params, err := target(to.Conversation)
	if err != nil {
		return err
	}
	return a.call(ctx, "sendTyping", params, nil)
}

func (a *Adapter) MaxFileSize() int64 { return maxUpload }

// SendFile sends the file as a data URI, so the daemon needs no access to
// Habakkuk's disk.
func (a *Adapter) SendFile(ctx context.Context, to channel.Address, name, mediaType string, data []byte, caption string) error {
	params, err := target(to.Conversation)
	if err != nil {
		return err
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	name = strings.NewReplacer(";", "_", ",", "_").Replace(name)
	params["attachments"] = []string{"data:" + mediaType + ";filename=" + name + ";base64," + base64.StdEncoding.EncodeToString(data)}
	if caption != "" {
		plain, styles := styled(caption)
		params["message"] = plain
		if len(styles) > 0 {
			params["textStyle"] = styles
		}
	}
	var res sendResult
	if err := a.call(ctx, "send", params, &res); err != nil {
		return err
	}
	return res.check("send")
}

var (
	fence    = regexp.MustCompile("^\\s*```")
	heading  = regexp.MustCompile(`^#{1,6}[ \t]+(.+?)[ \t#]*$`)
	bullet   = regexp.MustCompile(`^([ \t]*)[-*][ \t]+`)
	inlineMD = regexp.MustCompile("\\*\\*([^*\\n]+?)\\*\\*|__([^_\\n]+?)__|~~([^~\\n]+?)~~|`([^`\\n]+)`|\\[([^\\]\\n]+)\\]\\(([^)\\s]+)\\)|\\*([^*\\s](?:[^*\\n]*?[^*\\s])?)\\*|_([^_\\s](?:[^_\\n]*?[^_\\s])?)_")
)

// styled turns Markdown into plain text plus Signal text styles
// ("start:length:STYLE", counted in UTF-16 code units). Nested markup keeps
// only its outer style.
func styled(md string) (string, []string) {
	var out strings.Builder
	var styles []string
	pos := 0
	write := func(s string) {
		out.WriteString(s)
		pos += len(utf16.Encode([]rune(s)))
	}
	style := func(start int, name string) {
		if pos > start {
			styles = append(styles, fmt.Sprintf("%d:%d:%s", start, pos-start, name))
		}
	}
	inline := func(s string) {
		last := 0
		for _, m := range inlineMD.FindAllStringSubmatchIndex(s, -1) {
			group := func(i int) string {
				if m[2*i] < 0 {
					return ""
				}
				return s[m[2*i]:m[2*i+1]]
			}
			// _x_ inside a word (snake_case) is not italic.
			if m[16] >= 0 && (wordAt(s, m[0]-1) || wordAt(s, m[1])) {
				continue
			}
			write(s[last:m[0]])
			last = m[1]
			start := pos
			switch {
			case m[2] >= 0 || m[4] >= 0:
				write(group(1) + group(2))
				style(start, "BOLD")
			case m[6] >= 0:
				write(group(3))
				style(start, "STRIKETHROUGH")
			case m[8] >= 0:
				write(group(4))
				style(start, "MONOSPACE")
			case m[10] >= 0:
				write(group(5))
				if group(6) != group(5) {
					write(" (" + group(6) + ")")
				}
			default:
				write(group(7) + group(8))
				style(start, "ITALIC")
			}
		}
		write(s[last:])
	}
	inFence, first := false, true
	for _, line := range strings.Split(md, "\n") {
		if fence.MatchString(line) {
			inFence = !inFence
			continue
		}
		if !first {
			write("\n")
		}
		first = false
		switch {
		case inFence:
			start := pos
			write(line)
			style(start, "MONOSPACE")
		case heading.MatchString(line):
			start := pos
			inline(heading.FindStringSubmatch(line)[1])
			style(start, "BOLD")
		default:
			if b := bullet.FindStringSubmatchIndex(line); b != nil {
				write(line[b[2]:b[3]] + "• ")
				line = line[b[1]:]
			}
			inline(line)
		}
	}
	return out.String(), styles
}

func wordAt(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
