package line

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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
)

type call struct {
	path string
	body map[string]any
}

type fakeLINE struct {
	mu    sync.Mutex
	calls []call
}

func (f *fakeLINE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"message":"Authentication failed"}`)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/content") {
		io.WriteString(w, "JPEG")
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.calls = append(f.calls, call{r.URL.Path, body})
	f.mu.Unlock()
	switch r.URL.Path {
	case "/v2/bot/info":
		io.WriteString(w, `{"userId":"Ubot","displayName":"Habakkuk"}`)
	case "/v2/bot/message/reply":
		if body["replyToken"] == "used" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"message":"Invalid reply token"}`)
			return
		}
		io.WriteString(w, `{}`)
	default:
		io.WriteString(w, `{}`)
	}
}

func (f *fakeLINE) got() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
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

func signed(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func hook(a *Adapter, sig, body string) int {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("X-Line-Signature", sig)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w.Code
}

func TestWebhookRepliesAndPush(t *testing.T) {
	fake := &fakeLINE{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	a := New("chan-secret", "tok", "https://habakkuk.example.com/channels/line", srv.URL)
	in := &inbox{up: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Run(ctx, in) }()
	select {
	case d := <-in.up:
		if d != "LINE Habakkuk" {
			t.Fatalf("connected as %q", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not connected")
	}

	body := `{"destination":"Ubot","events":[
{"type":"message","mode":"active","replyToken":"rt1","source":{"type":"user","userId":"Uay"},"message":{"id":"100","type":"text","text":"hello"}},
{"type":"message","mode":"active","replyToken":"used","source":{"type":"group","groupId":"Cg1","userId":"Ubo"},
 "message":{"id":"101","type":"text","text":"@Habakkuk 😀 what's up","mention":{"mentionees":[{"index":0,"length":9,"type":"user","isSelf":true}]}}},
{"type":"message","mode":"active","source":{"type":"user","userId":"Uay"},"message":{"id":"102","type":"image","contentProvider":{"type":"line"}}},
{"type":"message","mode":"active","source":{"type":"user","userId":"Uay"},"message":{"id":"103","type":"sticker"}},
{"type":"follow","source":{"type":"user","userId":"Uay"}}]}`
	if code := hook(a, signed("other", body), body); code != http.StatusForbidden {
		t.Fatalf("bad signature: %d", code)
	}
	if code := hook(a, signed("chan-secret", body), body); code != http.StatusOK {
		t.Fatalf("webhook: %d", code)
	}
	in.mu.Lock()
	msgs := in.msgs
	in.mu.Unlock()
	if len(msgs) != 3 {
		t.Fatalf("delivered %d: %+v", len(msgs), msgs)
	}
	if m := msgs[0]; m.Conversation != "Uay" || !m.Direct || m.Text != "hello" || m.ID != "100" {
		t.Fatalf("dm = %+v", m)
	}
	if g := msgs[1]; g.Conversation != "Cg1" || g.Direct || !g.Mentioned || g.Text != "😀 what's up" || g.Sender != "Ubo" {
		t.Fatalf("group = %+v", g)
	}
	if data, err := msgs[2].Attachments[0].Fetch(ctx); err != nil || string(data) != "JPEG" {
		t.Fatalf("image = %q %v", data, err)
	}

	// A fresh token answers for free; a used one falls back to push; a
	// later chunk (no replyTo) pushes.
	if err := a.Send(ctx, channel.Address{Conversation: "Uay"}, "hi back", "100"); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, channel.Address{Conversation: "Cg1"}, "nothing much", "101"); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, channel.Address{Conversation: "Uay"}, "more", "100"); err != nil {
		t.Fatal(err)
	}
	if err := a.Typing(ctx, channel.Address{Conversation: "Uay"}); err != nil {
		t.Fatal(err)
	}
	if err := a.SendFile(ctx, channel.Address{Conversation: "Uay"}, "c.png", "image/png", []byte("PNG"), "chart"); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, c := range fake.got()[1:] {
		paths = append(paths, c.path)
	}
	want := []string{"/v2/bot/message/reply", "/v2/bot/message/reply", "/v2/bot/message/push", "/v2/bot/message/push",
		"/v2/bot/chat/loading/start", "/v2/bot/message/push"}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Fatalf("calls = %v\nwant  %v", paths, want)
	}
	img := fake.got()[len(fake.got())-1].body["messages"].([]any)[0].(map[string]any)
	tok, ok := strings.CutPrefix(img["originalContentUrl"].(string), "https://habakkuk.example.com/channels/line/media/")
	if !ok {
		t.Fatalf("image message = %v", img)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/media/"+tok, nil))
	if w.Body.String() != "PNG" {
		t.Fatalf("served %q", w.Body)
	}
	err := a.SendFile(ctx, channel.Address{Conversation: "Uay"}, "a.pdf", "application/pdf", []byte("x"), "")
	if !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("pdf: %v", err)
	}
}

func TestBadTokenIsFatal(t *testing.T) {
	srv := httptest.NewServer(&fakeLINE{})
	defer srv.Close()
	err := New("s", "nope", "", srv.URL).Run(context.Background(), &inbox{up: make(chan string, 1)})
	if !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("err = %v, want Fatal", err)
	}
}
