package ntfy

import (
	"context"
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

type published struct {
	method, topic string
	header        http.Header
	body          string
}

type fakeNtfy struct {
	mu     sync.Mutex
	pubs   []published
	sinces []string
	stream []string // JSON lines served on subscribe
}

func (f *fakeNtfy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tk_1" {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":"forbidden"}`)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/json") {
		f.mu.Lock()
		f.sinces = append(f.sinces, r.URL.Query().Get("since"))
		lines := f.stream
		f.stream = nil
		f.mu.Unlock()
		for _, l := range lines {
			fmt.Fprintln(w, l)
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		return
	}
	if strings.HasPrefix(r.URL.Path, "/file/") {
		io.WriteString(w, "ATTACHED")
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.pubs = append(f.pubs, published{r.Method, strings.TrimPrefix(r.URL.Path, "/"), r.Header, string(body)})
	f.mu.Unlock()
	io.WriteString(w, `{"id":"out1","event":"message"}`)
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

func run(t *testing.T, a *Adapter) (*inbox, func()) {
	in := &inbox{up: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, in) }()
	select {
	case <-in.up:
	case err := <-done:
		t.Fatalf("Run: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("not connected")
	}
	return in, func() { cancel(); <-done }
}

func TestSubscribePublishResume(t *testing.T) {
	fake := &fakeNtfy{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	fake.stream = []string{
		`{"id":"open1","event":"open","topic":"habakkuk-in"}`,
		`{"id":"m1","event":"message","topic":"habakkuk-in","title":"Server","message":"disk is at 91%"}`,
		`{"id":"m2","event":"message","topic":"habakkuk-in","message":"my own reply","tags":["robot"]}`,
		`{"id":"k1","event":"keepalive","topic":"habakkuk-in"}`,
		`{"id":"m3","event":"message","topic":"habakkuk-in","message":"You received a file: log.txt","attachment":{"name":"log.txt","type":"text/plain","size":8,"url":"` + srv.URL + `/file/m3.txt"}}`,
	}
	state := filepath.Join(t.TempDir(), "ntfy.json")
	a := New(srv.URL, "habakkuk-out", "habakkuk-in", "tk_1", state)
	in, stop := run(t, a)
	var msgs []channel.Message
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && len(msgs) < 2; time.Sleep(10 * time.Millisecond) {
		in.mu.Lock()
		msgs = append([]channel.Message(nil), in.msgs...)
		in.mu.Unlock()
	}
	stop()
	if len(msgs) != 2 {
		t.Fatalf("delivered %d: %+v", len(msgs), msgs)
	}
	if m := msgs[0]; m.Conversation != "habakkuk-out" || m.Sender != "ntfy" || m.ID != "m1" || m.Text != "Server\n\ndisk is at 91%" || !m.Direct {
		t.Fatalf("message = %+v", m)
	}
	if m := msgs[1]; m.Text != "" || len(m.Attachments) != 1 {
		t.Fatalf("file message = %+v", m)
	}
	if data, err := msgs[1].Attachments[0].Fetch(context.Background()); err != nil || string(data) != "ATTACHED" {
		t.Fatalf("attachment = %q %v", data, err)
	}

	ctx := context.Background()
	to := channel.Address{Conversation: "habakkuk-out"}
	if err := a.Send(ctx, to, "**Cleaned up** 20 GB", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.SendFile(ctx, to, "rapport é.pdf", "application/pdf", []byte("PDF"), "Q3 report"); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	pubs := fake.pubs
	fake.mu.Unlock()
	if len(pubs) != 2 || pubs[0].method != http.MethodPost || pubs[0].topic != "habakkuk-out" || pubs[0].body != "**Cleaned up** 20 GB" ||
		pubs[0].header.Get("Markdown") != "yes" || pubs[0].header.Get("Tags") != "robot" {
		t.Fatalf("publish = %+v", pubs)
	}
	if p := pubs[1]; p.method != http.MethodPut || p.body != "PDF" || !strings.HasPrefix(p.header.Get("Filename"), "=?utf-8?q?") || p.header.Get("Message") != "Q3 report" {
		t.Fatalf("file publish = %+v", p)
	}

	// A restart resumes after the last message seen.
	b := New(srv.URL, "habakkuk-out", "habakkuk-in", "tk_1", state)
	_, stop = run(t, b)
	stop()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sinces) != 2 || fake.sinces[0] != "" || fake.sinces[1] != "m3" {
		t.Fatalf("since = %q", fake.sinces)
	}
}

func TestSendOnlyAndAuth(t *testing.T) {
	fake := &fakeNtfy{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	a := New(srv.URL, "alerts", "", "tk_1", "")
	_, stop := run(t, a)
	stop()
	bad := New(srv.URL, "alerts", "", "wrong", "")
	if err := bad.Send(context.Background(), channel.Address{Conversation: "alerts"}, "x", ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("forbidden publish: %v", err)
	}
	if err := New(srv.URL, "t", "in", "wrong", "").Run(context.Background(), &inbox{up: make(chan string, 1)}); !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("forbidden subscribe: %v", err)
	}
}
