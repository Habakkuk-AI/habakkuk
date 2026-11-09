package zulip

import (
	"context"
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

type fakeZulip struct {
	mu      sync.Mutex
	polls   int
	posts   []string // path + form
	deleted bool
}

func (f *fakeZulip) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if u, p, _ := r.BasicAuth(); u != "bot@example.com" || p != "key" {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"result":"error","msg":"Invalid API key","code":"INVALID_API_KEY"}`)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method + " " + r.URL.Path {
	case "GET /api/v1/users/me":
		io.WriteString(w, `{"result":"success","user_id":9,"full_name":"Habakkuk"}`)
	case "POST /api/v1/register":
		r.ParseForm()
		if r.Form.Get("event_types") != `["message"]` {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		io.WriteString(w, `{"result":"success","queue_id":"q1","last_event_id":-1}`)
	case "GET /api/v1/events":
		f.polls++
		switch f.polls {
		case 1:
			if r.URL.Query().Get("last_event_id") != "-1" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			io.WriteString(w, `{"result":"success","events":[
{"type":"message","id":0,"flags":[],"message":{"id":100,"sender_id":5,"sender_full_name":"Ay","type":"private","content":"hello","display_recipient":[{"id":5},{"id":9}]}},
{"type":"message","id":1,"flags":["mentioned"],"message":{"id":101,"sender_id":6,"sender_full_name":"Bo","type":"stream","stream_id":3,"subject":"plans","content":"@**Habakkuk** see [notes.pdf](/user_uploads/2/ab/notes.pdf)","display_recipient":"general"}},
{"type":"message","id":2,"flags":[],"message":{"id":102,"sender_id":9,"type":"private","content":"my own","display_recipient":[{"id":5},{"id":9}]}},
{"type":"message","id":3,"flags":[],"message":{"id":103,"sender_id":5,"type":"private","content":"group dm","display_recipient":[{"id":5},{"id":7},{"id":9}]}},
{"type":"heartbeat","id":4}]}`)
		case 2:
			if r.URL.Query().Get("last_event_id") != "4" {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"result":"error","msg":"wrong id"}`)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"result":"error","msg":"Bad event queue id: q1","code":"BAD_EVENT_QUEUE_ID"}`)
		}
	case "DELETE /api/v1/events":
		f.deleted = r.URL.Query().Get("queue_id") == "q1"
		io.WriteString(w, `{"result":"success"}`)
	case "GET /user_uploads/2/ab/notes.pdf":
		io.WriteString(w, "%PDF")
	case "POST /api/v1/user_uploads":
		r.ParseMultipartForm(1 << 20)
		f.posts = append(f.posts, "upload "+r.MultipartForm.File["filename"][0].Filename)
		io.WriteString(w, `{"result":"success","uri":"/user_uploads/2/cd/c.png","url":"/user_uploads/2/cd/c.png"}`)
	case "POST /api/v1/messages", "POST /api/v1/typing":
		r.ParseForm()
		if r.Form.Get("to") == "[404]" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"result":"error","msg":"Invalid user ID 404","code":"BAD_REQUEST"}`)
			return
		}
		f.posts = append(f.posts, r.URL.Path+" "+r.Form.Encode())
		io.WriteString(w, `{"result":"success","id":200}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

type inbox struct {
	mu   sync.Mutex
	msgs []channel.Message
}

func (i *inbox) Deliver(_ context.Context, m channel.Message) {
	i.mu.Lock()
	i.msgs = append(i.msgs, m)
	i.mu.Unlock()
}
func (i *inbox) Beat()            {}
func (i *inbox) Connected(string) {}

func TestZulip(t *testing.T) {
	fake := &fakeZulip{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	a := New(srv.URL+"/", "bot@example.com", "key")
	in := &inbox{}
	ctx := context.Background()

	// The second poll says the queue is gone: Run returns so the
	// supervisor registers a new one, and deletes the old queue.
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, in) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "BAD_EVENT_QUEUE_ID") || errors.As(err, new(channel.Fatal)) {
			t.Fatalf("run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not end")
	}
	fake.mu.Lock()
	deleted := fake.deleted
	fake.mu.Unlock()
	if !deleted {
		t.Fatal("queue not deleted")
	}
	msgs := in.msgs
	if len(msgs) != 3 {
		t.Fatalf("delivered %d: %+v", len(msgs), msgs)
	}
	if m := msgs[0]; m.Conversation != "dm:5" || !m.Direct || m.Text != "hello" || m.Sender != "5" || m.SenderName != "Ay" {
		t.Fatalf("dm = %+v", m)
	}
	if m := msgs[1]; m.Conversation != "stream:3" || m.Thread != "plans" || !m.Mentioned || m.Direct ||
		m.Text != "see [notes.pdf](/user_uploads/2/ab/notes.pdf)" || len(m.Attachments) != 1 || m.Attachments[0].Name != "notes.pdf" {
		t.Fatalf("stream = %+v", m)
	}
	if data, err := msgs[1].Attachments[0].Fetch(ctx); err != nil || string(data) != "%PDF" {
		t.Fatalf("upload = %q %v", data, err)
	}
	if m := msgs[2]; m.Conversation != "dm:5,7" || m.Direct || m.Mentioned {
		t.Fatalf("group dm = %+v", m)
	}

	if err := a.Send(ctx, channel.Address{Conversation: "dm:5"}, "hi", "100"); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, channel.Address{Conversation: "stream:3", Thread: "plans"}, "**ok**", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.Typing(ctx, channel.Address{Conversation: "stream:3", Thread: "plans"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Typing(ctx, channel.Address{Conversation: "stream:3", Thread: "plans"}); err != nil { // throttled
		t.Fatal(err)
	}
	if err := a.SendFile(ctx, channel.Address{Conversation: "dm:5"}, "c.png", "image/png", []byte("PNG"), "chart"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/api/v1/messages content=hi&to=%5B5%5D&type=direct",
		"/api/v1/messages content=%2A%2Aok%2A%2A&to=3&topic=plans&type=stream",
		"/api/v1/typing op=start&stream_id=3&topic=plans&type=stream",
		"upload c.png",
		"/api/v1/messages content=chart%0A%5Bc.png%5D%28%2Fuser_uploads%2F2%2Fcd%2Fc.png%29&to=%5B5%5D&type=direct",
	}
	if got := strings.Join(fake.posts, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("posts:\n%s\nwant\n%s", got, strings.Join(want, "\n"))
	}
	if err := a.Send(ctx, channel.Address{Conversation: "dm:404"}, "x", ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("bad user: %v", err)
	}
	if err := a.Send(ctx, channel.Address{Conversation: "elsewhere"}, "x", ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("bad conversation: %v", err)
	}
}

func TestBadKeyIsFatal(t *testing.T) {
	srv := httptest.NewServer(&fakeZulip{})
	defer srv.Close()
	if err := New(srv.URL, "bot@example.com", "nope").Run(context.Background(), &inbox{}); !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("err = %v, want Fatal", err)
	}
}
