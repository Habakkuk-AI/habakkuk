package irc

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

// fakeIRC is a one-client server: it negotiates CAP and SASL, welcomes the
// client, then sends the scripted lines and records what the client says.
type fakeIRC struct {
	ln       net.Listener
	password string
	script   []string
	mu       sync.Mutex
	got      []string
	conn     net.Conn
}

func newFake(t *testing.T, password string, script ...string) *fakeIRC {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIRC{ln: ln, password: password, script: script}
	t.Cleanup(func() { ln.Close() })
	go f.serve()
	return f
}

func (f *fakeIRC) serve() {
	c, err := f.ln.Accept()
	if err != nil {
		return
	}
	f.mu.Lock()
	f.conn = c
	f.mu.Unlock()
	defer c.Close()
	r := bufio.NewReader(c)
	send := func(s string) { c.Write([]byte(s + "\r\n")) }
	nick := ""
	for {
		raw, err := r.ReadString('\n')
		if err != nil {
			return
		}
		l := strings.TrimRight(raw, "\r\n")
		f.mu.Lock()
		f.got = append(f.got, l)
		f.mu.Unlock()
		p := parse(l)
		switch p.command {
		case "CAP":
			switch p.param(0) {
			case "LS":
				send(":srv CAP * LS * :multi-prefix sasl=PLAIN,EXTERNAL")
				send(":srv CAP * LS :account-tag server-time")
			case "REQ":
				send(":srv CAP * ACK :" + p.param(1))
			case "END":
				send(":srv 001 " + nick + " :Welcome")
				for _, s := range f.script {
					send(s)
				}
			}
		case "NICK":
			if p.param(0) == "habakkuk" {
				send(":srv 433 * habakkuk :Nickname is already in use")
				continue
			}
			nick = p.param(0)
		case "AUTHENTICATE":
			if p.param(0) == "PLAIN" {
				send("AUTHENTICATE +")
				continue
			}
			want := base64.StdEncoding.EncodeToString([]byte("habakkuk\x00habakkuk\x00" + f.password))
			if p.param(0) == want {
				send(":srv 903 " + nick + " :SASL authentication successful")
			} else {
				send(":srv 904 " + nick + " :SASL authentication failed")
			}
		case "PING":
			send(":srv PONG srv :" + p.param(0))
		}
	}
}

func (f *fakeIRC) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

type inbox struct {
	mu   sync.Mutex
	msgs []channel.Message
	up   chan string
}

func (i *inbox) Deliver(_ context.Context, m channel.Message) {
	i.mu.Lock()
	i.msgs = append(i.msgs, m)
	i.mu.Unlock()
}
func (i *inbox) Beat() {}
func (i *inbox) Connected(d string) {
	select {
	case i.up <- d:
	default:
	}
}

func TestRegisterJoinReceiveSend(t *testing.T) {
	f := newFake(t, "pw",
		":sam!a@host PRIVMSG habakkuk_ :hello there",
		"@account=bo;msgid=m1 :bo!b@host PRIVMSG #Habakkuk :habakkuk_: what's the weather?",
		":bo!b@host PRIVMSG #habakkuk :just chatting",
		":bo!b@host PRIVMSG #habakkuk :ask habakkuk_ maybe",
		":bo!b@host PRIVMSG #habakkuk :\x01ACTION waves\x01",
		":habakkuk_!f@host PRIVMSG #habakkuk :echo of my own line",
	)
	a := New(f.ln.Addr().String(), false, "habakkuk", "habakkuk", "habakkuk", "pw", []string{"#habakkuk"})
	in := &inbox{up: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, in) }()
	select {
	case d := <-in.up:
		if d != "habakkuk_ on "+f.ln.Addr().String() {
			t.Fatalf("connected as %q", d)
		}
	case err := <-done:
		t.Fatalf("Run: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("not registered")
	}
	var msgs []channel.Message
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && len(msgs) < 4; time.Sleep(10 * time.Millisecond) {
		in.mu.Lock()
		msgs = append([]channel.Message(nil), in.msgs...)
		in.mu.Unlock()
	}
	if len(msgs) != 4 {
		t.Fatalf("delivered %d: %+v", len(msgs), msgs)
	}
	if m := msgs[0]; !m.Direct || m.Conversation != "sam" || m.Sender != "sam" || m.Text != "hello there" {
		t.Fatalf("dm = %+v", m)
	}
	if m := msgs[1]; m.Direct || !m.Mentioned || m.Conversation != "#habakkuk" || m.SenderAlt != "bo" || m.ID != "m1" || m.Text != "what's the weather?" {
		t.Fatalf("addressed = %+v", m)
	}
	if msgs[2].Mentioned || !msgs[3].Mentioned {
		t.Fatalf("mention detection: %+v / %+v", msgs[2], msgs[3])
	}

	long := strings.Repeat("word ", 120) + "\n\nsecond paragraph"
	if err := a.Send(ctx, channel.Address{Conversation: "#habakkuk"}, long, ""); err != nil {
		t.Fatal(err)
	}
	var sent []string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		sent = nil
		for _, l := range f.lines() {
			if strings.HasPrefix(l, "PRIVMSG #habakkuk :") {
				sent = append(sent, l)
			}
		}
		if len(sent) >= 3 {
			break
		}
	}
	if len(sent) != 3 || sent[2] != "PRIVMSG #habakkuk :second paragraph" {
		t.Fatalf("sent = %q", sent)
	}
	for _, l := range sent {
		if len(l)+2 > 512-100 {
			t.Fatalf("line of %d bytes leaves no room for the relay prefix", len(l))
		}
	}
	got := strings.Join(f.lines(), "\n")
	for _, want := range []string{"CAP REQ :sasl account-tag", "AUTHENTICATE PLAIN", "CAP END", "NICK habakkuk_", "JOIN #habakkuk"} {
		if !strings.Contains(got, want) {
			t.Errorf("client never sent %q:\n%s", want, got)
		}
	}
	if err := a.Send(ctx, channel.Address{Conversation: "bad target"}, "x", ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("bad target: %v", err)
	}
}

func TestSASLFailureIsFatal(t *testing.T) {
	f := newFake(t, "right")
	a := New(f.ln.Addr().String(), false, "habakkuk", "habakkuk", "habakkuk", "wrong", nil)
	err := a.Run(context.Background(), &inbox{up: make(chan string, 1)})
	if !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("err = %v, want Fatal", err)
	}
}

func TestSplitLine(t *testing.T) {
	s := strings.Repeat("é", 300) // 600 bytes, no spaces
	parts := splitLine(s, 101)
	for _, p := range parts {
		if len(p) > 101 || !utf8.ValidString(p) {
			t.Fatalf("bad part %q", p)
		}
	}
	if strings.Join(parts, "") != s {
		t.Fatal("parts do not add back up")
	}
	if parts := splitLine("   ", 10); parts != nil {
		t.Fatalf("blank line = %q", parts)
	}
}
