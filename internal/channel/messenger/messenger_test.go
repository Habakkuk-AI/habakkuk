package messenger

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/metagraph"
)

type call struct {
	path string
	body map[string]any
	form map[string]string
}

type fakeGraph struct {
	mu    sync.Mutex
	calls []call
}

func (f *fakeGraph) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/cdn/pic.jpg" {
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		io.WriteString(w, "JPEG")
		return
	}
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"Invalid OAuth access token","code":190}}`)
		return
	}
	c := call{path: r.URL.Path}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		r.ParseMultipartForm(1 << 20)
		c.form = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			c.form[k] = v[0]
		}
		if fh := r.MultipartForm.File["filedata"]; len(fh) == 1 {
			c.form["filedata"] = fh[0].Filename + ";" + fh[0].Header.Get("Content-Type")
		}
	} else if r.Method == http.MethodPost {
		json.NewDecoder(r.Body).Decode(&c.body)
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	switch r.URL.Path {
	case "/me":
		io.WriteString(w, `{"id":"PAGE1","name":"Habakkuk"}`)
	case "/PAGE1/messages", "/me/messages":
		if rcpt, _ := c.body["recipient"].(map[string]any); rcpt != nil && rcpt["id"] == "OLD" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"(#10) This message is sent outside of allowed window.","code":10,"error_subcode":2018278}}`)
			return
		}
		io.WriteString(w, `{"recipient_id":"x","message_id":"m.out"}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeGraph) take() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
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

func run(t *testing.T, cfg Config) (*Adapter, *fakeGraph, *inbox, string) {
	t.Helper()
	fake := &fakeGraph{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	cfg.Token, cfg.AppSecret, cfg.VerifyToken, cfg.APIURL = "tok", "appsecret", "verify-me", srv.URL
	a := New(cfg)
	in := &inbox{up: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = a.Run(ctx, in) }()
	select {
	case <-in.up:
	case <-time.After(5 * time.Second):
		t.Fatal("not connected")
	}
	fake.take()
	return a, fake, in, srv.URL
}

func post(a *Adapter, secret, body string) int {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("X-Hub-Signature-256", metagraph.Sign(secret, []byte(body)))
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w.Code
}

func TestMessenger(t *testing.T) {
	a, fake, in, base := run(t, Config{PageID: "PAGE1"})
	ctx := context.Background()

	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/?hub.mode=subscribe&hub.verify_token=verify-me&hub.challenge=abc", nil))
	if w.Body.String() != "abc" {
		t.Fatalf("handshake: %d %q", w.Code, w.Body)
	}

	body := `{"object":"page","entry":[{"id":"PAGE1","time":1,"messaging":[
{"sender":{"id":"PSID1"},"recipient":{"id":"PAGE1"},"timestamp":1,"message":{"mid":"m.1","text":"hello"}},
{"sender":{"id":"PSID1"},"recipient":{"id":"PAGE1"},"timestamp":1,"message":{"mid":"m.2","attachments":[{"type":"image","payload":{"url":"` + base + `/cdn/pic.jpg"}},{"type":"fallback","payload":{}}]}},
{"sender":{"id":"PSID1"},"recipient":{"id":"PAGE1"},"timestamp":1,"message":{"mid":"m.3","text":"Approve","quick_reply":{"payload":"cb:n|0"}}},
{"sender":{"id":"PSID1"},"recipient":{"id":"PAGE1"},"timestamp":1,"postback":{"mid":"m.4","title":"Go","payload":"cb:n|1"}},
{"sender":{"id":"PAGE1"},"recipient":{"id":"PSID1"},"timestamp":1,"message":{"mid":"m.5","text":"echo","is_echo":true}},
{"sender":{"id":"PSID1"},"recipient":{"id":"PAGE1"},"timestamp":1,"read":{"watermark":1}}]},
{"id":"PAGE2","messaging":[{"sender":{"id":"PSID9"},"recipient":{"id":"PAGE2"},"message":{"mid":"m.9","text":"other page"}}]}]}`
	if code := post(a, "nope", body); code != http.StatusForbidden {
		t.Fatalf("bad signature: %d", code)
	}
	if code := post(a, "appsecret", body); code != http.StatusOK {
		t.Fatalf("webhook: %d", code)
	}
	in.mu.Lock()
	msgs := in.msgs
	in.mu.Unlock()
	if len(msgs) != 4 {
		t.Fatalf("delivered %d: %+v", len(msgs), msgs)
	}
	if m := msgs[0]; m.Text != "hello" || !m.Direct || m.Conversation != "PSID1" || m.ID != "m.1" {
		t.Fatalf("text = %+v", m)
	}
	if m := msgs[1]; len(m.Attachments) != 1 {
		t.Fatalf("image = %+v", m)
	} else if data, err := m.Attachments[0].Fetch(ctx); err != nil || string(data) != "JPEG" {
		t.Fatalf("fetch = %q %v", data, err)
	}
	if msgs[2].Callback != "cb:n|0" || msgs[3].Callback != "cb:n|1" {
		t.Fatalf("choices = %+v %+v", msgs[2], msgs[3])
	}

	to := channel.Address{Conversation: "PSID1"}
	if err := a.Send(ctx, to, "hi", "m.1"); err != nil {
		t.Fatal(err)
	}
	if err := a.Typing(ctx, to); err != nil {
		t.Fatal(err)
	}
	if err := a.Typing(ctx, to); err != nil { // throttled
		t.Fatal(err)
	}
	if err := a.SendFile(ctx, to, "c.png", "image/png", []byte("PNG"), "chart"); err != nil {
		t.Fatal(err)
	}
	if err := a.SendButtons(ctx, to, "Run it?", [][]channel.Button{{{Text: "Approve", Data: "a"}, {Text: "Deny", Data: "d"}}}); err != nil {
		t.Fatal(err)
	}
	calls := fake.take()
	if len(calls) != 5 {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].path != "/PAGE1/messages" || calls[0].body["message"].(map[string]any)["text"] != "hi" || calls[0].body["messaging_type"] != "RESPONSE" {
		t.Fatalf("send = %+v", calls[0])
	}
	if calls[1].body["sender_action"] != "typing_on" {
		t.Fatalf("typing = %+v", calls[1])
	}
	if f := calls[2].form; f["filedata"] != "c.png;image/png" || !strings.Contains(f["message"], `"type":"image"`) || f["recipient"] != `{"id":"PSID1"}` {
		t.Fatalf("upload = %+v", f)
	}
	if calls[3].body["message"].(map[string]any)["text"] != "chart" {
		t.Fatalf("caption = %+v", calls[3])
	}
	if qr := calls[4].body["message"].(map[string]any)["quick_replies"].([]any); len(qr) != 2 {
		t.Fatalf("quick replies = %v", qr)
	}

	err := a.Send(ctx, channel.Address{Conversation: "OLD"}, "late", "")
	if !errors.As(err, new(channel.Undeliverable)) || !strings.Contains(err.Error(), "24 hours") {
		t.Fatalf("outside window: %v", err)
	}
}

func TestInstagramMediaByURL(t *testing.T) {
	a, fake, in, _ := run(t, Config{Instagram: true, WebhookURL: "https://habakkuk.example.com/channels/ig"})
	ctx := context.Background()
	body := `{"object":"instagram","entry":[{"id":"IG1","time":1,"messaging":[
{"sender":{"id":"IGSID1"},"recipient":{"id":"IG1"},"timestamp":1,"message":{"mid":"ig.1","text":"hey"}}]}]}`
	if code := post(a, "appsecret", body); code != http.StatusOK {
		t.Fatalf("webhook: %d", code)
	}
	// A Messenger payload on an Instagram channel is ignored.
	if code := post(a, "appsecret", strings.Replace(body, `"instagram"`, `"page"`, 1)); code != http.StatusOK {
		t.Fatal(code)
	}
	in.mu.Lock()
	n := len(in.msgs)
	in.mu.Unlock()
	if n != 1 {
		t.Fatalf("delivered %d", n)
	}
	if a.MaxLen() != 1000 {
		t.Fatal("instagram max length")
	}
	to := channel.Address{Conversation: "IGSID1"}
	if err := a.SendFile(ctx, to, "c.png", "image/png", []byte("PNG"), ""); err != nil {
		t.Fatal(err)
	}
	calls := fake.take()
	att := calls[0].body["message"].(map[string]any)["attachment"].(map[string]any)
	link := att["payload"].(map[string]any)["url"].(string)
	tok, ok := strings.CutPrefix(link, "https://habakkuk.example.com/channels/ig/media/")
	if !ok || calls[0].path != "/me/messages" {
		t.Fatalf("attachment = %v on %s", att, calls[0].path)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/media/"+tok, nil))
	if w.Body.String() != "PNG" {
		t.Fatalf("served %q", w.Body)
	}
	err := a.SendFile(ctx, to, "a.zip", "application/zip", []byte("PK"), "")
	if !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("zip: %v", err)
	}
}

func TestBadTokenIsFatal(t *testing.T) {
	srv := httptest.NewServer(&fakeGraph{})
	defer srv.Close()
	a := New(Config{Token: "nope", AppSecret: "s", VerifyToken: "v", APIURL: srv.URL})
	if err := a.Run(context.Background(), &inbox{}); !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("err = %v, want Fatal", err)
	}
}
