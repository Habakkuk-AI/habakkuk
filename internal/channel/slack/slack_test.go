package slack

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

type fakeSlack struct {
	mu      sync.Mutex
	acks    []string
	posts   []map[string]any
	frames  []string // sent after hello
	wsURL   string
	baseURL string
	uploads []map[string]any // the three steps of each file upload, in order
	opens   []string         // the app token of each Socket Mode connection
	// updates records, for each chat.update call, whether the envelope
	// named in ackFirst had been acknowledged by the time the call was
	// answered (the handler waits for it).
	updates  []bool
	ackFirst string
}

// acked reports whether the envelope has been acknowledged.
func (f *fakeSlack) acked(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.acks {
		if a == id {
			return true
		}
	}
	return false
}

func (f *fakeSlack) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth.test", func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer xoxb-a":
			w.Write([]byte(`{"ok":true,"team_id":"TA","team":"Alpha","user_id":"UBOT"}`))
		case "Bearer xoxb-b":
			w.Write([]byte(`{"ok":true,"team_id":"TB","team":"Beta","user_id":"UBOT2"}`))
		default:
			w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
		}
	})
	mux.HandleFunc("/api/apps.connections.open", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.opens = append(f.opens, r.Header.Get("Authorization"))
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true,"url":"` + f.wsURL + `"}`))
	})
	mux.HandleFunc("/api/users.info", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true,"user":{"real_name":"Bo Real","profile":{"display_name":"bo"}}}`))
	})
	mux.HandleFunc("/api/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["channel"] == "CGONE" {
			w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
			return
		}
		body["token"] = r.Header.Get("Authorization")
		f.mu.Lock()
		f.posts = append(f.posts, body)
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/api/chat.update", func(w http.ResponseWriter, r *http.Request) {
		// Hold the answer until the ack arrives (or give up): an adapter
		// that only acks after handling the press can never get it here in
		// time, because it is still waiting for this answer.
		deadline := time.Now().Add(time.Second)
		for !f.acked(f.ackFirst) && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		ok := f.acked(f.ackFirst)
		f.mu.Lock()
		f.updates = append(f.updates, ok)
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/api/files.getUploadURLExternal", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Header.Get("Authorization") == "Bearer xoxb-b" {
			w.Write([]byte(`{"ok":false,"error":"missing_scope"}`))
			return
		}
		f.mu.Lock()
		f.uploads = append(f.uploads, map[string]any{"filename": r.Form.Get("filename"), "length": r.Form.Get("length")})
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true,"upload_url":"` + f.baseURL + `/upload/F1","file_id":"F1"}`))
	})
	mux.HandleFunc("/upload/F1", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.uploads = append(f.uploads, map[string]any{"data": string(data)})
		f.mu.Unlock()
		w.Write([]byte("OK - " + strconv.Itoa(len(data))))
	})
	mux.HandleFunc("/api/files.completeUploadExternal", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.uploads = append(f.uploads, body)
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true,"files":[{"id":"F1"}]}`))
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		c.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","num_connections":1}`))
		go func() {
			for {
				_, data, err := c.Read(ctx)
				if err != nil {
					return
				}
				var ack struct {
					EnvelopeID string `json:"envelope_id"`
				}
				json.Unmarshal(data, &ack)
				f.mu.Lock()
				f.acks = append(f.acks, ack.EnvelopeID)
				f.mu.Unlock()
			}
		}()
		for _, fr := range f.frames {
			c.Write(ctx, websocket.MessageText, []byte(fr))
		}
		<-ctx.Done()
	})
	return mux
}

func envelope(id, team string, event string) string {
	return `{"type":"events_api","envelope_id":"` + id + `","payload":{"team_id":"` + team + `","event":` + event + `}}`
}

type inbox struct {
	mu   sync.Mutex
	msgs []channel.Message
	conn string
}

func (i *inbox) Deliver(_ context.Context, m channel.Message) {
	i.mu.Lock()
	i.msgs = append(i.msgs, m)
	i.mu.Unlock()
}
func (i *inbox) Beat()              {}
func (i *inbox) Connected(d string) { i.mu.Lock(); i.conn = d; i.mu.Unlock() }

func TestSocketModeEvents(t *testing.T) {
	f := &fakeSlack{frames: []string{
		envelope("e1", "TA", `{"type":"message","channel_type":"im","channel":"D1","user":"U1","text":"hi &amp; bye <https://x.io|x>","ts":"1.1"}`),
		envelope("e2", "TB", `{"type":"app_mention","channel":"C2","user":"U2","text":"<@UBOT2> deploy?","ts":"2.2"}`),
		envelope("e3", "TB", `{"type":"message","channel_type":"channel","channel":"C2","user":"U2","text":"<@UBOT2> deploy?","ts":"2.2"}`),
		envelope("e4", "TA", `{"type":"message","channel_type":"im","channel":"D1","user":"UBOT","text":"my own echo","ts":"3.3"}`),
		envelope("e5", "TA", `{"type":"message","subtype":"message_changed","channel_type":"im","channel":"D1","ts":"4.4"}`),
		`{"type":"disconnect","reason":"refresh_requested"}`,
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	f.wsURL = "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	a := New("xapp-1", []string{"xoxb-a", "xoxb-b"}, srv.URL+"/api")
	in := &inbox{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := a.Run(ctx, in)
	if err == nil || !strings.Contains(err.Error(), "refresh_requested") {
		t.Fatalf("run ended with %v", err)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if !strings.Contains(in.conn, "Alpha") || !strings.Contains(in.conn, "Beta") {
		t.Fatalf("connected = %q", in.conn)
	}
	if len(in.msgs) != 2 {
		t.Fatalf("got %d messages: %+v", len(in.msgs), in.msgs)
	}
	if m := in.msgs[0]; !m.Direct || m.Conversation != "TA/D1" || m.Text != "hi & bye x (https://x.io)" {
		t.Fatalf("dm: %+v", m)
	}
	if m := in.msgs[1]; !m.Mentioned || m.Thread != "2.2" || m.Text != "deploy?" || m.SenderName != "bo" {
		t.Fatalf("mention: %+v", m)
	}
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.acks) < 5 {
		t.Fatalf("acked %v", f.acks)
	}
}

func TestListenChannelsAndTopLevelDMs(t *testing.T) {
	f := &fakeSlack{frames: []string{
		// A DM written inside a thread: still answered at the top level.
		envelope("e1", "TA", `{"type":"message","channel_type":"im","channel":"D1","user":"U1","text":"hi","ts":"1.2","thread_ts":"1.1"}`),
		// A listen channel: a top-level message with no mention starts a thread.
		envelope("e2", "TA", `{"type":"message","channel_type":"channel","channel":"CLISTEN","user":"U1","text":"status?","ts":"2.1"}`),
		// ... and a reply in some thread there continues that thread.
		envelope("e3", "TA", `{"type":"message","channel_type":"channel","channel":"CLISTEN","user":"U1","text":"and now?","ts":"2.3","thread_ts":"2.2"}`),
		// A listen entry with a workspace only matches that workspace.
		envelope("e4", "TB", `{"type":"message","channel_type":"channel","channel":"CB","user":"U2","text":"beta","ts":"3.1"}`),
		envelope("e5", "TA", `{"type":"message","channel_type":"channel","channel":"CB","user":"U2","text":"alpha","ts":"3.2"}`),
		// An ordinary channel still needs a mention.
		envelope("e6", "TA", `{"type":"message","channel_type":"channel","channel":"COTHER","user":"U1","text":"chatter","ts":"4.1"}`),
		// A mention in a listen channel comes once, through app_mention.
		envelope("e7", "TA", `{"type":"message","channel_type":"channel","channel":"CLISTEN","user":"U1","text":"<@UBOT> hi","ts":"5.1"}`),
		// Bots in a listen channel are never answered.
		envelope("e8", "TA", `{"type":"message","channel_type":"channel","channel":"CLISTEN","bot_id":"B9","user":"U9","text":"beep","ts":"6.1"}`),
		`{"type":"disconnect","reason":"refresh_requested"}`,
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	f.wsURL = "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	a := New("xapp-1", []string{"xoxb-a", "xoxb-b"}, srv.URL+"/api")
	a.Listen = []string{"CLISTEN", "TB/CB"}
	in := &inbox{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	a.Run(ctx, in)
	in.mu.Lock()
	defer in.mu.Unlock()
	var got []string
	for _, m := range in.msgs {
		got = append(got, m.Conversation+" "+m.Thread+" "+m.Text+" "+strconv.FormatBool(m.Mentioned || m.Direct))
	}
	want := []string{"TA/D1  hi true", "TA/CLISTEN 2.1 status? true", "TA/CLISTEN 2.2 and now? true", "TB/CB 3.1 beta true", "TA/CB  alpha false", "TA/COTHER  chatter false"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Two Slack apps (one per workspace, each with its own app token) run side
// by side in one process: each holds its own Socket Mode connection and
// answers with its own bot token.
func TestTwoAppsSideBySide(t *testing.T) {
	f1 := &fakeSlack{frames: []string{
		envelope("a1", "TA", `{"type":"app_mention","channel":"C1","user":"U1","text":"<@UBOT> one","ts":"1.1"}`),
		`{"type":"disconnect","reason":"refresh_requested"}`,
	}}
	f2 := &fakeSlack{frames: []string{
		envelope("b1", "TB", `{"type":"message","channel_type":"im","channel":"D2","user":"U2","text":"two","ts":"2.1"}`),
		`{"type":"disconnect","reason":"refresh_requested"}`,
	}}
	srv1, srv2 := httptest.NewServer(f1.handler()), httptest.NewServer(f2.handler())
	defer srv1.Close()
	defer srv2.Close()
	f1.wsURL = "ws" + strings.TrimPrefix(srv1.URL, "http") + "/ws"
	f2.wsURL = "ws" + strings.TrimPrefix(srv2.URL, "http") + "/ws"
	a1 := New("xapp-one", []string{"xoxb-a"}, srv1.URL+"/api")
	a2 := New("xapp-two", []string{"xoxb-b"}, srv2.URL+"/api")
	in1, in2 := &inbox{}, &inbox{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, run := range []func(){func() { a1.Run(ctx, in1) }, func() { a2.Run(ctx, in2) }} {
		wg.Add(1)
		go func() { defer wg.Done(); run() }()
	}
	wg.Wait()
	if len(in1.msgs) != 1 || in1.msgs[0].Conversation != "TA/C1" || in1.msgs[0].Text != "one" || in1.conn != "Alpha" {
		t.Fatalf("app one: %q %+v", in1.conn, in1.msgs)
	}
	if len(in2.msgs) != 1 || in2.msgs[0].Conversation != "TB/D2" || in2.msgs[0].Text != "two" || in2.conn != "Beta" {
		t.Fatalf("app two: %q %+v", in2.conn, in2.msgs)
	}
	if len(f1.opens) != 1 || f1.opens[0] != "Bearer xapp-one" || len(f2.opens) != 1 || f2.opens[0] != "Bearer xapp-two" {
		t.Fatalf("socket connections: %v %v", f1.opens, f2.opens)
	}
	if err := a1.Send(ctx, channel.Address{Conversation: "TA/C1", Thread: "1.1"}, "from one", ""); err != nil {
		t.Fatal(err)
	}
	if err := a2.Send(ctx, channel.Address{Conversation: "TB/D2"}, "from two", ""); err != nil {
		t.Fatal(err)
	}
	if p := f1.posts; len(p) != 1 || p[0]["token"] != "Bearer xoxb-a" {
		t.Fatalf("app one posts: %v", p)
	}
	if p := f2.posts; len(p) != 1 || p[0]["token"] != "Bearer xoxb-b" {
		t.Fatalf("app two posts: %v", p)
	}
	// Each app knows only its own workspace.
	if err := a1.Send(ctx, channel.Address{Conversation: "TB/D2"}, "x", ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("app one sending to app two's workspace: %v", err)
	}
}

func TestSendUsesWorkspaceToken(t *testing.T) {
	f := &fakeSlack{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	a := New("xapp-1", []string{"xoxb-a", "xoxb-b"}, srv.URL+"/api")
	ctx := context.Background()
	if err := a.Send(ctx, channel.Address{Conversation: "TB/C2", Thread: "2.2"}, "*hi*", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, channel.Address{Conversation: "TA/CGONE"}, "x", ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("missing channel: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.posts[0]; p["token"] != "Bearer xoxb-b" || p["thread_ts"] != "2.2" || p["channel"] != "C2" {
		t.Fatalf("post: %v", p)
	}
}

func TestSendFileUploadsIntoThread(t *testing.T) {
	f := &fakeSlack{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	f.baseURL = srv.URL
	a := New("xapp-1", []string{"xoxb-a", "xoxb-b"}, srv.URL+"/api")
	ctx := context.Background()
	to := channel.Address{Conversation: "TA/C1", Thread: "5.5"}
	if err := a.SendFile(ctx, to, "report.pdf", "application/pdf", []byte("%PDF-1.7"), "*Q3*"); err != nil {
		t.Fatal(err)
	}
	// Workspace B's token lacks files:write: retrying cannot help.
	if err := a.SendFile(ctx, channel.Address{Conversation: "TB/C2"}, "a.txt", "text/plain", []byte("a"), ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("missing scope: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.uploads) != 3 {
		t.Fatalf("steps: %v", f.uploads)
	}
	if u := f.uploads[0]; u["filename"] != "report.pdf" || u["length"] != "8" {
		t.Fatalf("get upload url: %v", u)
	}
	if u := f.uploads[1]; u["data"] != "%PDF-1.7" {
		t.Fatalf("upload: %v", u)
	}
	done := f.uploads[2]
	files, _ := done["files"].([]any)
	if done["channel_id"] != "C1" || done["thread_ts"] != "5.5" || done["initial_comment"] != "*Q3*" || len(files) != 1 {
		t.Fatalf("complete: %v", done)
	}
	if !a.threads["TA/C1/5.5"] {
		t.Fatal("thread not remembered as one the bot replied in")
	}
}

func TestBadBotTokenIsFatal(t *testing.T) {
	f := &fakeSlack{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	err := New("xapp-1", []string{"xoxb-wrong"}, srv.URL+"/api").Run(context.Background(), &inbox{})
	if !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("err = %v", err)
	}
}

// A button press is acknowledged to Slack before the adapter does anything
// with it, so Slack never shows "this app did not respond", and the press
// still reaches the gateway. That holds for a button whose prompt is long
// gone (posted before a restart): the gateway answers that one in words.
func TestButtonPressIsAckedBeforeItIsHandled(t *testing.T) {
	press := func(id, value string) string {
		return `{"type":"interactive","envelope_id":"` + id + `","payload":{"type":"block_actions","team":{"id":"TA"},
			"user":{"id":"U7","name":"sam"},"channel":{"id":"D1"},"message":{"ts":"5.5","text":"I'd like to run a command."},
			"actions":[{"value":"` + value + `","action_ts":"6.6"}]}}`
	}
	f := &fakeSlack{ackFirst: "i1", frames: []string{
		press("i1", "fy|0123456789ab|0|aaaaaaaaaaaaaaaa"),                                // a prompt from before a restart
		`{"type":"interactive","envelope_id":"i2","payload":{"type":"view_submission"}}`, // nothing the adapter handles
		`{"type":"disconnect","reason":"refresh_requested"}`,
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	f.wsURL = "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	a := New("xapp-1", []string{"xoxb-a"}, srv.URL+"/api")
	in := &inbox{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Run(ctx, in); err == nil || !strings.Contains(err.Error(), "refresh_requested") {
		t.Fatalf("run ended with %v", err)
	}
	in.mu.Lock()
	msgs := append([]channel.Message(nil), in.msgs...)
	in.mu.Unlock()
	if len(msgs) != 1 || msgs[0].Callback != "fy|0123456789ab|0|aaaaaaaaaaaaaaaa" || msgs[0].Sender != "U7" || !msgs[0].Direct {
		t.Fatalf("the press did not reach the gateway: %+v", msgs)
	}
	deadline := time.Now().Add(2 * time.Second)
	for (!f.acked("i1") || !f.acked("i2")) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.updates) != 1 || !f.updates[0] {
		t.Fatalf("the press was handled before it was acknowledged (chat.update calls saw ack: %v)", f.updates)
	}
	for _, id := range []string{"i1", "i2"} {
		found := false
		for _, a := range f.acks {
			found = found || a == id
		}
		if !found {
			t.Fatalf("envelope %s was never acknowledged: %v", id, f.acks)
		}
	}
}
