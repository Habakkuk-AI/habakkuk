package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is the part of the HTTP API the remote client uses: health,
// overview, and a web chat whose agent echoes each message.
type fakeServer struct {
	t     *testing.T
	token string

	mu     sync.Mutex
	events []event
	wake   chan struct{}
	afters []string // the after= of each event stream request
	agents []string // the agent of each chat request
}

func newFakeServer(t *testing.T, token string) (*fakeServer, *httptest.Server) {
	f := &fakeServer{t: t, token: token, wake: make(chan struct{})}
	// History from before the client connected: never printed.
	f.events = []event{
		{Seq: 1, Role: "user", Text: "old question", At: time.Now().Add(-time.Hour)},
		{Seq: 2, Role: "assistant", Text: "old answer", At: time.Now().Add(-time.Hour)},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"degraded","channels":[],"queue":{"pending":1,"running":0}}`)
	})
	mux.HandleFunc("GET /api/overview", f.auth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"channels":[{"account":"tg","type":"telegram","state":"connected","detail":"@hk_bot"},
			{"account":"mail","type":"email","state":"failed","last_error":"login refused\nmore"}],
			"queue":{"pending":1,"running":2},"running":[],
			"errors":[{"kind":"turn","agent":"habakkuk","error":"model timed out"}],
			"approvals":1,"open_cards":3,"schedules":{"total":4,"enabled":3}}`)
	}))
	mux.HandleFunc("POST /api/chat", f.auth(f.chat))
	mux.HandleFunc("GET /api/chat/{conversation}/events", f.auth(f.stream))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"missing or wrong token"}`)
			return
		}
		next(w, r)
	}
}

func (f *fakeServer) add(role, text string) {
	f.mu.Lock()
	f.events = append(f.events, event{Seq: int64(len(f.events) + 1), Role: role, Text: text, At: time.Now()})
	close(f.wake)
	f.wake = make(chan struct{})
	f.mu.Unlock()
}

func (f *fakeServer) chat(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != "application/json" {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}
	var req struct{ Conversation, Text, Agent string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Conversation == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if req.Text == "fail" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"text is empty"}`)
		return
	}
	f.mu.Lock()
	f.agents = append(f.agents, req.Agent)
	f.mu.Unlock()
	f.add("user", req.Text)
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.add("assistant", "echo: "+req.Text)
	}()
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprint(w, `{"message":"web_1","turn":"turn_1"}`)
}

// stream serves the events after ?after=N, then new ones, and ends after
// a short while so the client has to reconnect (as behind a proxy).
func (f *fakeServer) stream(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	f.mu.Lock()
	f.afters = append(f.afters, r.URL.Query().Get("after"))
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")
	end := time.After(150 * time.Millisecond)
	for {
		f.mu.Lock()
		evs := f.events[min(int(after), len(f.events)):]
		wake := f.wake
		f.mu.Unlock()
		for _, e := range evs {
			data, _ := json.Marshal(e)
			fmt.Fprintf(w, "id: %d\nevent: message\ndata: %s\n\n", e.Seq, data)
			after = e.Seq
		}
		fmt.Fprint(w, "event: typing\ndata: {}\n\n: keepalive\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-wake:
		case <-end:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func testClient(srv *httptest.Server, token string) *Client {
	c, err := NewClient(Remote{Target: srv.URL, TokenEnv: "HK_TEST_TOKEN"}, func(string) string { return token })
	if err != nil {
		panic(err)
	}
	c.Retry = 10 * time.Millisecond
	c.ReplyWait = 5 * time.Second
	return c
}

func TestHTTPStatus(t *testing.T) {
	_, srv := newFakeServer(t, "s3cret")
	var out bytes.Buffer
	if err := testClient(srv, "s3cret").Status(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Health:    degraded", "Queue:     1 pending, 2 running", "Approvals: 1 waiting", "Board:     3 open cards",
		"Schedules: 3 of 4 enabled", "tg (telegram): connected, @hk_bot", "mail (email): failed; last error: login refused\n",
		"Errors in the last 7 days: 1", "turn habakkuk: model timed out",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status output lacks %q:\n%s", want, out.String())
		}
	}
	err := testClient(srv, "wrong").Status(context.Background(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "refused the token") {
		t.Fatalf("wrong token: %v", err)
	}
	c := testClient(srv, "x")
	c.Token = ""
	if err := c.Status(context.Background(), io.Discard); err == nil || !strings.Contains(err.Error(), "-token-env") {
		t.Fatalf("no token: %v", err)
	}
}

func TestHTTPChatPiped(t *testing.T) {
	f, srv := newFakeServer(t, "s3cret")
	var out, errOut bytes.Buffer
	in := strings.NewReader("hello there\n\nfail\nsecond $msg; 'quoted'\n")
	o := ChatOptions{Conversation: "cli-test", Agent: "ops"}
	if err := testClient(srv, "s3cret").Chat(context.Background(), o, Stdio{In: in, Out: &out, Err: &errOut}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "old answer") || strings.Contains(got, "old question") {
		t.Errorf("history was printed:\n%s", got)
	}
	if want := "echo: hello there\necho: second $msg; 'quoted'\n"; got != want {
		t.Errorf("piped lines are turns in order; got\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "> ") {
		t.Errorf("no prompt when piped:\n%s", got)
	}
	if !strings.Contains(errOut.String(), "text is empty") {
		t.Errorf("a refused message should be reported: %q", errOut.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.afters) == 0 || f.afters[0] != "0" {
		t.Fatalf("stream requests: %q", f.afters)
	}
	for _, a := range f.agents {
		if a != "ops" {
			t.Fatalf("agent %q, want ops", a)
		}
	}
}

func TestHTTPChatReconnects(t *testing.T) {
	f, srv := newFakeServer(t, "")
	c := testClient(srv, "unused")
	c.Token = ""
	pr, pw := io.Pipe()
	var mu sync.Mutex
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- c.Chat(context.Background(), ChatOptions{Conversation: "c1", Interactive: true},
			Stdio{In: pr, Out: lockedWriter{&mu, &out}, Err: io.Discard})
	}()
	fmt.Fprintln(pw, "one")
	time.Sleep(400 * time.Millisecond) // past the stream's end: the client reconnects
	fmt.Fprintln(pw, "two")
	time.Sleep(200 * time.Millisecond)
	fmt.Fprintln(pw, "/exit")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := out.String()
	mu.Unlock()
	if strings.Count(got, "echo: one") != 1 || strings.Count(got, "echo: two") != 1 {
		t.Fatalf("each reply once:\n%s", got)
	}
	if !strings.Contains(got, "> ") || !strings.Contains(got, "Conversation c1") {
		t.Fatalf("interactive chat shows a prompt:\n%s", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.afters) < 2 || f.afters[len(f.afters)-1] == "0" {
		t.Fatalf("reconnects should resume after the last event: %q", f.afters)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestRunHTTPCommands(t *testing.T) {
	_, srv := newFakeServer(t, "")
	rem := Remote{Target: srv.URL}
	for _, c := range []struct {
		args string
		want string
	}{
		{"send -to tg:1 hi", "send needs an ssh remote; web is an http remote, which runs only status and chat"},
		{"retro", "needs an ssh remote"},
		{"tui", "hk -r web chat"},
		{"chat -session ses_1", "-session and -resume need an ssh remote"},
		{"chat -conversation bad/id", "-conversation"},
		{"status extra", "usage"},
	} {
		code, err := RunHTTP(context.Background(), "web", rem, strings.Fields(c.args), Stdio{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard}, false)
		if err == nil || code == 0 || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: code %d, err %v, want %q", c.args, code, err, c.want)
		}
	}
	var out bytes.Buffer
	code, err := RunHTTP(context.Background(), "web", rem, []string{"status"}, Stdio{Out: &out}, false)
	if err != nil || code != 0 || !strings.Contains(out.String(), "Health:") {
		t.Fatalf("status: %d %v %s", code, err, out.String())
	}
	t.Setenv("HK_UNSET_TOKEN", "")
	_, err = RunHTTP(context.Background(), "web", Remote{Target: srv.URL, TokenEnv: "HK_UNSET_TOKEN"}, []string{"status"}, Stdio{Out: io.Discard}, false)
	if err == nil || !strings.Contains(err.Error(), "set HK_UNSET_TOKEN") {
		t.Fatalf("unset token variable: %v", err)
	}
}

func TestReadEvents(t *testing.T) {
	stream := "retry: 3000\n\n: keepalive\n\nevent: typing\ndata: {}\n\n" +
		"id: 5\nevent: message\ndata: {\"seq\":5,\"role\":\"assistant\",\"text\":\"a\"}\n\n" +
		"id: 4\nevent: message\ndata: {\"seq\":4,\"role\":\"assistant\",\"text\":\"old\"}\n\n" +
		"data: {\"seq\":6,\"role\":\"user\",\r\ndata: \"text\":\"b\"}\r\n\r\n" +
		"event: message\ndata: {\"seq\":7,\"role\":\"assistant\",\"text\":\"cut"
	var got []string
	last := readEvents(strings.NewReader(stream), 4, func(e event) { got = append(got, e.Role+":"+e.Text) })
	if last != 6 || strings.Join(got, ",") != "assistant:a,user:b" {
		t.Fatalf("last %d, got %q", last, got)
	}
}

func TestDefaultConversation(t *testing.T) {
	c := DefaultConversation()
	if !strings.HasPrefix(c, "cli-") || !conversationRE.MatchString(c) {
		t.Fatalf("DefaultConversation() = %q", c)
	}
}
