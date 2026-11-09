package bluebubbles

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
)

type fakeServer struct {
	mu     sync.Mutex
	calls  []string
	texts  []map[string]any
	files  []map[string]string
	typing int
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("password") != "pw" {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"status":401,"message":"Unauthorized"}`)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	switch {
	case r.URL.Path == "/api/v1/ping":
		io.WriteString(w, `{"status":200,"message":"Ping received!","data":"pong"}`)
	case r.URL.Path == "/api/v1/message/text":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.texts = append(f.texts, body)
		io.WriteString(w, `{"status":200,"data":{}}`)
	case r.URL.Path == "/api/v1/message/attachment":
		r.ParseMultipartForm(1 << 20)
		got := map[string]string{}
		for k, v := range r.MultipartForm.Value {
			got[k] = v[0]
		}
		fh, _ := r.MultipartForm.File["attachment"][0].Open()
		data, _ := io.ReadAll(fh)
		got["data"] = string(data)
		f.files = append(f.files, got)
		io.WriteString(w, `{"status":200,"data":{}}`)
	case strings.HasSuffix(r.URL.Path, "/typing"):
		f.typing++
		io.WriteString(w, `{"status":200}`)
	case strings.HasPrefix(r.URL.Path, "/api/v1/attachment/"):
		io.WriteString(w, "HEIC-BYTES")
	default:
		w.WriteHeader(http.StatusNotFound)
	}
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

func hook(a channel.Endpoint, secret, body string) int {
	r := httptest.NewRequest(http.MethodPost, "/?secret="+secret, strings.NewReader(body))
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w.Code
}

func TestWebhookAndREST(t *testing.T) {
	fake := &fakeServer{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	a := New(srv.URL, "pw", "hook-secret", true, "Habakkuk")
	ep := a.WithTyping()
	in := &inbox{up: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ep.Run(ctx, in) }()
	select {
	case <-in.up:
	case <-time.After(5 * time.Second):
		t.Fatal("not connected")
	}

	dm := `{"type":"new-message","data":{"guid":"g1","text":"hey ￼","isFromMe":false,"handle":{"address":"+15551234567"},
"chats":[{"guid":"iMessage;-;+15551234567"}],"attachments":[{"guid":"a1","transferName":"IMG_1.heic","mimeType":"image/heic","totalBytes":10}]}}`
	if code := hook(ep, "wrong", dm); code != http.StatusForbidden {
		t.Fatalf("wrong secret: %d", code)
	}
	for _, body := range []string{
		dm,
		`{"type":"new-message","data":{"guid":"g2","text":"my own","isFromMe":true,"handle":{"address":"+1"},"chats":[{"guid":"iMessage;-;+1"}]}}`,
		`{"type":"new-message","data":{"guid":"g3","text":"Loved “hey”","handle":{"address":"+15551234567"},"chats":[{"guid":"iMessage;-;+15551234567"}],"associatedMessageType":"love"}}`,
		`{"type":"new-message","data":{"guid":"g4","text":"habakkuk, what time?","handle":{"address":"bo@icloud.com"},"chats":[{"guid":"iMessage;+;chat99"}],"associatedMessageType":null}}`,
		`{"type":"updated-message","data":{"guid":"g1"}}`,
	} {
		if code := hook(ep, "hook-secret", body); code != http.StatusOK {
			t.Fatalf("hook: %d", code)
		}
	}
	in.mu.Lock()
	msgs := in.msgs
	in.mu.Unlock()
	if len(msgs) != 2 {
		t.Fatalf("delivered %d: %+v", len(msgs), msgs)
	}
	m := msgs[0]
	if m.Conversation != "iMessage;-;+15551234567" || m.Sender != "15551234567" || !m.Direct || m.Text != "hey" || len(m.Attachments) != 1 {
		t.Fatalf("dm = %+v", m)
	}
	if data, err := m.Attachments[0].Fetch(ctx); err != nil || string(data) != "HEIC-BYTES" {
		t.Fatalf("attachment = %q %v", data, err)
	}
	if g := msgs[1]; g.Direct || !g.Mentioned || g.Sender != "bo@icloud.com" {
		t.Fatalf("group = %+v", g)
	}

	to := channel.Address{Conversation: m.Conversation}
	if err := ep.Send(ctx, to, ep.Format("**ok**"), "g1"); err != nil {
		t.Fatal(err)
	}
	if err := ep.(channel.Typer).Typing(ctx, to); err != nil {
		t.Fatal(err)
	}
	if err := ep.(channel.FileSender).SendFile(ctx, to, "a.pdf", "application/pdf", []byte("PDF"), "here"); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.texts) != 2 || fake.texts[0]["message"] != "ok" || fake.texts[0]["selectedMessageGuid"] != "g1" ||
		fake.texts[0]["method"] != "private-api" || fake.texts[1]["message"] != "here" {
		t.Fatalf("texts = %v", fake.texts)
	}
	if len(fake.files) != 1 || fake.files[0]["data"] != "PDF" || fake.files[0]["chatGuid"] != m.Conversation || fake.typing != 1 {
		t.Fatalf("files = %v typing = %d", fake.files, fake.typing)
	}
}

func TestPlainAdapterHasNoTyping(t *testing.T) {
	if _, ok := New("http://x", "pw", "s", false, "").WithTyping().(channel.Typer); ok {
		t.Fatal("typing offered without the Private API")
	}
}

func TestWrongPasswordIsFatal(t *testing.T) {
	srv := httptest.NewServer(&fakeServer{})
	defer srv.Close()
	err := New(srv.URL, "nope", "s", false, "").Run(context.Background(), &inbox{up: make(chan string, 1)})
	if !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("err = %v, want Fatal", err)
	}
	if strings.Contains(err.Error(), "nope") {
		t.Fatalf("error leaks the password: %v", err)
	}
}
