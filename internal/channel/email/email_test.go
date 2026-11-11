package email

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/textproto"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

func TestParsePlainReplyStripsHistory(t *testing.T) {
	raw := crlf(`From: Sam <SAM@Example.com>
To: habakkuk@example.com
Subject: Re: Plans
Message-ID: <m2@example.com>
In-Reply-To: <m1@example.com>
References: <root@example.com> <m1@example.com>
Authentication-Results: mx.example.com; dkim=pass header.d=example.com; dmarc=pass header.from=example.com
Content-Type: text/plain; charset=utf-8

Sounds good, book it.

> earlier answer in the middle stays

Thanks!
--
Sam

On Mon, 3 Nov 2026 at 10:00, Habakkuk <habakkuk@example.com> wrote:
> Shall I book the table?
> --
> Habakkuk
`)
	m, err := parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.From != "sam@example.com" || m.FromName != "Sam" || m.ID != "m2@example.com" || m.root() != "root@example.com" {
		t.Fatalf("header: %+v", m)
	}
	if !m.Authentic || m.Automated != "" {
		t.Fatalf("authentic=%v automated=%q", m.Authentic, m.Automated)
	}
	want := "Sounds good, book it.\n\n> earlier answer in the middle stays\n\nThanks!\n--\nSam"
	if m.Text != want {
		t.Fatalf("text = %q, want %q", m.Text, want)
	}
}

func TestParseHTMLAndAttachments(t *testing.T) {
	raw := crlf(`From: bo@example.org
Subject: Report
Message-ID: <r1@example.org>
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary=XX

--XX
Content-Type: multipart/alternative; boundary=YY

--YY
Content-Type: text/html; charset=utf-8

<html><head><style>p{}</style></head><body><p>Here is the <b>report</b>, see <a href="https://example.org/r">the site</a>.</p><ul><li>one</li><li>two</li></ul><div class="gmail_quote">On Sun someone wrote:<blockquote>old stuff</blockquote></div></body></html>
--YY--

--XX
Content-Type: application/pdf; name="q3.pdf"
Content-Disposition: attachment; filename="q3.pdf"
Content-Transfer-Encoding: base64

JVBERi0xLjQK
--XX--
`)
	m, err := parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := "Here is the report, see the site (https://example.org/r).\n\n• one\n• two"
	if m.Text != want {
		t.Fatalf("text = %q, want %q", m.Text, want)
	}
	if len(m.Files) != 1 || m.Files[0].Name != "q3.pdf" || m.Files[0].MediaType != "application/pdf" || string(m.Files[0].Data) != "%PDF-1.4\n" {
		t.Fatalf("files = %+v", m.Files)
	}
	if m.Authentic {
		t.Fatal("a message without Authentication-Results is not authentic")
	}
}

func TestAutomatedAndAuthentic(t *testing.T) {
	for _, tc := range []struct {
		hdr, from string
		auto      bool
		authentic bool
	}{
		{"Auto-Submitted: auto-replied", "a@x.com", true, false},
		{"Auto-Submitted: no", "a@x.com", false, false},
		{"Precedence: bulk", "a@x.com", true, false},
		{"List-Id: <news.x.com>", "a@x.com", true, false},
		{"X-Autoreply: yes", "a@x.com", true, false},
		{"X-Other: 1", "MAILER-DAEMON@x.com", true, false},
		{"Authentication-Results: mx; dkim=pass header.d=x.com", "a@mail.x.com", false, true},
		{"Authentication-Results: mx; dkim=pass header.d=evil.com", "a@x.com", false, false},
		{"Authentication-Results: mx; dmarc=fail\r\nAuthentication-Results: forged; dmarc=pass", "a@x.com", false, false},
	} {
		raw := crlf("From: "+tc.from+"\nMessage-ID: <1@x>\n") + tc.hdr + "\r\n\r\nhi\r\n"
		m, err := parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if (m.Automated != "") != tc.auto || m.Authentic != tc.authentic {
			t.Errorf("%q from %s: automated=%q authentic=%v", tc.hdr, tc.from, m.Automated, m.Authentic)
		}
	}
}

func TestStripQuotedVariants(t *testing.T) {
	for in, want := range map[string]string{
		"ok\n\nOn Tue, Nov 4, 2026 at 9:00 AM Habakkuk <f@x.com>\nwrote:\n> q":  "ok",
		"yes please\n\n-----Original Message-----\nFrom: Habakkuk\nSent: today": "yes please",
		"fine\n________________________________\nFrom: Habakkuk":                "fine",
		"see below\nFrom: Habakkuk <f@x.com>\nSent: Monday\nTo: me":             "see below",
		"Le lun. 3 nov. 2026, Habakkuk a écrit :\n> x":                          "",
	} {
		if got := stripQuoted(in); got != want {
			t.Errorf("stripQuoted(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitConversation(t *testing.T) {
	peer, root, ok := split("a.b@x.com/abc/def@host")
	if !ok || peer != "a.b@x.com" || root != "abc/def@host" {
		t.Fatalf("split = %q %q %v", peer, root, ok)
	}
	if _, _, ok := split("nobody"); ok {
		t.Fatal("split accepted a conversation without an address")
	}
}

// fakeSMTP accepts mail on loopback and keeps it.
type fakeSMTP struct {
	ln     net.Listener
	mu     sync.Mutex
	got    []sentMail
	reject string // recipient refused with 550
}

type sentMail struct {
	from, to, data, auth string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	tp := textproto.NewConn(c)
	var m sentMail
	_ = tp.PrintfLine("220 fake ESMTP")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			_ = tp.PrintfLine("250-fake\r\n250-AUTH PLAIN\r\n250 8BITMIME")
		case "AUTH":
			m.auth = arg
			_ = tp.PrintfLine("235 ok")
		case "MAIL":
			m.from, _, _ = strings.Cut(strings.TrimPrefix(arg, "FROM:<"), ">")
			_ = tp.PrintfLine("250 ok")
		case "RCPT":
			m.to = strings.Trim(strings.TrimPrefix(arg, "TO:"), "<> ")
			if m.to == f.reject {
				_ = tp.PrintfLine("550 no such user")
				continue
			}
			_ = tp.PrintfLine("250 ok")
		case "DATA":
			_ = tp.PrintfLine("354 go on")
			data, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			m.data = string(data)
			f.mu.Lock()
			f.got = append(f.got, m)
			f.mu.Unlock()
			_ = tp.PrintfLine("250 queued")
		case "QUIT":
			_ = tp.PrintfLine("221 bye")
			return
		default:
			_ = tp.PrintfLine("250 ok")
		}
	}
}

func (f *fakeSMTP) sent() []sentMail {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMail(nil), f.got...)
}

type inbox struct {
	mu   sync.Mutex
	msgs []channel.Message
	conn chan string
}

func (i *inbox) Deliver(_ context.Context, m channel.Message) {
	i.mu.Lock()
	i.msgs = append(i.msgs, m)
	i.mu.Unlock()
}
func (i *inbox) Beat() {}
func (i *inbox) Connected(d string) {
	select {
	case i.conn <- d:
	default:
	}
}

func (i *inbox) wait(t *testing.T, n int) []channel.Message {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		i.mu.Lock()
		got := append([]channel.Message(nil), i.msgs...)
		i.mu.Unlock()
		if len(got) >= n {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d messages", n)
	return nil
}

// newIMAP serves one in-memory mailbox on loopback; idle says whether the
// server offers IDLE (IMAP4rev2) or only polling.
func newIMAP(t *testing.T, idle bool) (*imapmemserver.User, string) {
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("habakkuk@example.com", "pw")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(user)
	caps := imap.CapSet{imap.CapIMAP4rev1: {}}
	if idle {
		caps[imap.CapIMAP4rev2] = struct{}{}
	}
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         caps,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return user, ln.Addr().String()
}

func appendMail(t *testing.T, u *imapmemserver.User, raw string) {
	t.Helper()
	if _, err := u.Append("INBOX", bytes.NewReader([]byte(crlf(raw))), &imap.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
}

const firstMail = `From: Sam <sam@example.com>
To: habakkuk@example.com
Subject: Dinner
Message-ID: <d1@example.com>
Authentication-Results: mx.example.com; dmarc=pass header.from=example.com
Content-Type: text/plain

Book a table for two at 7.
`

func runAdapter(t *testing.T, idle bool) (*Adapter, *imapmemserver.User, *fakeSMTP, *inbox, func()) {
	t.Helper()
	user, addr := newIMAP(t, idle)
	appendMail(t, user, "From: old@example.com\nMessage-ID: <old@example.com>\nSubject: old\n\nalready here\n")
	smtpSrv := newFakeSMTP(t)
	a := New(Config{
		Address: "habakkuk@example.com", Name: "Habakkuk", IMAP: addr, SMTP: smtpSrv.ln.Addr().String(),
		IMAPTLS: "none", SMTPTLS: "none", Password: "pw", Poll: 50 * time.Millisecond,
		StatePath: filepath.Join(t.TempDir(), "email-mail.json"),
	}, quiet)
	in := &inbox{conn: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, in) }()
	select {
	case <-in.conn:
	case err := <-done:
		t.Fatalf("Run returned early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("never connected")
	}
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	}
	return a, user, smtpSrv, in, stop
}

func TestRunDeliversAndRepliesInThread(t *testing.T) {
	for _, idle := range []bool{true, false} {
		t.Run(map[bool]string{true: "idle", false: "poll"}[idle], func(t *testing.T) {
			a, user, smtpSrv, in, stop := runAdapter(t, idle)
			defer stop()
			appendMail(t, user, firstMail)
			appendMail(t, user, "From: list@example.com\nMessage-ID: <n@example.com>\nList-Id: <news>\nAuthentication-Results: mx; dmarc=pass\n\nnews\n")
			appendMail(t, user, "From: spoof@example.com\nMessage-ID: <s@example.com>\n\nno auth results\n")
			appendMail(t, user, "From: Habakkuk <habakkuk@example.com>\nMessage-ID: <own@example.com>\nAuthentication-Results: mx; dmarc=pass\n\nmy own copy\n")
			in.wait(t, 1)
			time.Sleep(200 * time.Millisecond) // the skipped ones must stay skipped
			msgs := in.wait(t, 1)
			if len(msgs) != 1 {
				t.Fatalf("delivered %d messages, want only the person's: %+v", len(msgs), msgs)
			}
			m := msgs[0]
			if m.Conversation != "sam@example.com/d1@example.com" || m.ID != "d1@example.com" || m.Sender != "sam@example.com" ||
				!m.Direct || m.Text != "Subject: Dinner\n\nBook a table for two at 7." {
				t.Fatalf("message = %+v", m)
			}

			to := channel.Address{Conversation: m.Conversation}
			if err := a.Send(context.Background(), to, "Booked for 7pm.", m.ID); err != nil {
				t.Fatal(err)
			}
			if err := a.SendFile(context.Background(), to, "receipt.pdf", "application/pdf", []byte("%PDF-1.4"), "The receipt"); err != nil {
				t.Fatal(err)
			}
			sent := smtpSrv.sent()
			if len(sent) != 2 {
				t.Fatalf("sent %d mails, want 2", len(sent))
			}
			r := sent[0]
			if r.from != "habakkuk@example.com" || r.to != "sam@example.com" || r.auth == "" {
				t.Fatalf("envelope = %+v", r)
			}
			for _, want := range []string{"Subject: Re: Dinner", "In-Reply-To: <d1@example.com>", "References: <d1@example.com>",
				"Auto-Submitted: auto-replied", "Booked for 7pm."} {
				if !strings.Contains(r.data, want) {
					t.Errorf("reply lacks %q:\n%s", want, r.data)
				}
			}
			first, _ := parse([]byte(r.data))
			f := sent[1].data
			for _, want := range []string{"Subject: Re: Dinner", "References: <d1@example.com> <" + first.ID + ">", "filename=receipt.pdf", "The receipt"} {
				if !strings.Contains(f, want) {
					t.Errorf("file mail lacks %q:\n%s", want, f)
				}
			}

			// The answer to the reply lands in the same conversation.
			appendMail(t, user, `From: sam@example.com
Subject: Re: Dinner
Message-ID: <d2@example.com>
In-Reply-To: <`+first.ID+`>
References: <d1@example.com> <`+first.ID+`>
Authentication-Results: mx; dmarc=pass

Thanks!

On Tue, Habakkuk wrote:
> Booked for 7pm.
`)
			msgs = in.wait(t, 2)
			if msgs[1].Conversation != m.Conversation || msgs[1].Text != "Thanks!" {
				t.Fatalf("follow-up = %+v", msgs[1])
			}
		})
	}
}

func TestRestartResumesAfterLastSeen(t *testing.T) {
	a, user, _, in, stop := runAdapter(t, true)
	appendMail(t, user, firstMail)
	in.wait(t, 1)
	stop()
	appendMail(t, user, "From: sam@example.com\nMessage-ID: <later@example.com>\nAuthentication-Results: mx; dmarc=pass\n\nwhile you were away\n")

	in2 := &inbox{conn: make(chan string, 1)}
	b := New(a.cfg, quiet)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Run(ctx, in2) }()
	in2.wait(t, 1)
	time.Sleep(100 * time.Millisecond)
	msgs := in2.wait(t, 1)
	if len(msgs) != 1 || msgs[0].ID != "later@example.com" {
		t.Fatalf("after restart delivered %+v, want only the new mail", msgs)
	}
}

func TestSendRefusedRecipientIsUndeliverable(t *testing.T) {
	smtpSrv := newFakeSMTP(t)
	smtpSrv.reject = "gone@example.com"
	a := New(Config{Address: "habakkuk@example.com", SMTP: smtpSrv.ln.Addr().String(), SMTPTLS: "none"}, quiet)
	err := a.Send(context.Background(), channel.Address{Conversation: "gone@example.com/x@y"}, "hi", "")
	var u channel.Undeliverable
	if !errors.As(err, &u) {
		t.Fatalf("err = %v, want Undeliverable", err)
	}
	if err := a.Send(context.Background(), channel.Address{Conversation: "no-address"}, "hi", ""); !errors.As(err, &u) {
		t.Fatalf("bad conversation: err = %v, want Undeliverable", err)
	}
}

func TestBadLoginIsFatal(t *testing.T) {
	_, addr := newIMAP(t, true)
	a := New(Config{Address: "habakkuk@example.com", IMAP: addr, IMAPTLS: "none", Password: "wrong"}, quiet)
	err := a.Run(context.Background(), &inbox{conn: make(chan string, 1)})
	var f channel.Fatal
	if !errors.As(err, &f) {
		t.Fatalf("err = %v, want Fatal", err)
	}
}

// A short IDLE makes a test that races an append with the start of IDLE
// find the message on the next search instead of waiting minutes.
func init() { idleFor = 300 * time.Millisecond }
