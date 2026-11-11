package rocketchat

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

	"github.com/coder/websocket"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

type fakeRC struct {
	mu    sync.Mutex
	got   []string // client frames
	posts []string
}

func (f *fakeRC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/websocket" {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			json.Unmarshal(data, &m)
			f.mu.Lock()
			f.got = append(f.got, string(data))
			f.mu.Unlock()
			switch m["msg"] {
			case "connect":
				c.Write(ctx, websocket.MessageText, []byte(`{"msg":"connected","session":"s1"}`))
			case "method":
				p := m["params"].([]any)[0].(map[string]any)
				if p["resume"] != "tok" {
					c.Write(ctx, websocket.MessageText, []byte(`{"msg":"result","id":"login","error":{"message":"You've been logged out by the server."}}`))
					continue
				}
				c.Write(ctx, websocket.MessageText, []byte(`{"msg":"result","id":"login","result":{"id":"bot1"}}`))
			case "sub":
				c.Write(ctx, websocket.MessageText, []byte(`{"msg":"ready","subs":["sub"]}`))
				c.Write(ctx, websocket.MessageText, []byte(`{"msg":"ping"}`))
				for _, ev := range []string{
					`[{"_id":"m1","rid":"dmroom","msg":"hello","u":{"_id":"u1","username":"sam","name":"Sam"}},{"roomType":"d"}]`,
					`[{"_id":"m2","rid":"GENERAL","msg":"@habakkuk what's up","tmid":"t1","u":{"_id":"u2","username":"bo"},"mentions":[{"_id":"bot1","username":"habakkuk"}]},{"roomType":"c"}]`,
					`[{"_id":"m3","rid":"GENERAL","msg":"see file","u":{"_id":"u2","username":"bo"},"attachments":[{"title":"r.pdf","title_link":"/file-upload/f1/r.pdf","type":"file"}]},{"roomType":"c"}]`,
					`[{"_id":"m4","rid":"GENERAL","msg":"","t":"uj","u":{"_id":"u3","username":"cy"}},{"roomType":"c"}]`,
					`[{"_id":"m5","rid":"dmroom","msg":"mine","u":{"_id":"bot1","username":"habakkuk"}},{"roomType":"d"}]`,
					`[{"_id":"m1","rid":"dmroom","msg":"hello edited","editedAt":{"$date":1},"u":{"_id":"u1","username":"sam"}},{"roomType":"d"}]`,
				} {
					c.Write(ctx, websocket.MessageText, []byte(`{"msg":"changed","collection":"stream-room-messages","id":"id","fields":{"eventName":"__my_messages__","args":`+ev+`}}`))
				}
			}
		}
	}
	if r.Header.Get("X-User-Id") != "bot1" || r.Header.Get("X-Auth-Token") != "tok" {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"status":"error","message":"You must be logged in to do this."}`)
		return
	}
	switch r.URL.Path {
	case "/api/v1/me":
		io.WriteString(w, `{"_id":"bot1","username":"habakkuk","success":true}`)
	case "/file-upload/f1/r.pdf":
		io.WriteString(w, "%PDF")
	case "/api/v1/chat.sendMessage":
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.posts = append(f.posts, string(raw))
		f.mu.Unlock()
		if strings.Contains(string(raw), `"rid":"gone"`) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"success":false,"error":"error-invalid-room"}`)
			return
		}
		io.WriteString(w, `{"success":true}`)
	case "/api/v1/rooms.upload/GENERAL":
		r.ParseMultipartForm(1 << 20)
		f.mu.Lock()
		f.posts = append(f.posts, "upload "+r.MultipartForm.File["file"][0].Filename+" "+r.FormValue("msg")+" "+r.FormValue("tmid"))
		f.mu.Unlock()
		io.WriteString(w, `{"success":true}`)
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

func (i *inbox) count() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.msgs)
}

func TestRocketChat(t *testing.T) {
	fake := &fakeRC{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	a := New(srv.URL, "bot1", "tok")
	in := &inbox{up: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Run(ctx, in) }()
	select {
	case d := <-in.up:
		if d != "Rocket.Chat @habakkuk" {
			t.Fatalf("connected as %q", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not connected")
	}
	deadline := time.Now().Add(5 * time.Second)
	for in.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // anything extra would arrive now
	in.mu.Lock()
	msgs := append([]channel.Message(nil), in.msgs...)
	in.mu.Unlock()
	if len(msgs) != 3 {
		t.Fatalf("delivered %d: %+v", len(msgs), msgs)
	}
	if m := msgs[0]; m.Conversation != "dmroom" || !m.Direct || m.Text != "hello" || m.Sender != "u1" || m.SenderAlt != "sam" {
		t.Fatalf("dm = %+v", m)
	}
	if m := msgs[1]; m.Conversation != "GENERAL" || m.Thread != "t1" || !m.Mentioned || m.Direct || m.Text != "what's up" {
		t.Fatalf("channel = %+v", m)
	}
	if data, err := msgs[2].Attachments[0].Fetch(ctx); err != nil || string(data) != "%PDF" || msgs[2].Attachments[0].Name != "r.pdf" {
		t.Fatalf("file = %q %v", data, err)
	}
	fake.mu.Lock()
	frames := strings.Join(fake.got, "\n")
	fake.mu.Unlock()
	if !strings.Contains(frames, `"msg":"pong"`) || !strings.Contains(frames, `"__my_messages__"`) {
		t.Fatalf("frames:\n%s", frames)
	}

	if err := a.Send(ctx, channel.Address{Conversation: "GENERAL", Thread: "t1"}, "**sure**", "m2"); err != nil {
		t.Fatal(err)
	}
	if err := a.SendFile(ctx, channel.Address{Conversation: "GENERAL", Thread: "t1"}, "c.png", "image/png", []byte("PNG"), "chart"); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	posts := fake.posts
	fake.mu.Unlock()
	if len(posts) != 2 || posts[0] != `{"message":{"msg":"**sure**","rid":"GENERAL","tmid":"t1"}}` || posts[1] != "upload c.png chart t1" {
		t.Fatalf("posts = %q", posts)
	}
	if err := a.Send(ctx, channel.Address{Conversation: "gone"}, "x", ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("gone room: %v", err)
	}
}

func TestBadTokenIsFatal(t *testing.T) {
	srv := httptest.NewServer(&fakeRC{})
	defer srv.Close()
	if err := New(srv.URL, "bot1", "nope").Run(context.Background(), &inbox{}); !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("err = %v, want Fatal", err)
	}
}
