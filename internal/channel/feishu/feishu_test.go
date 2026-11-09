package feishu

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

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

type call struct {
	path, token string
	body        map[string]any
	form        map[string]string
}

type fakeFeishu struct {
	mu      sync.Mutex
	tokens  int
	calls   []call
	current string
}

func (f *fakeFeishu) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if in["app_id"] != "cli_1" || in["app_secret"] != "sec" {
			io.WriteString(w, `{"code":10014,"msg":"app secret invalid"}`)
			return
		}
		f.tokens++
		f.current = "t" + string(rune('0'+f.tokens))
		io.WriteString(w, `{"code":0,"msg":"ok","tenant_access_token":"`+f.current+`","expire":7200}`)
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok != f.current {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"code":99991663,"msg":"Invalid access token"}`)
		return
	}
	c := call{path: r.URL.Path, token: tok}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		r.ParseMultipartForm(1 << 20)
		c.form = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			c.form[k] = v[0]
		}
	} else if r.Method == http.MethodPost {
		json.NewDecoder(r.Body).Decode(&c.body)
	}
	if r.URL.RawQuery != "" {
		c.path += "?" + r.URL.RawQuery
	}
	f.calls = append(f.calls, c)
	switch {
	case r.URL.Path == "/open-apis/bot/v3/info":
		io.WriteString(w, `{"code":0,"msg":"ok","bot":{"app_name":"Habakkuk","open_id":"ou_bot"}}`)
	case strings.HasPrefix(r.URL.Path, "/open-apis/im/v1/messages/om_1/resources/img_in"):
		io.WriteString(w, "PNG")
	case r.URL.Path == "/open-apis/im/v1/images":
		io.WriteString(w, `{"code":0,"data":{"image_key":"img_1"}}`)
	case r.URL.Path == "/open-apis/im/v1/files":
		io.WriteString(w, `{"code":0,"data":{"file_key":"file_1"}}`)
	case c.body["receive_id"] == "oc_gone":
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"code":230002,"msg":"Bot/User can NOT be out of the chat."}`)
	default:
		io.WriteString(w, `{"code":0,"msg":"success","data":{"message_id":"om_out"}}`)
	}
}

func (f *fakeFeishu) take() []call {
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

func (i *inbox) take() []channel.Message {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := i.msgs
	i.msgs = nil
	return out
}

const key = "enc-key"

// send posts an event the way Feishu does with an Encrypt Key set.
func send(a *Adapter, plain string, sign bool) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"encrypt": Encrypt(key, []byte(plain))})
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	if sign {
		r.Header.Set("X-Lark-Request-Timestamp", "1700000000")
		r.Header.Set("X-Lark-Request-Nonce", "n0nce")
		r.Header.Set("X-Lark-Signature", Signature("1700000000", "n0nce", key, body))
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}

func event(token, chatType, chat, msgType, content, mentions, thread string) string {
	c, _ := json.Marshal(content)
	return `{"schema":"2.0","header":{"event_id":"ev1","event_type":"im.message.receive_v1","token":"` + token + `","app_id":"cli_1"},
"event":{"sender":{"sender_id":{"open_id":"ou_ay","user_id":"ay"},"sender_type":"user"},
"message":{"message_id":"om_1","chat_id":"` + chat + `","chat_type":"` + chatType + `","message_type":"` + msgType + `",` + thread + `
"content":` + string(c) + `,"mentions":[` + mentions + `]}}}`
}

func TestFeishu(t *testing.T) {
	fake := &fakeFeishu{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	a := New(Config{AppID: "cli_1", AppSecret: "sec", VerificationToken: "vtok", EncryptKey: key, APIURL: srv.URL})
	in := &inbox{up: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Run(ctx, in) }()
	select {
	case d := <-in.up:
		if d != "Feishu Habakkuk" {
			t.Fatalf("connected as %q", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not connected")
	}
	fake.take()

	// URL verification, encrypted like every other event.
	w := send(a, `{"challenge":"ch-123","token":"vtok","type":"url_verification"}`, false)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"challenge":"ch-123"`) {
		t.Fatalf("challenge: %d %s", w.Code, w.Body)
	}
	if w := send(a, `{"challenge":"ch-123","token":"wrong","type":"url_verification"}`, false); w.Code != http.StatusForbidden {
		t.Fatalf("challenge with a bad token: %d", w.Code)
	}

	dm := event("vtok", "p2p", "oc_dm", "text", `{"text":"hello"}`, "", "")
	if w := send(a, dm, false); w.Code != http.StatusForbidden {
		t.Fatalf("unsigned event: %d", w.Code)
	}
	if w := send(a, event("other", "p2p", "oc_dm", "text", `{"text":"hello"}`, "", ""), true); w.Code != http.StatusForbidden {
		t.Fatalf("wrong token: %d", w.Code)
	}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(dm))
	if w := httptest.NewRecorder(); func() int { a.ServeHTTP(w, r); return w.Code }() != http.StatusForbidden {
		t.Fatal("plaintext event accepted with an Encrypt Key set")
	}
	for _, ev := range []string{
		dm,
		event("vtok", "group", "oc_g", "text", `{"text":"@_user_1 what about @_user_2?"}`,
			`{"key":"@_user_1","id":{"open_id":"ou_bot"},"name":"Habakkuk"},{"key":"@_user_2","id":{"open_id":"ou_bo"},"name":"Bo"}`, ""),
		event("vtok", "group", "oc_g", "text", `{"text":"in the thread"}`, "", `"root_id":"om_root","thread_id":"omt_1",`),
		event("vtok", "p2p", "oc_dm", "post", `{"title":"Report","content":[[{"tag":"text","text":"see "},{"tag":"img","image_key":"img_in"}]]}`, "", ""),
		event("vtok", "p2p", "oc_dm", "sticker", `{"file_key":"x"}`, "", ""),
	} {
		if w := send(a, ev, true); w.Code != http.StatusOK {
			t.Fatalf("event: %d %s", w.Code, w.Body)
		}
	}
	msgs := in.take()
	if len(msgs) != 4 {
		t.Fatalf("delivered %d: %+v", len(msgs), msgs)
	}
	if m := msgs[0]; m.Text != "hello" || !m.Direct || m.Conversation != "oc_dm" || m.Sender != "ou_ay" {
		t.Fatalf("dm = %+v", m)
	}
	if m := msgs[1]; m.Text != "what about @Bo?" || m.Direct || !m.Mentioned || m.Thread != "" {
		t.Fatalf("group = %+v", m)
	}
	if m := msgs[2]; m.Thread != "om_root" || m.Mentioned {
		t.Fatalf("thread = %+v", m)
	}
	if m := msgs[3]; m.Text != "Report\nsee" || len(m.Attachments) != 1 {
		t.Fatalf("post = %+v", m)
	}

	// The token goes stale: one refresh, then the send succeeds.
	fake.mu.Lock()
	fake.current = "revoked"
	fake.mu.Unlock()
	if err := a.Send(channel.WithSendKey(ctx, "job1.0"), channel.Address{Conversation: "oc_dm"}, "**hi**", "om_1"); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, channel.Address{Conversation: "oc_g", Thread: "om_root"}, "in thread", "om_1"); err != nil {
		t.Fatal(err)
	}
	if err := a.SendFile(ctx, channel.Address{Conversation: "oc_dm"}, "c.png", "image/png", []byte("PNG"), "chart"); err != nil {
		t.Fatal(err)
	}
	if err := a.SendFile(ctx, channel.Address{Conversation: "oc_dm"}, "r.pdf", "application/pdf", []byte("%PDF"), ""); err != nil {
		t.Fatal(err)
	}
	calls := fake.take()
	var paths []string
	for _, c := range calls {
		paths = append(paths, c.path)
	}
	want := []string{
		"/open-apis/im/v1/messages?receive_id_type=chat_id",
		"/open-apis/im/v1/messages/om_1/reply",
		"/open-apis/im/v1/images", "/open-apis/im/v1/messages?receive_id_type=chat_id", "/open-apis/im/v1/messages?receive_id_type=chat_id",
		"/open-apis/im/v1/files", "/open-apis/im/v1/messages?receive_id_type=chat_id",
	}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Fatalf("calls:\n got %v\nwant %v", paths, want)
	}
	if fake.tokens != 2 {
		t.Fatalf("fetched %d tokens, want 2", fake.tokens)
	}
	first := calls[0].body
	if first["msg_type"] != "post" || first["receive_id"] != "oc_dm" || first["uuid"] == nil ||
		!strings.Contains(first["content"].(string), `"tag":"md","text":"**hi**"`) {
		t.Fatalf("send = %v", first)
	}
	if calls[1].body["reply_in_thread"] != true {
		t.Fatalf("thread reply = %v", calls[1].body)
	}
	if calls[3].body["msg_type"] != "image" || calls[4].body["msg_type"] != "text" {
		t.Fatalf("image then caption = %v %v", calls[3].body, calls[4].body)
	}
	if calls[5].form["file_type"] != "pdf" || calls[5].form["file_name"] != "r.pdf" || calls[6].body["msg_type"] != "file" {
		t.Fatalf("file = %v %v", calls[5].form, calls[6].body)
	}

	if data, err := msgs[3].Attachments[0].Fetch(ctx); err != nil || string(data) != "PNG" {
		t.Fatalf("resource = %q %v", data, err)
	}
	err := a.Send(ctx, channel.Address{Conversation: "oc_gone"}, "x", "")
	if !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("bot out of chat: %v", err)
	}
}

func TestReplyInThreadAndLark(t *testing.T) {
	a := New(Config{AppSecret: "s", VerificationToken: "vtok", ReplyInThread: true, APIURL: LarkURL + "/"})
	a.botID = "ou_bot"
	in := &inbox{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.listener.Run(ctx, in, "", time.Hour, nil) }()
	for a.listener.Inbox() == nil {
		time.Sleep(time.Millisecond)
	}
	// Without an Encrypt Key events arrive in plain JSON with the token.
	body := event("vtok", "group", "oc_g", "text", `{"text":"@_user_1 hi"}`, `{"key":"@_user_1","id":{"open_id":"ou_bot"},"name":"Habakkuk"}`, "")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	if msgs := in.take(); len(msgs) != 1 || msgs[0].Thread != "om_1" || msgs[0].Text != "hi" {
		t.Fatalf("msgs = %+v", msgs)
	}
	if a.cfg.APIURL != LarkURL {
		t.Fatal(a.cfg.APIURL)
	}
}

func TestBadSecretIsFatal(t *testing.T) {
	srv := httptest.NewServer(&fakeFeishu{})
	defer srv.Close()
	a := New(Config{AppID: "cli_1", AppSecret: "wrong", VerificationToken: "v", APIURL: srv.URL})
	if err := a.Run(context.Background(), &inbox{}); !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("err = %v, want Fatal", err)
	}
}

func TestFormat(t *testing.T) {
	if got := Format("# Title\ntext ## not\n### Sub ###"); got != "**Title**\ntext ## not\n**Sub**" {
		t.Fatalf("Format = %q", got)
	}
}
