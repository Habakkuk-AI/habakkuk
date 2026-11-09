package dingtalk

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

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

type call struct {
	path string
	body map[string]any
}

type fakeDingTalk struct {
	srv    *httptest.Server
	mu     sync.Mutex
	tokens int
	valid  string
	calls  []call
}

func (f *fakeDingTalk) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/v1.0/oauth2/accessToken":
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if in["appKey"] != "ding1" || in["appSecret"] != "sec" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"code":"invalidClientIdOrSecret","message":"wrong secret"}`)
			return
		}
		f.tokens++
		f.valid = "tok" + strconv.Itoa(f.tokens)
		io.WriteString(w, `{"accessToken":"`+f.valid+`","expireIn":7200}`)
		return
	case "/dl/x":
		io.WriteString(w, "JPEG")
		return
	case "/media/upload":
		if r.URL.Query().Get("access_token") != f.valid {
			io.WriteString(w, `{"errcode":40014,"errmsg":"invalid access_token"}`)
			return
		}
		r.ParseMultipartForm(1 << 20)
		f.calls = append(f.calls, call{path: "/media/upload?type=" + r.URL.Query().Get("type") + "&name=" + r.MultipartForm.File["media"][0].Filename})
		io.WriteString(w, `{"errcode":0,"media_id":"@media1","type":"image"}`)
		return
	}
	if r.Header.Get("x-acs-dingtalk-access-token") != f.valid {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"code":"InvalidAuthentication","message":"token expired"}`)
		return
	}
	c := call{path: r.URL.Path}
	json.NewDecoder(r.Body).Decode(&c.body)
	f.calls = append(f.calls, c)
	switch r.URL.Path {
	case "/v1.0/robot/messageFiles/download":
		io.WriteString(w, `{"downloadUrl":"`+f.srv.URL+`/dl/x"}`)
	case "/v1.0/robot/groupMessages/send", "/v1.0/robot/oToMessages/batchSend":
		if c.body["openConversationId"] == "cidGone" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"code":"resource.not.found","message":"robot not in the group"}`)
			return
		}
		io.WriteString(w, `{"processQueryKey":"q1"}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeDingTalk) take() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
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

func hook(a *Adapter, ts, sign, body string) int {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("timestamp", ts)
	r.Header.Set("sign", sign)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w.Code
}

func TestDingTalk(t *testing.T) {
	fake := &fakeDingTalk{}
	fake.srv = httptest.NewServer(fake)
	defer fake.srv.Close()
	a := New(Config{ClientID: "ding1", ClientSecret: "sec", APIURL: fake.srv.URL})
	now := time.UnixMilli(1_800_000_000_000)
	a.Now = func() time.Time { return now }
	in := &inbox{up: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Run(ctx, in) }()
	select {
	case d := <-in.up:
		if d != "DingTalk robot ding1" {
			t.Fatalf("connected as %q", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not connected")
	}

	ts := strconv.FormatInt(now.UnixMilli(), 10)
	dm := `{"conversationId":"cidDM","conversationType":"1","msgId":"m1","msgtype":"text","senderNick":"Ay",
"senderStaffId":"staff1","senderId":"$:enc","robotCode":"ding1","text":{"content":" hello "}}`
	if code := hook(a, ts, Sign("other", ts), dm); code != http.StatusForbidden {
		t.Fatalf("bad signature: %d", code)
	}
	old := strconv.FormatInt(now.Add(-2*time.Hour).UnixMilli(), 10)
	if code := hook(a, old, Sign("sec", old), dm); code != http.StatusForbidden {
		t.Fatalf("stale timestamp: %d", code)
	}
	for _, body := range []string{
		dm,
		`{"conversationId":"cidGroup1","conversationType":"2","msgId":"m2","msgtype":"text","senderNick":"Bo","senderStaffId":"staff2","isInAtList":true,"robotCode":"ding1","text":{"content":" what's up"}}`,
		`{"conversationId":"cidDM","conversationType":"1","msgId":"m3","msgtype":"picture","senderStaffId":"staff1","robotCode":"ding1","content":{"downloadCode":"dc1"}}`,
		`{"conversationId":"cidDM","conversationType":"1","msgId":"m4","msgtype":"audio","senderStaffId":"staff1","robotCode":"ding1","content":{"downloadCode":"dc2","recognition":"spoken words"}}`,
		`{"conversationId":"cidDM","conversationType":"1","msgId":"m5","msgtype":"text","senderStaffId":"staff1","robotCode":"another","text":{"content":"not ours"}}`,
	} {
		if code := hook(a, ts, Sign("sec", ts), body); code != http.StatusOK {
			t.Fatalf("callback: %d", code)
		}
	}
	in.mu.Lock()
	msgs := in.msgs
	in.mu.Unlock()
	if len(msgs) != 4 {
		t.Fatalf("delivered %d: %+v", len(msgs), msgs)
	}
	if m := msgs[0]; m.Text != "hello" || !m.Direct || m.Conversation != "staff1" || m.Sender != "staff1" || m.SenderName != "Ay" {
		t.Fatalf("dm = %+v", m)
	}
	if m := msgs[1]; m.Conversation != "cidGroup1" || m.Direct || !m.Mentioned || m.Text != "what's up" {
		t.Fatalf("group = %+v", m)
	}
	if data, err := msgs[2].Attachments[0].Fetch(ctx); err != nil || string(data) != "JPEG" {
		t.Fatalf("picture = %q %v", data, err)
	}
	if m := msgs[3]; m.Text != "spoken words" || len(m.Attachments) != 0 {
		t.Fatalf("voice = %+v", m)
	}
	fake.take()

	// The token is revoked: one refresh and the send goes through.
	fake.mu.Lock()
	fake.valid = "revoked"
	fake.mu.Unlock()
	if err := a.Send(ctx, channel.Address{Conversation: "staff1"}, "**hi** there", "m1"); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, channel.Address{Conversation: "cidGroup1"}, "# Done\nall good", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.SendFile(ctx, channel.Address{Conversation: "staff1"}, "c.png", "image/png", []byte("PNG"), "chart"); err != nil {
		t.Fatal(err)
	}
	if err := a.SendFile(ctx, channel.Address{Conversation: "staff1"}, "r.pdf", "application/pdf", []byte("%PDF"), ""); err != nil {
		t.Fatal(err)
	}
	calls := fake.take()
	var paths []string
	for _, c := range calls {
		paths = append(paths, c.path)
	}
	want := []string{
		"/v1.0/robot/oToMessages/batchSend", "/v1.0/robot/groupMessages/send",
		"/media/upload?type=image&name=c.png", "/v1.0/robot/oToMessages/batchSend", "/v1.0/robot/oToMessages/batchSend",
		"/media/upload?type=file&name=r.pdf", "/v1.0/robot/oToMessages/batchSend",
	}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Fatalf("calls:\n got %v\nwant %v", paths, want)
	}
	if b := calls[0].body; b["msgKey"] != "sampleMarkdown" || b["robotCode"] != "ding1" || b["msgParam"] != `{"text":"**hi** there","title":"hi there"}` {
		t.Fatalf("dm send = %v", b)
	}
	if ids := calls[0].body["userIds"].([]any); len(ids) != 1 || ids[0] != "staff1" {
		t.Fatalf("user ids = %v", ids)
	}
	if b := calls[1].body; b["openConversationId"] != "cidGroup1" || !strings.Contains(b["msgParam"].(string), `"title":"Done"`) {
		t.Fatalf("group send = %v", b)
	}
	if b := calls[3].body; b["msgKey"] != "sampleImageMsg" || b["msgParam"] != `{"photoURL":"@media1"}` {
		t.Fatalf("image = %v", b)
	}
	if b := calls[6].body; b["msgKey"] != "sampleFile" || !strings.Contains(b["msgParam"].(string), `"fileType":"pdf"`) {
		t.Fatalf("file = %v", b)
	}
	if fake.tokens != 2 {
		t.Fatalf("fetched %d tokens", fake.tokens)
	}
	err := a.Send(ctx, channel.Address{Conversation: "cidGone"}, "x", "")
	if !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("gone group: %v", err)
	}
}

func TestBadSecretIsFatal(t *testing.T) {
	srv := httptest.NewServer(&fakeDingTalk{})
	defer srv.Close()
	a := New(Config{ClientID: "ding1", ClientSecret: "wrong", APIURL: srv.URL})
	if err := a.Run(context.Background(), &inbox{}); !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("err = %v, want Fatal", err)
	}
}
