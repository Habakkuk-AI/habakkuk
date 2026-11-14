package twitch

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

type fakeTMI struct {
	ln   net.Listener
	mu   sync.Mutex
	got  []string
	auth bool // accept the token
}

func (f *fakeTMI) serve(t *testing.T) {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	say := func(s string) { conn.Write([]byte(s + "\r\n")) }
	for sc.Scan() {
		l := sc.Text()
		f.mu.Lock()
		f.got = append(f.got, l)
		f.mu.Unlock()
		switch {
		case strings.HasPrefix(l, "NICK "):
			if !f.auth {
				say(":tmi.twitch.tv NOTICE * :Login authentication failed")
				return
			}
			say(":tmi.twitch.tv 001 habakkuk_bot :Welcome, GLHF!")
			say("PING :tmi.twitch.tv")
		case strings.HasPrefix(l, "JOIN "):
			say(`@badge-info=;display-name=Sam;id=m1;user-id=101 :sam!sam@sam.tmi.twitch.tv PRIVMSG #streamer :@habakkuk_bot what's the plan?`)
			say(`@display-name=Bo;id=m2;user-id=102 :bo!bo@bo.tmi.twitch.tv PRIVMSG #streamer :just chatting`)
			say(`@display-name=Cy;id=m3;user-id=103 :cy!cy@cy.tmi.twitch.tv PRIVMSG #streamer :habakkuk_bot: hi`)
			say(`@display-name=habakkuk_bot;id=m4;user-id=9 :habakkuk_bot!habakkuk_bot@habakkuk_bot.tmi.twitch.tv PRIVMSG #streamer :my own`)
		}
	}
}

func (f *fakeTMI) lines() []string {
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

func (i *inbox) count() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.msgs)
}

func start(t *testing.T, auth bool, token string) (*Adapter, *fakeTMI) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeTMI{ln: ln, auth: auth}
	go f.serve(t)
	a := New(ln.Addr().String(), "Habakkuk_Bot", token, []string{"Streamer", "#other"})
	a.TLS, a.Gap = false, time.Millisecond
	return a, f
}

func TestTwitch(t *testing.T) {
	a, fake := start(t, true, "abc123")
	in := &inbox{up: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Run(ctx, in) }()
	select {
	case d := <-in.up:
		if d != "Twitch habakkuk_bot in #streamer, #other" {
			t.Fatalf("connected as %q", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not connected")
	}
	deadline := time.Now().Add(5 * time.Second)
	for in.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	in.mu.Lock()
	msgs := append([]channel.Message(nil), in.msgs...)
	in.mu.Unlock()
	if len(msgs) != 3 {
		t.Fatalf("delivered %d: %+v", len(msgs), msgs)
	}
	if m := msgs[0]; m.Conversation != "#streamer" || m.Sender != "101" || m.SenderAlt != "sam" || m.SenderName != "Sam" ||
		!m.Mentioned || m.Text != "what's the plan?" || m.ID != "m1" || m.Direct {
		t.Fatalf("mention = %+v", m)
	}
	if m := msgs[1]; m.Mentioned || m.Text != "just chatting" {
		t.Fatalf("chatter = %+v", m)
	}
	if m := msgs[2]; !m.Mentioned || m.Text != "hi" {
		t.Fatalf("addressed = %+v", m)
	}

	long := strings.Repeat("word ", 120) // 600 characters: two messages
	if err := a.Send(ctx, channel.Address{Conversation: "#streamer"}, "first line\n"+long, "m1"); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, channel.Address{Conversation: "streamer"}, "x", ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("bad channel: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	var sent []string
	for time.Now().Before(deadline) {
		sent = nil
		for _, l := range fake.lines() {
			if strings.Contains(l, "PRIVMSG") {
				sent = append(sent, l)
			}
		}
		if len(sent) == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(sent) != 3 || sent[0] != "@reply-parent-msg-id=m1 PRIVMSG #streamer :first line" ||
		!strings.HasPrefix(sent[1], "PRIVMSG #streamer :word word") || len(sent[1]) > len("PRIVMSG #streamer :")+500 {
		t.Fatalf("sent:\n%s", strings.Join(sent, "\n"))
	}
	all := strings.Join(fake.lines(), "\n")
	for _, want := range []string{"PASS oauth:abc123", "NICK habakkuk_bot", "CAP REQ :twitch.tv/tags twitch.tv/commands", "JOIN #streamer,#other", "PONG :tmi.twitch.tv"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
}

func TestBadTokenIsFatal(t *testing.T) {
	a, _ := start(t, false, "oauth:nope")
	err := a.Run(context.Background(), &inbox{up: make(chan string, 1)})
	if !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("err = %v, want Fatal", err)
	}
}

func TestParse(t *testing.T) {
	l := parse(`@display-name=A\sB;id=x :a!a@a PRIVMSG #c :hello : world`)
	if l.tags["display-name"] != "A B" || l.nick != "a" || l.command != "PRIVMSG" || l.param(0) != "#c" || l.param(1) != "hello : world" {
		t.Fatalf("parse = %+v", l)
	}
}
