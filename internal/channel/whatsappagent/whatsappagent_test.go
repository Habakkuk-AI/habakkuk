package whatsappagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

func init() { pollGap = 10 * time.Millisecond }

type call struct {
	method, path, query string
	body                map[string]any
	form                map[string]string
}

// fakePlatform serves the agent API: queued poll replies, sends, statuses
// and media.
type fakePlatform struct {
	srv      *httptest.Server
	mu       sync.Mutex
	calls    []call
	polls    []string // bodies for successive polls; "" answers 204
	pollCode int      // when set, polls fail with this HTTP status
	sendErr  string   // when set, /messages fails with this body (HTTP 403)
}

func (f *fakePlatform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer key" {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"(#190) bad header","type":"OAuthException","code":190}}`)
		return
	}
	c := call{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		r.ParseMultipartForm(1 << 20)
		c.form = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			c.form[k] = v[0]
		}
		if fh := r.MultipartForm.File["file"]; len(fh) == 1 {
			c.form["file"] = fh[0].Filename
		}
	} else if r.Method == http.MethodPost {
		json.NewDecoder(r.Body).Decode(&c.body)
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	var poll string
	pollCode, sendErr := f.pollCode, f.sendErr
	if r.URL.Path == "/updates" && len(f.polls) > 0 {
		poll, f.polls = f.polls[0], f.polls[1:]
	}
	f.mu.Unlock()
	switch r.URL.Path {
	case "/updates":
		if pollCode != 0 {
			w.WriteHeader(pollCode)
			io.WriteString(w, `{"error":{"message":"(#1752041) replaced","type":"OAuthException","code":1752041}}`)
			return
		}
		if poll == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		io.WriteString(w, poll)
	case "/messages":
		if sendErr != "" {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, sendErr)
			return
		}
		io.WriteString(w, `{"messaging_product":"whatsapp","messages":[{"id":"wamid.out"}]}`)
	case "/statuses":
		io.WriteString(w, `{"success":true}`)
	case "/media":
		io.WriteString(w, `{"id":"up1"}`)
	case "/media/M1":
		io.WriteString(w, `{"url":"`+f.srv.URL+`/content/M1","mime_type":"image/jpeg"}`)
	case "/media/EVIL":
		io.WriteString(w, `{"url":"https://evil.example.com/steal","mime_type":"image/jpeg"}`)
	case "/content/M1":
		io.WriteString(w, "JPEG")
	default:
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"unknown","code":100}}`)
	}
}

func (f *fakePlatform) got(path string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.path == path {
			out = append(out, c)
		}
	}
	return out
}

func newFake(t *testing.T) (*fakePlatform, *Adapter) {
	t.Helper()
	f := &fakePlatform{}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	a := New(Config{Token: "key", APIURL: f.srv.URL, StatePath: filepath.Join(t.TempDir(), "state.json")})
	return f, a
}

type inbox struct {
	mu        sync.Mutex
	msgs      []channel.Message
	connected string
}

func (in *inbox) Deliver(_ context.Context, m channel.Message) {
	in.mu.Lock()
	in.msgs = append(in.msgs, m)
	in.mu.Unlock()
}
func (in *inbox) Beat() {}
func (in *inbox) Connected(d string) {
	in.mu.Lock()
	in.connected = d
	in.mu.Unlock()
}
func (in *inbox) got() []channel.Message {
	in.mu.Lock()
	defer in.mu.Unlock()
	return append([]channel.Message(nil), in.msgs...)
}

func update(next int64, contacts, messages string) string {
	return fmt.Sprintf(`{"object":"whatsapp_agent_platform","entry":[{"id":"123456789","changes":[{"field":"messages",
"value":{"messaging_product":"whatsapp","contacts":[%s],"messages":[%s],"statuses":[]}}]}],"next_offset":%d}`, contacts, messages, next)
}

func textMsg(id string, at int64, body string) string {
	return fmt.Sprintf(`{"from":"user:50972923564215","id":%q,"timestamp":"%d","type":"text","text":{"body":%q}}`, id, at, body)
}

// runUntil runs the adapter until cond holds, then stops it.
func runUntil(t *testing.T, a *Adapter, in *inbox, cond func() bool) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, in) }()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		select {
		case err := <-done:
			cancel()
			return err
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	return <-done
}

func TestPollDeliversAndResumes(t *testing.T) {
	f, a := newFake(t)
	now := time.Now().Unix()
	old := now - int64(time.Hour/time.Second)
	f.polls = []string{
		"", // empty poll: offset stays
		update(7, `{"wa_id":"user:50972923564215","profile":{"name":"Alex"}}`,
			textMsg("wamid.old", old, "from before Habakkuk")+","+textMsg("wamid.1", now, "hello agent")+
				`,{"from":"user:50972923564215","id":"wamid.r","timestamp":"`+fmt.Sprint(now)+`","type":"reaction","reaction":{"message_id":"wamid.1","emoji":"👍"}}`),
	}
	in := &inbox{}
	if err := runUntil(t, a, in, func() bool { return len(in.got()) == 1 && len(f.got("/updates")) >= 3 }); err != nil {
		t.Fatal(err)
	}
	m := in.got()[0]
	if m.ID != "wamid.1" || m.Text != "hello agent" || m.Conversation != "50972923564215" || m.Sender != "50972923564215" ||
		m.SenderName != "Alex" || !m.Direct {
		t.Fatalf("message: %+v", m)
	}
	if in.connected == "" {
		t.Fatal("never reported connected")
	}
	polls := f.got("/updates")
	if !strings.Contains(polls[0].query, "offset=0") || !strings.Contains(polls[1].query, "offset=0") ||
		!strings.Contains(polls[2].query, "offset=7") || !strings.Contains(polls[0].query, "timeout=20") {
		t.Fatalf("poll queries: %q %q %q", polls[0].query, polls[1].query, polls[2].query)
	}

	// A new adapter on the same state resumes at the saved offset and
	// knows the creator.
	b := New(Config{Token: "key", APIURL: f.srv.URL, StatePath: a.cfg.StatePath})
	if err := runUntil(t, b, &inbox{}, func() bool { return len(f.got("/updates")) > len(polls) }); err != nil {
		t.Fatal(err)
	}
	if all := f.got("/updates"); !strings.Contains(all[len(all)-1].query, "offset=7") {
		t.Fatalf("resumed poll: %q", all[len(all)-1].query)
	}
	if err := b.Send(context.Background(), channel.Address{Conversation: Creator}, "proactive", ""); err != nil {
		t.Fatal(err)
	}
	if sends := f.got("/messages"); sends[len(sends)-1].body["to"] != "user:50972923564215" {
		t.Fatalf("creator send: %+v", sends)
	}
}

func TestPollConflictIsFatal(t *testing.T) {
	f, a := newFake(t)
	f.pollCode = http.StatusConflict
	err := a.Run(context.Background(), &inbox{})
	var fatal channel.Fatal
	if !errors.As(err, &fatal) || !strings.Contains(err.Error(), "another program") {
		t.Fatalf("got %v", err)
	}
}

func TestBadKeyIsFatal(t *testing.T) {
	_, a := newFake(t)
	a.api.Token = "wrong"
	var fatal channel.Fatal
	if err := a.Run(context.Background(), &inbox{}); !errors.As(err, &fatal) {
		t.Fatalf("got %v", err)
	}
	if err := New(Config{}).Run(context.Background(), &inbox{}); !errors.As(err, &fatal) {
		t.Fatalf("empty key: %v", err)
	}
}

func TestSendText(t *testing.T) {
	f, a := newFake(t)
	if err := a.Send(context.Background(), channel.Address{Conversation: "50972923564215"}, "hi", "wamid.1"); err != nil {
		t.Fatal(err)
	}
	b := f.got("/messages")[0].body
	if b["to"] != "user:50972923564215" || b["type"] != "text" || b["messaging_product"] != "whatsapp" ||
		b["text"].(map[string]any)["body"] != "hi" || b["context"].(map[string]any)["message_id"] != "wamid.1" {
		t.Fatalf("body: %+v", b)
	}
	// No creator known yet: a send to "creator" cannot go anywhere.
	var und channel.Undeliverable
	if err := a.Send(context.Background(), channel.Address{Conversation: Creator}, "hi", ""); !errors.As(err, &und) {
		t.Fatalf("creator unknown: %v", err)
	}
}

func TestSendToNonCreatorIsUndeliverable(t *testing.T) {
	f, a := newFake(t)
	f.sendErr = `{"error":{"message":"(#131005) not creator","type":"OAuthException","code":131005}}`
	err := a.Send(context.Background(), channel.Address{Conversation: "1"}, "hi", "")
	var und channel.Undeliverable
	if !errors.As(err, &und) || !strings.Contains(err.Error(), "only message the person who created it") {
		t.Fatalf("got %v", err)
	}
}

func TestMediaInAndOut(t *testing.T) {
	f, a := newFake(t)
	m, ok := a.convert(&message{From: "user:9", ID: "wamid.i", Type: "image", Image: &media{ID: "M1", MimeType: "image/jpeg", Caption: "look"}})
	if !ok || m.Text != "look" || len(m.Attachments) != 1 || m.Attachments[0].Name != "image.jpg" {
		t.Fatalf("convert: %+v %v", m, ok)
	}
	data, err := m.Attachments[0].Fetch(context.Background())
	if err != nil || string(data) != "JPEG" {
		t.Fatalf("fetch: %q %v", data, err)
	}
	if _, err := a.download(context.Background(), "EVIL"); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("foreign host: %v", err)
	}
	if m, ok := a.convert(&message{From: "user:9", ID: "v", Type: "audio", Audio: &media{ID: "A", MimeType: "audio/ogg", Voice: true}}); !ok || m.Attachments[0].Name != "voice.ogg" {
		t.Fatalf("voice: %+v", m)
	}
	if _, ok := a.convert(&message{From: "agent:1", ID: "x", Type: "text"}); ok {
		t.Fatal("accepted a message that is not from a user")
	}

	to := channel.Address{Conversation: "9"}
	if err := a.SendFile(context.Background(), to, "pic.png", "image/png", []byte("PNG"), "a caption"); err != nil {
		t.Fatal(err)
	}
	if err := a.SendFile(context.Background(), to, "notes.md", "text/markdown", []byte("# hi"), ""); err != nil {
		t.Fatal(err)
	}
	if err := a.SendFile(context.Background(), to, "song.mp3", "audio/mpeg", []byte("MP3"), "listen"); err != nil {
		t.Fatal(err)
	}
	ups, sends := f.got("/media"), f.got("/messages")
	if len(ups) != 3 || ups[0].form["type"] != "image/png" || ups[1].form["type"] != "application/octet-stream" ||
		ups[0].form["messaging_product"] != "whatsapp" || ups[0].form["file"] != "pic.png" {
		t.Fatalf("uploads: %+v", ups)
	}
	if len(sends) != 4 {
		t.Fatalf("sends: %+v", sends)
	}
	if img := sends[0].body["image"].(map[string]any); sends[0].body["type"] != "image" || img["caption"] != "a caption" || img["id"] != "up1" {
		t.Fatalf("image send: %+v", sends[0].body)
	}
	if doc := sends[1].body["document"].(map[string]any); sends[1].body["type"] != "document" || doc["filename"] != "notes.md" {
		t.Fatalf("document send: %+v", sends[1].body)
	}
	if sends[2].body["type"] != "audio" || sends[2].body["audio"].(map[string]any)["caption"] != nil ||
		sends[3].body["text"].(map[string]any)["body"] != "listen" {
		t.Fatalf("audio caption should follow as text: %+v %+v", sends[2].body, sends[3].body)
	}
	var und channel.Undeliverable
	if err := a.SendFile(context.Background(), to, "big.bin", "", make([]byte, maxMedia+1), ""); !errors.As(err, &und) {
		t.Fatalf("too big: %v", err)
	}
}

func TestTypingThrottled(t *testing.T) {
	f, a := newFake(t)
	now := time.Unix(1000, 0)
	a.Now = func() time.Time { return now }
	to := channel.Address{Conversation: "9", ReplyTo: "wamid.1"}
	for i := 0; i < 3; i++ {
		if err := a.Typing(context.Background(), to); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(typingEvery)
	if err := a.Typing(context.Background(), to); err != nil {
		t.Fatal(err)
	}
	st := f.got("/statuses")
	if len(st) != 2 || st[0].body["status"] != "read" || st[0].body["message_id"] != "wamid.1" ||
		st[0].body["typing_indicator"].(map[string]any)["type"] != "text" {
		t.Fatalf("statuses: %+v", st)
	}
	if err := a.Typing(context.Background(), channel.Address{Conversation: "9"}); err != nil || len(f.got("/statuses")) != 2 {
		t.Fatal("typing without a message to mark should do nothing")
	}
}
