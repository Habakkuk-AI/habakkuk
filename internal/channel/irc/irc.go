// Package irc is the IRC adapter: one nick on one network over TLS, with
// SASL PLAIN when a password is set. In channels Habakkuk answers lines that
// address it by nick; private messages always reach it.
package irc

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
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
	pingEvery = time.Minute
	lineGap   = 400 * time.Millisecond // between lines sent, so servers do not flood-kick
	// maxPayload leaves room in IRC's 512-byte line for the command, the
	// target and the prefix the server adds when relaying.
	maxPayload = 400
)

type Adapter struct {
	Server   string // host:port
	TLS      bool
	Nick     string
	Username string
	SASLUser string
	Password string
	Channels []string
	// TLSConfig overrides the default (system roots, server name from
	// Server); tests use it.
	TLSConfig *tls.Config

	mu       sync.Mutex
	w        *bufio.Writer
	conn     net.Conn
	nick     string // the nick in use, which may differ from Nick
	lastSent time.Time
}

func New(server string, useTLS bool, nick, username, saslUser, password string, channels []string) *Adapter {
	return &Adapter{Server: server, TLS: useTLS, Nick: nick, Username: username, SASLUser: saslUser,
		Password: password, Channels: channels}
}

func (a *Adapter) MaxLen() int               { return 2000 }
func (a *Adapter) Format(md string) string   { return channel.FormatPlain(md) }
func (a *Adapter) StaleAfter() time.Duration { return 3 * pingEvery }

// line is one parsed IRC message.
type line struct {
	tags    map[string]string
	nick    string // from the prefix
	command string
	params  []string // the trailing parameter is the last
}

func parse(s string) line {
	var l line
	if strings.HasPrefix(s, "@") {
		var raw string
		raw, s, _ = strings.Cut(s[1:], " ")
		l.tags = map[string]string{}
		for _, kv := range strings.Split(raw, ";") {
			k, v, _ := strings.Cut(kv, "=")
			l.tags[k] = unescapeTag(v)
		}
	}
	if strings.HasPrefix(s, ":") {
		var prefix string
		prefix, s, _ = strings.Cut(s[1:], " ")
		l.nick, _, _ = strings.Cut(prefix, "!")
	}
	for s != "" {
		if strings.HasPrefix(s, ":") {
			l.params = append(l.params, s[1:])
			break
		}
		var p string
		p, s, _ = strings.Cut(s, " ")
		if p == "" {
			continue
		}
		if l.command == "" {
			l.command = strings.ToUpper(p)
		} else {
			l.params = append(l.params, p)
		}
	}
	return l
}

func unescapeTag(v string) string {
	return strings.NewReplacer(`\:`, ";", `\s`, " ", `\\`, `\`, `\r`, "\r", `\n`, "\n").Replace(v)
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

// write sends one raw line now, outside the pacing; registration and pongs
// use it.
func (a *Adapter) write(s string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.w == nil {
		return errors.New("irc: not connected")
	}
	if _, err := a.w.WriteString(s + "\r\n"); err != nil {
		return err
	}
	return a.w.Flush()
}

func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	conn, err := a.dial(ctx)
	if err != nil {
		return fmt.Errorf("irc %s: %w", a.Server, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	a.mu.Lock()
	a.conn, a.w, a.nick = conn, bufio.NewWriter(conn), a.Nick
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.conn == conn {
			a.conn, a.w = nil, nil
		}
		a.mu.Unlock()
		_ = conn.Close()
	}()

	for _, l := range []string{"CAP LS 302", "NICK " + a.Nick, "USER " + a.Username + " 0 * :Habakkuk"} {
		if err := a.write(l); err != nil {
			return err
		}
	}
	pingCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-t.C:
				if a.write("PING :habakkuk") != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()

	r := bufio.NewReaderSize(conn, 8192)
	registered, caps := false, map[string]bool{}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(3 * pingEvery))
		raw, err := r.ReadString('\n')
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("irc %s: %w", a.Server, err)
		}
		in.Beat()
		l := parse(strings.TrimRight(raw, "\r\n"))
		switch l.command {
		case "PING":
			if err := a.write("PONG :" + l.param(0)); err != nil {
				return err
			}
		case "CAP":
			if err := a.cap(l, caps); err != nil {
				return err
			}
		case "AUTHENTICATE":
			if l.param(0) == "+" {
				creds := base64.StdEncoding.EncodeToString([]byte(a.SASLUser + "\x00" + a.SASLUser + "\x00" + a.Password))
				if err := a.write("AUTHENTICATE " + creds); err != nil {
					return err
				}
			}
		case "903": // SASL succeeded
			if err := a.write("CAP END"); err != nil {
				return err
			}
		case "902", "904", "905", "906":
			return channel.Fatal{Err: fmt.Errorf("irc: SASL login as %s failed: %s", a.SASLUser, l.param(len(l.params)-1))}
		case "433", "432": // nick taken or refused
			if registered {
				continue
			}
			a.mu.Lock()
			a.nick += "_"
			n := a.nick
			a.mu.Unlock()
			if len(n) > 30 {
				return channel.Fatal{Err: fmt.Errorf("irc: nick %s is taken", a.Nick)}
			}
			if err := a.write("NICK " + n); err != nil {
				return err
			}
		case "001":
			registered = true
			a.mu.Lock()
			if n := l.param(0); n != "" {
				a.nick = n
			}
			nick := a.nick
			a.mu.Unlock()
			for _, ch := range a.Channels {
				if err := a.write("JOIN " + ch); err != nil {
					return err
				}
			}
			in.Connected(nick + " on " + a.Server)
		case "465", "ERROR":
			e := fmt.Errorf("irc %s: %s %s", a.Server, l.command, l.param(len(l.params)-1))
			if l.command == "465" {
				return channel.Fatal{Err: e}
			}
			return e
		case "NICK":
			a.mu.Lock()
			if strings.EqualFold(l.nick, a.nick) {
				a.nick = l.param(0)
			}
			a.mu.Unlock()
		case "PRIVMSG":
			if m, ok := a.convert(l); ok && registered {
				in.Deliver(ctx, m)
			}
		}
	}
}

// cap negotiates capabilities: SASL when a password is set, and
// account-tag so senders logged in to services carry their account.
func (a *Adapter) cap(l line, caps map[string]bool) error {
	switch strings.ToUpper(l.param(1)) {
	case "LS":
		for _, c := range strings.Fields(l.param(len(l.params) - 1)) {
			name, _, _ := strings.Cut(c, "=")
			caps[name] = true
		}
		if len(l.params) > 3 && l.params[2] == "*" {
			return nil // more LS lines follow
		}
		var want []string
		if caps["sasl"] && a.Password != "" {
			want = append(want, "sasl")
		}
		if caps["account-tag"] {
			want = append(want, "account-tag")
		}
		if a.Password != "" && !caps["sasl"] {
			return channel.Fatal{Err: fmt.Errorf("irc: %s does not offer SASL, which a password needs", a.Server)}
		}
		if len(want) == 0 {
			return a.write("CAP END")
		}
		return a.write("CAP REQ :" + strings.Join(want, " "))
	case "ACK":
		if strings.Contains(" "+l.param(len(l.params)-1)+" ", " sasl ") {
			return a.write("AUTHENTICATE PLAIN")
		}
		return a.write("CAP END")
	case "NAK":
		if a.Password != "" {
			return channel.Fatal{Err: errors.New("irc: the server refused SASL")}
		}
		return a.write("CAP END")
	}
	return nil
}

func (a *Adapter) convert(l line) (channel.Message, bool) {
	target, text := l.param(0), l.param(1)
	if l.nick == "" || target == "" || strings.HasPrefix(text, "\x01") { // CTCP, including /me
		return channel.Message{}, false
	}
	a.mu.Lock()
	nick := a.nick
	a.mu.Unlock()
	if strings.EqualFold(l.nick, nick) {
		return channel.Message{}, false
	}
	m := channel.Message{Sender: l.nick, SenderName: l.nick, Direct: strings.EqualFold(target, nick)}
	if acct := l.tags["account"]; acct != "" && acct != "*" {
		m.SenderAlt = acct
	}
	if m.Direct {
		m.Conversation = l.nick
	} else {
		m.Conversation = strings.ToLower(target)
		lower := strings.ToLower(text)
		ln := strings.ToLower(nick)
		switch {
		case strings.HasPrefix(lower, ln+":"), strings.HasPrefix(lower, ln+","):
			m.Mentioned, text = true, text[len(nick)+1:]
		case wordIn(lower, ln):
			m.Mentioned = true
		}
	}
	m.Text = strings.TrimSpace(text)
	m.ID = l.tags["msgid"]
	if m.ID == "" {
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		m.ID = hex.EncodeToString(b) // IRC never redelivers, so any unique id works
	}
	return m, m.Text != ""
}

func wordIn(s, w string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(w)
		before := start == 0 || !isNickChar(s[start-1])
		after := end == len(s) || !isNickChar(s[end])
		if before && after {
			return true
		}
		i = start + 1
	}
}

func isNickChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-_[]\\`^{}|", c) >= 0
}

// Send writes the text as PRIVMSG lines, each cut to fit IRC's line limit
// at a space where it can, paced so the server does not flood-kick.
func (a *Adapter) Send(ctx context.Context, to channel.Address, text, _ string) error {
	target := to.Conversation
	if target == "" || strings.ContainsAny(target, " \r\n,") {
		return channel.Undeliverable{Err: fmt.Errorf("irc: bad target %q", target)}
	}
	for _, l := range strings.Split(text, "\n") {
		for _, chunk := range splitLine(strings.TrimRight(l, "\r"), maxPayload-len(target)) {
			if err := a.paced(ctx, "PRIVMSG "+target+" :"+chunk); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *Adapter) paced(ctx context.Context, s string) error {
	a.mu.Lock()
	wait := time.Until(a.lastSent.Add(lineGap))
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

// splitLine cuts s into pieces of at most n bytes, at spaces where it can
// and never inside a UTF-8 character. Blank lines are dropped.
func splitLine(s string, n int) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for len(s) > n {
		cut := strings.LastIndexByte(s[:n], ' ')
		if cut <= n/2 {
			cut = n
			for cut > 0 && !utf8.RuneStart(s[cut]) {
				cut--
			}
		}
		out = append(out, s[:cut])
		s = strings.TrimLeft(s[cut:], " ")
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}
