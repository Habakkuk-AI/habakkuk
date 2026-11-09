// Package twitch is the Twitch chat adapter: one bot account on Twitch's
// IRC interface (irc.chat.twitch.tv over TLS, logged in with an OAuth user
// token), in the channels it joins. Chat is public, so a message is for
// Habakkuk only when it names the bot ("@habakkuk_bot" or "habakkuk_bot:").
package twitch

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

const (
	DefaultServer = "irc.chat.twitch.tv:6697"
	maxMessage    = 500 // characters in one chat message
	pingEvery     = time.Minute
	// lineGap keeps a bot that is not a moderator under Twitch's 20
	// messages per 30 seconds.
	lineGap = 1600 * time.Millisecond
)

type Adapter struct {
	Server    string // host:port, default DefaultServer
	TLS       bool
	Login     string // the bot account's login name
	Token     string // OAuth user token with chat:read and chat:edit
	Channels  []string
	TLSConfig *tls.Config
	Gap       time.Duration // between messages; default lineGap

	mu       sync.Mutex
	w        *bufio.Writer
	lastSent time.Time
}

func New(server, login, token string, channels []string) *Adapter {
	if server == "" {
		server = DefaultServer
	}
	norm := make([]string, 0, len(channels))
	for _, c := range channels {
		norm = append(norm, "#"+strings.ToLower(strings.TrimPrefix(c, "#")))
	}
	return &Adapter{Server: server, TLS: true, Login: strings.ToLower(login), Token: token, Channels: norm, Gap: lineGap}
}

func (a *Adapter) MaxLen() int               { return maxMessage }
func (a *Adapter) Format(md string) string   { return channel.FormatPlain(md) }
func (a *Adapter) StaleAfter() time.Duration { return 3 * pingEvery }

type line struct {
	tags    map[string]string
	nick    string
	command string
	params  []string
}

// parse reads one IRC line with IRCv3 tags.
func parse(s string) line {
	var l line
	if strings.HasPrefix(s, "@") {
		var tags string
		tags, s, _ = strings.Cut(s[1:], " ")
		l.tags = map[string]string{}
		for _, kv := range strings.Split(tags, ";") {
			k, v, _ := strings.Cut(kv, "=")
			l.tags[k] = unescape(v)
		}
	}
	if strings.HasPrefix(s, ":") {
		var prefix string
		prefix, s, _ = strings.Cut(s[1:], " ")
		l.nick, _, _ = strings.Cut(prefix, "!")
	}
	s, trailing, hasTrailing := strings.Cut(s, " :")
	fields := strings.Fields(s)
	if len(fields) > 0 {
		l.command, l.params = fields[0], fields[1:]
	}
	if hasTrailing {
		l.params = append(l.params, trailing)
	}
	return l
}

func unescape(v string) string {
	return strings.NewReplacer(`\s`, " ", `\:`, ";", `\\`, `\`, `\r`, "\r", `\n`, "\n").Replace(v)
}

func (l line) param(i int) string {
	if i < len(l.params) {
		return l.params[i]
	}
	return ""
}

func (a *Adapter) dial(ctx context.Context) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: time.Minute}
	if !a.TLS {
		return d.DialContext(ctx, "tcp", a.Server)
	}
	cfg := &tls.Config{}
	if a.TLSConfig != nil {
		cfg = a.TLSConfig.Clone()
	}
	if cfg.ServerName == "" {
		cfg.ServerName, _, _ = net.SplitHostPort(a.Server)
	}
	return (&tls.Dialer{NetDialer: d, Config: cfg}).DialContext(ctx, "tcp", a.Server)
}

func (a *Adapter) write(s string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.w == nil {
		return errors.New("twitch: not connected")
	}
	if _, err := a.w.WriteString(s + "\r\n"); err != nil {
		return err
	}
	return a.w.Flush()
}

// Run logs in, joins the channels and reads chat until the connection
// fails or Twitch asks for a reconnect.
func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	if a.Token == "" {
		return channel.Fatal{Err: errors.New("twitch: the token variable is empty")}
	}
	conn, err := a.dial(ctx)
	if err != nil {
		return fmt.Errorf("twitch: %w", err)
	}
	defer func() { _ = conn.Close() }()
	parent := ctx
	ctx, cancel := context.WithCancel(ctx) // ends the pinger with the run
	defer cancel()
	stop := context.AfterFunc(parent, func() { _ = conn.Close() })
	defer stop()
	a.mu.Lock()
	a.w = bufio.NewWriter(conn)
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.w = nil
		a.mu.Unlock()
	}()
	pass := a.Token
	if !strings.HasPrefix(pass, "oauth:") {
		pass = "oauth:" + pass
	}
	for _, s := range []string{"CAP REQ :twitch.tv/tags twitch.tv/commands", "PASS " + pass, "NICK " + a.Login} {
		if err := a.write(s); err != nil {
			return fmt.Errorf("twitch: %w", err)
		}
	}
	go func() {
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if a.write("PING :tmi.twitch.tv") != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 16<<10), 64<<10)
	for sc.Scan() {
		in.Beat()
		l := parse(strings.TrimRight(sc.Text(), "\r"))
		switch l.command {
		case "PING":
			if err := a.write("PONG :" + l.param(0)); err != nil {
				return err
			}
		case "001":
			if len(a.Channels) > 0 {
				if err := a.write("JOIN " + strings.Join(a.Channels, ",")); err != nil {
					return err
				}
			}
			in.Connected("Twitch " + a.Login + " in " + strings.Join(a.Channels, ", "))
		case "NOTICE":
			if msg := l.param(1); strings.Contains(msg, "authentication failed") || strings.Contains(msg, "Improperly formatted auth") {
				return channel.Fatal{Err: fmt.Errorf("twitch: %s (check the token and login)", msg)}
			}
		case "RECONNECT":
			return errors.New("twitch: the server asked for a reconnect")
		case "PRIVMSG":
			if m, ok := a.convert(l); ok {
				in.Deliver(parent, m)
			}
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("twitch: %w", err)
	}
	return errors.New("twitch: connection closed")
}

func (a *Adapter) convert(l line) (channel.Message, bool) {
	target, text := l.param(0), l.param(1)
	login := strings.ToLower(l.nick)
	if !strings.HasPrefix(target, "#") || login == "" || login == a.Login || l.tags["id"] == "" {
		return channel.Message{}, false
	}
	sender := l.tags["user-id"]
	if sender == "" {
		sender = login
	}
	m := channel.Message{ID: l.tags["id"], Conversation: target, Sender: sender, SenderAlt: login,
		SenderName: l.tags["display-name"]}
	lower := strings.ToLower(text)
	switch {
	case strings.HasPrefix(lower, a.Login+":") || strings.HasPrefix(lower, a.Login+","):
		m.Mentioned, text = true, text[len(a.Login)+1:]
	case strings.Contains(lower, "@"+a.Login):
		i := strings.Index(lower, "@"+a.Login)
		m.Mentioned, text = true, text[:i]+text[i+1+len(a.Login):]
	}
	m.Text = strings.TrimSpace(text)
	return m, m.Text != ""
}

// Send posts the text line by line, each cut to 500 characters, as a
// threaded reply to replyTo when given, paced under Twitch's rate limit.
func (a *Adapter) Send(ctx context.Context, to channel.Address, text, replyTo string) error {
	target := to.Conversation
	if !strings.HasPrefix(target, "#") || strings.ContainsAny(target, " \r\n,") {
		return channel.Undeliverable{Err: fmt.Errorf("twitch: bad channel %q", target)}
	}
	prefix := ""
	if replyTo != "" && !strings.ContainsAny(replyTo, " ;\r\n") {
		prefix = "@reply-parent-msg-id=" + replyTo + " "
	}
	for _, l := range strings.Split(text, "\n") {
		for _, chunk := range split(strings.TrimSpace(l), maxMessage) {
			if err := a.paced(ctx, prefix+"PRIVMSG "+target+" :"+chunk); err != nil {
				return err
			}
			prefix = "" // only the first line is the reply
		}
	}
	return nil
}

func (a *Adapter) paced(ctx context.Context, s string) error {
	a.mu.Lock()
	wait := time.Until(a.lastSent.Add(a.Gap))
	a.lastSent = time.Now().Add(max(wait, 0))
	a.mu.Unlock()
	if wait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return a.write(s)
}

// split cuts s into pieces of at most n characters, at spaces where it can.
func split(s string, n int) []string {
	var out []string
	for utf8.RuneCountInString(s) > n {
		r := []rune(s)
		cut := n
		for i := n; i > n/2; i-- {
			if r[i] == ' ' {
				cut = i
				break
			}
		}
		out = append(out, string(r[:cut]))
		s = strings.TrimLeft(string(r[cut:]), " ")
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}
