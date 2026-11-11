package discord

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

type fakeDiscord struct {
	mu       sync.Mutex
	wsURL    string
	baseURL  string
	posts    []map[string]any // JSON message posts, with "channel" added
	files    []map[string]string
	typing   []string
	sessions map[string][]map[string]any // gateway path -> frames the client sent
	scripts  map[string]func(ctx context.Context, c *websocket.Conn, got <-chan map[string]any)
}

func (f *fakeDiscord) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v10/gateway/bot", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot tok" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"401: Unauthorized","code":0}`))
			return
		}
		w.Write([]byte(`{"url":"` + f.wsURL + `/ws","shards":1}`))
	})
	mux.HandleFunc("/api/v10/channels/{id}/typing", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.typing = append(f.typing, r.PathValue("id"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v10/channels/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		switch id {
		case "429":
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"message":"You are being rate limited.","retry_after":1.5,"global":false}`))
			return
		case "404":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Unknown Channel","code":10003}`))
			return
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			r.ParseMultipartForm(1 << 20)
			got := map[string]string{"channel": id, "payload_json": r.MultipartForm.Value["payload_json"][0]}
			for field, hs := range r.MultipartForm.File {
				fh, _ := hs[0].Open()
				data, _ := io.ReadAll(fh)
				fh.Close()
				got["field"], got["name"], got["data"], got["type"] = field, hs[0].Filename, string(data), hs[0].Header.Get("Content-Type")
			}
			f.mu.Lock()
			f.files = append(f.files, got)
			f.mu.Unlock()
			w.Write([]byte(`{"id":"900"}`))
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		body["channel"] = id
		f.mu.Lock()
		f.posts = append(f.posts, body)
		f.mu.Unlock()
		w.Write([]byte(`{"id":"901"}`))
	})
	mux.HandleFunc("/cdn/a.png", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("PNGDATA")) })
	gateway := func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		got := make(chan map[string]any, 16)
		go func() {
			for {
				_, data, err := c.Read(ctx)
				if err != nil {
					close(got)
					return
				}
				var fr map[string]any
				json.Unmarshal(data, &fr)
				f.mu.Lock()
				f.sessions[r.URL.Path] = append(f.sessions[r.URL.Path], fr)
				f.mu.Unlock()
				got <- fr
			}
		}()
		f.scripts[r.URL.Path](ctx, c, got)
	}
	mux.HandleFunc("/ws", gateway)
	mux.HandleFunc("/ws-resume", gateway)
	return mux
}

func newFake(t *testing.T) (*fakeDiscord, *Adapter) {
	f := &fakeDiscord{sessions: map[string][]map[string]any{},
		scripts: map[string]func(context.Context, *websocket.Conn, <-chan map[string]any){}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	f.baseURL = srv.URL
	f.wsURL = "ws" + strings.TrimPrefix(srv.URL, "http")
	return f, New("tok", srv.URL+"/api/v10")
}

func write(ctx context.Context, c *websocket.Conn, frames ...string) {
	for _, fr := range frames {
		c.Write(ctx, websocket.MessageText, []byte(fr))
	}
}

// next waits for the client's next frame other than a heartbeat.
func next(got <-chan map[string]any) map[string]any {
	for {
		select {
		case fr, ok := <-got:
			if !ok {
				return nil
			}
			if fr["op"] != float64(opHeartbeat) {
				return fr
			}
		case <-time.After(2 * time.Second):
			return nil
		}
	}
}

func dispatch(s int, t, d string) string {
	return `{"op":0,"s":` + strconv.Itoa(s) + `,"t":"` + t + `","d":` + d + `}`
}

type inbox struct {
	mu    sync.Mutex
	msgs  []channel.Message
	conn  []string
	beats int
}

func (i *inbox) Deliver(_ context.Context, m channel.Message) {
	i.mu.Lock()
	i.msgs = append(i.msgs, m)
	i.mu.Unlock()
}
func (i *inbox) Beat()              { i.mu.Lock(); i.beats++; i.mu.Unlock() }
func (i *inbox) Connected(d string) { i.mu.Lock(); i.conn = append(i.conn, d); i.mu.Unlock() }

func TestGatewayReceivesAndResumesAfterReconnect(t *testing.T) {
	f, a := newFake(t)
	f.scripts["/ws"] = func(ctx context.Context, c *websocket.Conn, got <-chan map[string]any) {
		write(ctx, c, `{"op":10,"d":{"heartbeat_interval":45000}}`)
		if id := next(got); id == nil || id["op"] != float64(opIdentify) {
			t.Errorf("first frame: %v", id)
			return
		}
		write(ctx, c,
			dispatch(1, "READY", `{"session_id":"S1","resume_gateway_url":"`+f.wsURL+`/ws-resume","user":{"id":"99","username":"habakkuk","bot":true}}`),
			dispatch(2, "GUILD_CREATE", `{"id":"G","threads":[{"id":"500","type":11}]}`),
			dispatch(3, "MESSAGE_CREATE", `{"id":"m1","channel_id":"D1","type":0,"author":{"id":"7","username":"sam","global_name":"Sam"},"content":"hello"}`),
			dispatch(4, "MESSAGE_CREATE", `{"id":"m2","channel_id":"C1","guild_id":"G","type":0,"author":{"id":"8","username":"bo"},"member":{"nick":"Bobby"},"content":"<@99> ask <@!12> about it","mentions":[{"id":"99","username":"habakkuk"},{"id":"12","username":"cy"}]}`),
			dispatch(5, "MESSAGE_CREATE", `{"id":"m3","channel_id":"C1","guild_id":"G","type":0,"author":{"id":"8","username":"bo"},"content":"just chatting"}`),
			dispatch(6, "MESSAGE_CREATE", `{"id":"m4","channel_id":"C1","guild_id":"G","type":0,"author":{"id":"50","username":"other","bot":true},"content":"bot noise"}`),
			dispatch(7, "MESSAGE_CREATE", `{"id":"m5","channel_id":"500","guild_id":"G","type":19,"author":{"id":"8","username":"bo"},"content":"look","referenced_message":{"author":{"id":"99"}},"attachments":[{"filename":"a.png","content_type":"image/png","size":7,"url":"`+f.baseURL+`/cdn/a.png"}]}`),
			`{"op":7,"d":null}`)
		for range got {
		}
	}
	f.scripts["/ws-resume"] = func(ctx context.Context, c *websocket.Conn, got <-chan map[string]any) {
		write(ctx, c, `{"op":10,"d":{"heartbeat_interval":45000}}`)
		if fr := next(got); fr == nil || fr["op"] != float64(opResume) {
			t.Errorf("resume frame: %v", fr)
			return
		}
		write(ctx, c,
			dispatch(8, "RESUMED", `{}`),
			dispatch(9, "MESSAGE_CREATE", `{"id":"m6","channel_id":"500","guild_id":"G","type":0,"author":{"id":"8","username":"bo"},"content":"and another"}`))
		c.Close(4004, "Authentication failed.")
	}
	in := &inbox{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Run(ctx, in); err == nil || !strings.Contains(err.Error(), "reconnect") {
		t.Fatalf("first run ended with %v", err)
	}
	if a.session != "S1" || a.seq.Load() != 7 {
		t.Fatalf("session %q seq %d not kept for resume", a.session, a.seq.Load())
	}
	// The bot replies in the thread; later messages there count as addressed to it.
	if err := a.Send(ctx, channel.Address{Conversation: "500"}, "sure", "m5"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx, in); !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("second run ended with %v", err)
	}

	f.mu.Lock()
	resume := f.sessions["/ws-resume"][0]
	f.mu.Unlock()
	d, _ := resume["d"].(map[string]any)
	if d["session_id"] != "S1" || d["seq"] != float64(7) || d["token"] != "tok" {
		t.Fatalf("resume payload: %v", resume)
	}
	ident, _ := f.sessions["/ws"][0]["d"].(map[string]any)
	if ident["intents"] != float64(intents) {
		t.Fatalf("identify: %v", ident)
	}

	in.mu.Lock()
	defer in.mu.Unlock()
	if len(in.conn) != 2 || in.conn[0] != "habakkuk" || in.conn[1] != "habakkuk" {
		t.Fatalf("connected: %v", in.conn)
	}
	if len(in.msgs) != 5 {
		t.Fatalf("got %d messages: %+v", len(in.msgs), in.msgs)
	}
	if m := in.msgs[0]; !m.Direct || m.Conversation != "D1" || m.Sender != "7" || m.SenderName != "Sam" || m.Text != "hello" {
		t.Fatalf("dm: %+v", m)
	}
	if m := in.msgs[1]; m.Direct || !m.Mentioned || m.Text != "ask @cy about it" || m.SenderName != "Bobby" {
		t.Fatalf("mention: %+v", m)
	}
	if m := in.msgs[2]; m.Mentioned {
		t.Fatalf("plain group message marked as mention: %+v", m)
	}
	m := in.msgs[3]
	if !m.Mentioned || m.Conversation != "500" || m.ID != "m5" || len(m.Attachments) != 1 || m.Attachments[0].MediaType != "image/png" {
		t.Fatalf("reply in thread: %+v", m)
	}
	data, err := m.Attachments[0].Fetch(context.Background())
	if err != nil || string(data) != "PNGDATA" {
		t.Fatalf("download: %q %v", data, err)
	}
	if m := in.msgs[4]; !m.Mentioned || m.Conversation != "500" || m.Text != "and another" {
		t.Fatalf("follow-up in the bot's thread after resume: %+v", m)
	}
}

func TestMissedHeartbeatAckReconnects(t *testing.T) {
	f, a := newFake(t)
	f.scripts["/ws"] = func(ctx context.Context, c *websocket.Conn, got <-chan map[string]any) {
		write(ctx, c, `{"op":10,"d":{"heartbeat_interval":40}}`)
		for range got { // never ack
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if err := a.Run(ctx, &inbox{}); !errors.Is(err, errZombie) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("zombie connection noticed too late")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var beats int
	for _, fr := range f.sessions["/ws"] {
		if fr["op"] == float64(opHeartbeat) {
			beats++
			if fr["d"] != nil {
				t.Fatalf("heartbeat before any dispatch carries %v, want null", fr["d"])
			}
		}
	}
	if beats != 1 {
		t.Fatalf("sent %d heartbeats without an ack, want 1", beats)
	}
}

func TestInvalidSessionForgetsSession(t *testing.T) {
	f, a := newFake(t)
	f.scripts["/ws"] = func(ctx context.Context, c *websocket.Conn, got <-chan map[string]any) {
		write(ctx, c, `{"op":10,"d":{"heartbeat_interval":45000}}`)
		next(got)
		write(ctx, c, `{"op":9,"d":false}`)
		for range got {
		}
	}
	a.session, a.resumeURL = "OLD", f.wsURL+"/ws"
	a.seq.Store(42)
	if err := a.Run(context.Background(), &inbox{}); err == nil || !strings.Contains(err.Error(), "invalid session") {
		t.Fatalf("err = %v", err)
	}
	if a.session != "" || a.seq.Load() != 0 {
		t.Fatalf("session %q seq %d kept after a non-resumable invalid session", a.session, a.seq.Load())
	}
	if fr := f.sessions["/ws"][0]; fr["op"] != float64(opResume) {
		t.Fatalf("first frame %v, want a resume attempt", fr)
	}
}

func TestBadTokenIsFatal(t *testing.T) {
	_, a := newFake(t)
	a.Token = "wrong"
	if err := a.Run(context.Background(), &inbox{}); !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("err = %v", err)
	}
}

func TestSendSplitTypingAndRateLimit(t *testing.T) {
	f, a := newFake(t)
	ctx := context.Background()
	long := strings.Repeat("word ", 900) // 4500 characters
	chunks := channel.Split(long, a.MaxLen()*85/100)
	for i, c := range chunks {
		reply := ""
		if i == 0 {
			reply = "m1"
		}
		if err := a.Send(ctx, channel.Address{Conversation: "100"}, a.Format(c), reply); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Send(ctx, channel.Address{Conversation: "100", Thread: "500"}, "in thread", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.Typing(ctx, channel.Address{Conversation: "200"}); err != nil {
		t.Fatal(err)
	}
	var ra channel.RetryAfter
	if err := a.Send(ctx, channel.Address{Conversation: "429"}, "x", ""); !errors.As(err, &ra) || ra.After != 1500*time.Millisecond {
		t.Fatalf("rate limit: %v", err)
	}
	if err := a.Send(ctx, channel.Address{Conversation: "404"}, "x", ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("unknown channel: %v", err)
	}
	if err := a.Send(ctx, channel.Address{Conversation: "nope"}, "x", ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("bad id: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(chunks) < 3 || len(f.posts) != len(chunks)+1 {
		t.Fatalf("%d chunks, %d posts", len(chunks), len(f.posts))
	}
	for _, p := range f.posts {
		if n := len([]rune(p["content"].(string))); n > 2000 {
			t.Fatalf("chunk of %d characters", n)
		}
	}
	first := f.posts[0]
	ref, _ := first["message_reference"].(map[string]any)
	if ref["message_id"] != "m1" || first["allowed_mentions"] == nil || f.posts[1]["message_reference"] != nil {
		t.Fatalf("reply reference: %v", first)
	}
	if last := f.posts[len(f.posts)-1]; last["channel"] != "500" {
		t.Fatalf("thread send went to %v", last["channel"])
	}
	if len(f.typing) != 1 || f.typing[0] != "200" {
		t.Fatalf("typing: %v", f.typing)
	}
}

func TestSendFileMultipart(t *testing.T) {
	f, a := newFake(t)
	ctx := context.Background()
	if err := a.SendFile(ctx, channel.Address{Conversation: "100"}, "chart.png", "image/png", []byte("PNGDATA"), "**Q3**"); err != nil {
		t.Fatal(err)
	}
	if err := a.SendFile(ctx, channel.Address{Conversation: "404"}, "a.txt", "text/plain", []byte("a"), ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("unknown channel: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.files) != 1 {
		t.Fatalf("uploads: %v", f.files)
	}
	up := f.files[0]
	if up["field"] != "files[0]" || up["name"] != "chart.png" || up["data"] != "PNGDATA" || up["type"] != "image/png" {
		t.Fatalf("file part: %v", up)
	}
	var payload map[string]any
	json.Unmarshal([]byte(up["payload_json"]), &payload)
	if payload["content"] != "**Q3**" || payload["attachments"] == nil {
		t.Fatalf("payload: %v", payload)
	}
}
