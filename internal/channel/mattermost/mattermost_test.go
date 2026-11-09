package mattermost

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

type fakeMM struct {
	mu     sync.Mutex
	events []string // sent over the socket after hello
	calls  []string
	posts  []map[string]any
}

func (f *fakeMM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"message":"Invalid or expired session"}`)
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	switch r.URL.Path {
	case "/api/v4/users/me":
		io.WriteString(w, `{"id":"bot1","username":"habakkuk"}`)
	case "/api/v4/websocket":
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		c.Write(ctx, websocket.MessageText, []byte(`{"event":"hello","data":{}}`))
		f.mu.Lock()
		evs := f.events
		f.mu.Unlock()
		for _, e := range evs {
			c.Write(ctx, websocket.MessageText, []byte(e))
		}
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	case "/api/v4/posts":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["channel_id"] == "gone" {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"message":"not a member"}`)
			return
		}
		f.mu.Lock()
		f.posts = append(f.posts, body)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":"p-new"}`)
	case "/api/v4/files":
		r.ParseMultipartForm(1 << 20)
		if r.MultipartForm.Value["channel_id"][0] != "c-dm" || len(r.MultipartForm.File["files"]) != 1 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		io.WriteString(w, `{"file_infos":[{"id":"f-up","name":"r.pdf"}]}`)
	case "/api/v4/files/f1":
		io.WriteString(w, "FILEDATA")
	default:
		io.WriteString(w, `{}`)
	}
}

func event(channelType, sender string, p map[string]any, mentions ...string) string {
	pj, _ := json.Marshal(p)
	mj, _ := json.Marshal(mentions)
	data, _ := json.Marshal(map[string]any{"channel_type": channelType, "sender_name": sender, "post": string(pj), "mentions": string(mj)})
	ev, _ := json.Marshal(map[string]any{"event": "posted", "data": json.RawMessage(data)})
	return string(ev)
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

func TestSocketEventsAndREST(t *testing.T) {
	fake := &fakeMM{events: []string{
		event("D", "@ay", map[string]any{"id": "p1", "user_id": "u1", "channel_id": "c-dm", "message": "hello",
			"file_ids": []string{"f1"}, "metadata": map[string]any{"files": []any{map[string]any{"id": "f1", "name": "a.txt", "mime_type": "text/plain"}}}}),
		event("O", "@bo", map[string]any{"id": "p2", "user_id": "u2", "channel_id": "c-town", "message": "@Habakkuk deploy status?"}, "bot1"),
		event("O", "@bo", map[string]any{"id": "p3", "user_id": "u2", "channel_id": "c-town", "message": "just chatting"}),
		event("O", "@habakkuk", map[string]any{"id": "p4", "user_id": "bot1", "channel_id": "c-town", "message": "my own"}),
		event("O", "@bo", map[string]any{"id": "p5", "user_id": "u2", "channel_id": "c-town", "message": "joined", "type": "system_join_channel"}),
		`{"event":"typing","data":{}}`,
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	a := New(srv.URL, "tok")
	in := &inbox{up: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Run(ctx, in) }()
	select {
	case d := <-in.up:
		if !strings.HasPrefix(d, "@habakkuk on ") {
			t.Fatalf("connected as %q", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not connected")
	}
	var msgs []channel.Message
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		in.mu.Lock()
		msgs = append([]channel.Message(nil), in.msgs...)
		in.mu.Unlock()
		if len(msgs) >= 3 {
			break
		}
	}
	time.Sleep(50 * time.Millisecond)
	in.mu.Lock()
	msgs = in.msgs
	in.mu.Unlock()
	if len(msgs) != 3 {
		t.Fatalf("delivered %d: %+v", len(msgs), msgs)
	}
	if m := msgs[0]; m.Conversation != "c-dm" || !m.Direct || m.Sender != "u1" || m.SenderAlt != "ay" || m.Text != "hello" || len(m.Attachments) != 1 {
		t.Fatalf("dm = %+v", m)
	}
	if data, err := msgs[0].Attachments[0].Fetch(ctx); err != nil || string(data) != "FILEDATA" {
		t.Fatalf("file = %q %v", data, err)
	}
	if m := msgs[1]; m.Direct || !m.Mentioned || m.Thread != "p2" || m.Text != "deploy status?" {
		t.Fatalf("mention = %+v", m)
	}
	if m := msgs[2]; m.Mentioned {
		t.Fatalf("unaddressed channel message marked mentioned: %+v", m)
	}

	// After the bot answers in a thread, follow-ups there count as addressed.
	if err := a.Send(ctx, channel.Address{Conversation: "c-town", Thread: "p2"}, "all green", "p2"); err != nil {
		t.Fatal(err)
	}
	m, ok := a.convert(json.RawMessage(mustData(event("O", "@bo", map[string]any{"id": "p6", "user_id": "u2", "channel_id": "c-town", "root_id": "p2", "message": "thanks"}))))
	if !ok || !m.Mentioned || m.Thread != "p2" {
		t.Fatalf("thread follow-up = %+v", m)
	}
	if err := a.Typing(ctx, channel.Address{Conversation: "c-dm"}); err != nil {
		t.Fatal(err)
	}
	if err := a.SendFile(ctx, channel.Address{Conversation: "c-dm"}, "r.pdf", "application/pdf", []byte("PDF"), "report"); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, channel.Address{Conversation: "gone"}, "hi", ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("not a member: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.posts) != 2 || fake.posts[0]["root_id"] != "p2" || fake.posts[1]["file_ids"].([]any)[0] != "f-up" || fake.posts[1]["message"] != "report" {
		t.Fatalf("posts = %v", fake.posts)
	}
	if !strings.Contains(strings.Join(fake.calls, ","), "POST /api/v4/users/me/typing") {
		t.Fatalf("no typing call: %v", fake.calls)
	}
}

func mustData(ev string) string {
	var e struct {
		Data json.RawMessage `json:"data"`
	}
	json.Unmarshal([]byte(ev), &e)
	return string(e.Data)
}

func TestBadTokenIsFatal(t *testing.T) {
	srv := httptest.NewServer(&fakeMM{})
	defer srv.Close()
	err := New(srv.URL, "nope").Run(context.Background(), &inbox{up: make(chan string, 1)})
	if !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("err = %v, want Fatal", err)
	}
}
