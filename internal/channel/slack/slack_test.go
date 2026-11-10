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
