package matrix

import (
	"context"
	"encoding/json"
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

type sent struct {
	Room, Txn string
	Content   map[string]any
}

type fakeHS struct {
	mu      sync.Mutex
	syncs   []string // since values asked for
	s2      int      // calls with since=s2
	joins   []string
	events  map[string]sent // txn -> event, as a server dedupes them
	puts    int
	typing  []string
	uploads []map[string]string
}

func ev(id, sender, typ, content string) string {
	return `{"type":"` + typ + `","event_id":"` + id + `","sender":"` + sender + `","content":` + content + `}`
}

func (f *fakeHS) syncBody(since string) (int, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs = append(f.syncs, since)
	switch since {
	case "":
		return 200, `{"next_batch":"s1","rooms":{
			"invite":{
				"!inv:hs":{"invite_state":{"events":[` + `{"type":"m.room.member","state_key":"@habakkuk:hs","sender":"@sam:hs","content":{"membership":"invite"}}]}},
				"!spam:hs":{"invite_state":{"events":[{"type":"m.room.member","state_key":"@habakkuk:hs","sender":"@stranger:hs","content":{"membership":"invite"}}]}}},
			"join":{"!dm:hs":{"timeline":{"events":[` + ev("$old", "@sam:hs", "m.room.message", `{"msgtype":"m.text","body":"history"}`) + `]}}}}}`
	case "s1":
		return 200, `{"next_batch":"s2","rooms":{"join":{
			"!dm:hs":{"timeline":{"events":[` +
			ev("$1", "@sam:hs", "m.room.message", `{"msgtype":"m.text","body":"hello"}`) + `,` +
			ev("$2", "@habakkuk:hs", "m.room.message", `{"msgtype":"m.text","body":"my own echo"}`) + `,` +
			ev("$3", "@sam:hs", "m.room.message", `{"msgtype":"m.text","body":"* hello!","m.relates_to":{"rel_type":"m.replace","event_id":"$1"}}`) + `]}},
			"!grp:hs":{"summary":{"m.joined_member_count":3},"timeline":{"events":[` +
			ev("$4", "@bo:hs", "m.room.message", `{"msgtype":"m.text","body":"Habakkuk: what's up","m.mentions":{"user_ids":["@habakkuk:hs"]}}`) + `,` +
			ev("$5", "@bo:hs", "m.room.message", `{"msgtype":"m.text","body":"just chatting"}`) + `,` +
			ev("$6", "@bo:hs", "m.room.message", `{"msgtype":"m.image","body":"cat.png","url":"mxc://hs/abc","info":{"mimetype":"image/png","size":3}}`) + `,` +
			ev("$7", "@bo:hs", "m.room.message", `{"msgtype":"m.file","body":"see this","filename":"old.pdf","url":"mxc://old/xyz","m.relates_to":{"rel_type":"m.thread","event_id":"$4"}}`) + `,` +
			ev("$8", "@bo:hs", "m.room.message", `{"msgtype":"m.text","body":"> <@cy:hs> earlier\n\nHabakkuk hi","formatted_body":"<a href=\"https://matrix.to/#/@habakkuk:hs\">Habakkuk</a> hi","m.relates_to":{"m.in_reply_to":{"event_id":"$x"}}}`) + `]}},
			"!enc:hs":{"timeline":{"events":[` +
			ev("$9", "@sam:hs", "m.room.encrypted", `{"algorithm":"m.megolm.v1.aes-sha2","ciphertext":"..."}`) + `,` +
			ev("$10", "@sam:hs", "m.room.encrypted", `{"algorithm":"m.megolm.v1.aes-sha2","ciphertext":"..."}`) + `]}}}}}`
	case "s2":
		f.s2++
		switch f.s2 {
		case 1:
			return 429, `{"errcode":"M_LIMIT_EXCEEDED","error":"Too many requests","retry_after_ms":10}`
		case 2:
			return 401, `{"errcode":"M_UNKNOWN_TOKEN","error":"Token revoked"}`
		}
		return 200, `{"next_batch":"s3","rooms":{"join":{"!enc:hs":{"timeline":{"events":[` +
			ev("$11", "@sam:hs", "m.room.encrypted", `{"algorithm":"m.megolm.v1.aes-sha2"}`) + `]}}}}}`
	}
	return 401, `{"errcode":"M_UNKNOWN_TOKEN","error":"Token revoked"}`
}

func (f *fakeHS) handler() http.Handler {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"errcode":"M_UNKNOWN_TOKEN","error":"Invalid token"}`))
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/_matrix/client/v3/account/whoami", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"user_id":"@habakkuk:hs","device_id":"DEV"}`))
	}))
	mux.HandleFunc("/_matrix/client/v3/profile/{user}/displayname", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"displayname":"Habakkuk"}`))
	}))
	mux.HandleFunc("/_matrix/client/v3/sync", auth(func(w http.ResponseWriter, r *http.Request) {
		code, body := f.syncBody(r.URL.Query().Get("since"))
		w.WriteHeader(code)
		w.Write([]byte(body))
	}))
	mux.HandleFunc("/_matrix/client/v3/join/{room}", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.joins = append(f.joins, r.PathValue("room"))
		f.mu.Unlock()
		w.Write([]byte(`{"room_id":"` + r.PathValue("room") + `"}`))
	}))
	mux.HandleFunc("/_matrix/client/v3/rooms/{room}/joined_members", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("room") == "!dm:hs" {
			w.Write([]byte(`{"joined":{"@habakkuk:hs":{},"@sam:hs":{}}}`))
			return
		}
		w.Write([]byte(`{"joined":{"@habakkuk:hs":{},"@sam:hs":{},"@bo:hs":{}}}`))
	}))
	mux.HandleFunc("/_matrix/client/v3/rooms/{room}/state/m.room.member/{user}", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"membership":"join","displayname":"Bo"}`))
	}))
	mux.HandleFunc("/_matrix/client/v3/rooms/{room}/typing/{user}", auth(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.typing = append(f.typing, r.PathValue("room")+" "+r.PathValue("user")+" "+string(body))
		f.mu.Unlock()
		w.Write([]byte(`{}`))
	}))
	mux.HandleFunc("PUT /_matrix/client/v3/rooms/{room}/send/m.room.message/{txn}", auth(func(w http.ResponseWriter, r *http.Request) {
		room, txn := r.PathValue("room"), r.PathValue("txn")
		switch room {
		case "!gone:hs":
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"errcode":"M_FORBIDDEN","error":"You are not in this room."}`))
			return
		case "!slow:hs":
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"errcode":"M_LIMIT_EXCEEDED","error":"Too many requests","retry_after_ms":2000}`))
			return
		}
		var c map[string]any
		json.NewDecoder(r.Body).Decode(&c)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.puts++
		if _, seen := f.events[txn]; !seen {
			f.events[txn] = sent{Room: room, Txn: txn, Content: c}
		}
		w.Write([]byte(`{"event_id":"$sent-` + txn + `"}`))
	}))
	mux.HandleFunc("POST /_matrix/media/v3/upload", auth(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.uploads = append(f.uploads, map[string]string{"filename": r.URL.Query().Get("filename"),
			"type": r.Header.Get("Content-Type"), "data": string(data)})
		f.mu.Unlock()
		w.Write([]byte(`{"content_uri":"mxc://hs/up1"}`))
	}))
	mux.HandleFunc("/_matrix/client/v1/media/download/{server}/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("server") == "old" {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"errcode":"M_UNRECOGNIZED","error":"Unrecognized request"}`))
			return
		}
		w.Write([]byte("IMG:" + r.PathValue("id")))
	}))
	mux.HandleFunc("/_matrix/media/v3/download/{server}/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("LEGACY:" + r.PathValue("id")))
	})
	return mux
}

func (f *fakeHS) byRoom(room string) []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sent
	for _, s := range f.events {
		if s.Room == room {
			out = append(out, s)
		}
	}
	return out
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

func newFake(t *testing.T) (*fakeHS, string) {
	f := &fakeHS{events: map[string]sent{}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func TestSyncReceivesJoinsAndKeepsPosition(t *testing.T) {
	f, url := newFake(t)
	statePath := filepath.Join(t.TempDir(), "channels", "matrix-mx.json")
	a := New(url, "tok", "@habakkuk:hs", statePath)
	a.Allowed = func(_ context.Context, user string) bool { return user == "@sam:hs" }
	in := &inbox{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Run(ctx, in); !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("run ended with %v", err)
	}
	f.mu.Lock()
	if strings.Join(f.syncs, ",") != ",s1,s2,s2" {
		t.Fatalf("syncs: %q", f.syncs)
	}
	if len(f.joins) != 1 || f.joins[0] != "!inv:hs" {
		t.Fatalf("joins: %v", f.joins)
	}
	f.mu.Unlock()

	in.mu.Lock()
	if in.conn != "@habakkuk:hs" {
		t.Fatalf("connected: %q", in.conn)
	}
	msgs := in.msgs
	in.mu.Unlock()
	if len(msgs) != 6 {
		t.Fatalf("got %d messages: %+v", len(msgs), msgs)
	}
	// Map order of rooms is random; find messages by id.
	byID := map[string]channel.Message{}
	for _, m := range msgs {
		byID[m.ID] = m
	}
	if m := byID["$1"]; !m.Direct || m.Conversation != "!dm:hs" || m.Sender != "@sam:hs" || m.Text != "hello" {
		t.Fatalf("dm: %+v", m)
	}
	if m := byID["$4"]; m.Direct || !m.Mentioned || m.Text != "what's up" || m.SenderName != "Bo" {
		t.Fatalf("mention: %+v", m)
	}
	if m := byID["$5"]; m.Mentioned || m.Direct {
		t.Fatalf("plain group message: %+v", m)
	}
	img := byID["$6"]
	if len(img.Attachments) != 1 || img.Text != "" || img.Attachments[0].Name != "cat.png" || img.Attachments[0].MediaType != "image/png" {
		t.Fatalf("image: %+v", img)
	}
	if data, err := img.Attachments[0].Fetch(ctx); err != nil || string(data) != "IMG:abc" {
		t.Fatalf("authenticated download: %q %v", data, err)
	}
	file := byID["$7"]
	if file.Thread != "$4" || file.Text != "see this" || len(file.Attachments) != 1 || file.Attachments[0].Name != "old.pdf" {
		t.Fatalf("file in thread: %+v", file)
	}
	if data, err := file.Attachments[0].Fetch(ctx); err != nil || string(data) != "LEGACY:xyz" {
		t.Fatalf("legacy download: %q %v", data, err)
	}
	if m := byID["$8"]; !m.Mentioned || m.Text != "Habakkuk hi" {
		t.Fatalf("pill mention with reply fallback: %+v", m)
	}
	enc := f.byRoom("!enc:hs")
	if len(enc) != 1 || enc[0].Content["msgtype"] != "m.notice" || !strings.Contains(enc[0].Content["body"].(string), "encryption") {
		t.Fatalf("encrypted room notices: %+v", enc)
	}

	// A restart resumes from the saved position and does not tell the
	// encrypted room again.
	b := New(url, "tok", "", statePath)
	if err := b.Run(ctx, &inbox{}); !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("second run ended with %v", err)
	}
	f.mu.Lock()
	if got := strings.Join(f.syncs[4:], ","); got != "s2,s3" {
		t.Fatalf("syncs after restart: %q", got)
	}
	f.mu.Unlock()
	if enc := f.byRoom("!enc:hs"); len(enc) != 1 {
		t.Fatalf("encrypted room told %d times", len(enc))
	}
}

func TestWrongUserIsFatal(t *testing.T) {
	_, url := newFake(t)
	err := New(url, "tok", "@other:hs", "").Run(context.Background(), &inbox{})
	if !errors.As(err, new(channel.Fatal)) || !strings.Contains(err.Error(), "@habakkuk:hs") {
		t.Fatalf("err = %v", err)
	}
	if err := New(url, "bad", "", "").Run(context.Background(), &inbox{}); !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("bad token: %v", err)
	}
}

func TestSendIsIdempotentPerDelivery(t *testing.T) {
	f, url := newFake(t)
	a := New(url, "tok", "@habakkuk:hs", "")
	ctx := context.Background()
	long := strings.Repeat("A paragraph of **text**.\n\n", 800) // ~20000 characters
	chunks := channel.Split(long, a.MaxLen()*85/100)
	for i, c := range chunks {
		reply := ""
		if i == 0 {
			reply = "$q"
		}
		key := channel.WithSendKey(ctx, fmt.Sprintf("dlv_1.%d", i))
		if err := a.Send(key, channel.Address{Conversation: "!grp:hs"}, a.Format(c), reply); err != nil {
			t.Fatal(err)
		}
	}
	// A retried delivery sends its first chunk again under the same key.
	if err := a.Send(channel.WithSendKey(ctx, "dlv_1.0"), channel.Address{Conversation: "!grp:hs"}, chunks[0], "$q"); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, channel.Address{Conversation: "!grp:hs", Thread: "$root"}, "in a thread", ""); err != nil {
		t.Fatal(err)
	}
	var ra channel.RetryAfter
	if err := a.Send(ctx, channel.Address{Conversation: "!slow:hs"}, "x", ""); !errors.As(err, &ra) || ra.After != 2*time.Second {
		t.Fatalf("rate limit: %v", err)
	}
	if err := a.Send(ctx, channel.Address{Conversation: "!gone:hs"}, "x", ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("not in room: %v", err)
	}
	if err := a.Typing(ctx, channel.Address{Conversation: "!grp:hs"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(chunks) < 3 || f.puts != len(chunks)+2 || len(f.events) != len(chunks)+1 {
		t.Fatalf("%d chunks, %d puts, %d events", len(chunks), f.puts, len(f.events))
	}
	var first, thread sent
	for _, s := range f.events {
		body := s.Content["body"].(string)
		if len([]rune(body)) > 8000 {
			t.Fatalf("chunk of %d characters", len([]rune(body)))
		}
		if body == "in a thread" {
			thread = s
		}
		if s.Txn == txnID("dlv_1.0") {
			first = s
		}
	}
	rel, _ := first.Content["m.relates_to"].(map[string]any)
	reply, _ := rel["m.in_reply_to"].(map[string]any)
	if reply["event_id"] != "$q" || first.Content["format"] != "org.matrix.custom.html" ||
		!strings.Contains(first.Content["formatted_body"].(string), "<b>text</b>") {
		t.Fatalf("first chunk: %+v", first)
	}
	rel, _ = thread.Content["m.relates_to"].(map[string]any)
	if rel["rel_type"] != "m.thread" || rel["event_id"] != "$root" || rel["is_falling_back"] != true {
		t.Fatalf("thread send: %+v", thread)
	}
	if len(f.typing) != 1 || !strings.HasPrefix(f.typing[0], "!grp:hs @habakkuk:hs ") || !strings.Contains(f.typing[0], `"typing":true`) {
		t.Fatalf("typing: %v", f.typing)
	}
}

func TestSendFileUploadsThenPosts(t *testing.T) {
	f, url := newFake(t)
	a := New(url, "tok", "@habakkuk:hs", "")
	ctx := channel.WithSendKey(context.Background(), "file_1.file")
	if err := a.SendFile(ctx, channel.Address{Conversation: "!dm:hs"}, "chart.png", "image/png", []byte("PNG"), "**Q3**"); err != nil {
		t.Fatal(err)
	}
	if err := a.SendFile(ctx, channel.Address{Conversation: "!gone:hs"}, "a.txt", "text/plain", []byte("a"), ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("not in room: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.uploads) != 2 || f.uploads[0]["filename"] != "chart.png" || f.uploads[0]["type"] != "image/png" || f.uploads[0]["data"] != "PNG" {
		t.Fatalf("uploads: %v", f.uploads)
	}
	s, ok := f.events[txnID("file_1.file")]
	if !ok || s.Room != "!dm:hs" {
		t.Fatalf("events: %v", f.events)
	}
	c := s.Content
	if c["msgtype"] != "m.image" || c["url"] != "mxc://hs/up1" || c["filename"] != "chart.png" || c["body"] != "**Q3**" ||
		!strings.Contains(c["formatted_body"].(string), "<b>Q3</b>") {
		t.Fatalf("file message: %v", c)
	}
}

func TestToHTMLKeepsCodeBlocks(t *testing.T) {
	got := toHTML("line one\nline two\n```\na\nb\n```")
	if got != "line one<br>line two<br><pre>a\nb</pre>" {
		t.Fatalf("html = %q", got)
	}
}
