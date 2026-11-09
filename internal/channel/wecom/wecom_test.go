package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

const aesKey = "jWmYm7qr5nMoAUwZRjGtBxmz3KA1tkAj3ykkR6q2B2C"

type call struct {
	path string
	body map[string]any
}

type fakeWeCom struct {
	mu     sync.Mutex
	tokens int
	valid  string
	calls  []call
}

func (f *fakeWeCom) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/cgi-bin/gettoken" {
		if q.Get("corpid") != "corp1" || q.Get("corpsecret") != "sec" {
			io.WriteString(w, `{"errcode":40001,"errmsg":"invalid credential"}`)
			return
		}
		f.tokens++
		f.valid = "tok" + strconv.Itoa(f.tokens)
		io.WriteString(w, `{"errcode":0,"errmsg":"ok","access_token":"`+f.valid+`","expires_in":7200}`)
		return
	}
	if q.Get("access_token") != f.valid {
		io.WriteString(w, `{"errcode":42001,"errmsg":"access_token expired"}`)
		return
	}
	c := call{path: r.URL.Path}
	switch r.URL.Path {
	case "/cgi-bin/media/get":
		w.Header().Set("Content-Type", "image/jpeg")
		io.WriteString(w, "JPEG")
		return
	case "/cgi-bin/media/upload":
		r.ParseMultipartForm(1 << 20)
		c.path += "?type=" + q.Get("type") + "&name=" + r.MultipartForm.File["media"][0].Filename
		f.calls = append(f.calls, c)
		io.WriteString(w, `{"errcode":0,"type":"image","media_id":"mid1"}`)
		return
	}
	json.NewDecoder(r.Body).Decode(&c.body)
	f.calls = append(f.calls, c)
	switch c.body["touser"] {
	case "gone":
		io.WriteString(w, `{"errcode":0,"errmsg":"ok","invaliduser":"gone"}`)
	case "blocked":
		io.WriteString(w, `{"errcode":60020,"errmsg":"not allow to access from your ip"}`)
	default:
		io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}
}

func (f *fakeWeCom) take() []call {
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

func push(a *Adapter, c *Crypt, inner string, tamper bool) int {
	enc := c.Encrypt([]byte(inner))
	sig := c.Signature("1700000000", "n1", enc)
	if tamper {
		sig = c.Signature("1700000001", "n1", enc)
	}
	body := "<xml><ToUserName><![CDATA[corp1]]></ToUserName><AgentID><![CDATA[1000002]]></AgentID><Encrypt><![CDATA[" + enc + "]]></Encrypt></xml>"
	r := httptest.NewRequest(http.MethodPost, "/?msg_signature="+sig+"&timestamp=1700000000&nonce=n1", strings.NewReader(body))
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w.Code
}

func inner(from, typ, extra string) string {
	return "<xml><ToUserName><![CDATA[corp1]]></ToUserName><FromUserName><![CDATA[" + from + "]]></FromUserName>" +
		"<CreateTime>1700000000</CreateTime><MsgType><![CDATA[" + typ + "]]></MsgType>" + extra +
		"<MsgId>" + strconv.Itoa(len(extra)) + typ + "</MsgId><AgentID>1000002</AgentID></xml>"
}

func TestWeCom(t *testing.T) {
	fake := &fakeWeCom{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	a := New(Config{CorpID: "corp1", AgentID: 1000002, Secret: "sec", Token: "cbtok", EncodingAESKey: aesKey, APIURL: srv.URL})
	in := &inbox{up: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Run(ctx, in) }()
	select {
	case d := <-in.up:
		if d != "WeCom app 1000002" {
			t.Fatalf("connected as %q", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not connected")
	}
	c, _ := NewCrypt("cbtok", aesKey, "corp1")

	// URL verification.
	echo := c.Encrypt([]byte("8573529371"))
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/?msg_signature="+c.Signature("1", "2", echo)+"&timestamp=1&nonce=2&echostr="+url.QueryEscape(echo), nil))
	if w.Code != http.StatusOK || w.Body.String() != "8573529371" {
		t.Fatalf("verify: %d %q", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/?msg_signature=bad&timestamp=1&nonce=2&echostr="+url.QueryEscape(echo), nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("bad verify: %d", w.Code)
	}

	if code := push(a, c, inner("ay", "text", "<Content><![CDATA[hello]]></Content>"), true); code != http.StatusForbidden {
		t.Fatalf("tampered: %d", code)
	}
	other, _ := NewCrypt("cbtok", aesKey, "corp2")
	if code := push(a, other, inner("ay", "text", "<Content><![CDATA[hi]]></Content>"), false); code != http.StatusForbidden {
		t.Fatalf("another corp: %d", code)
	}
	for _, m := range []string{
		inner("ay", "text", "<Content><![CDATA[ hello 你好 ]]></Content>"),
		inner("ay", "image", "<PicUrl><![CDATA[http://x]]></PicUrl><MediaId><![CDATA[media1]]></MediaId>"),
		inner("ay", "voice", "<MediaId><![CDATA[media2]]></MediaId><Format><![CDATA[amr]]></Format>"),
		inner("ay", "event", "<Event><![CDATA[enter_agent]]></Event>"),
	} {
		if code := push(a, c, m, false); code != http.StatusOK {
			t.Fatalf("push: %d", code)
		}
	}
	in.mu.Lock()
	msgs := in.msgs
	in.mu.Unlock()
	if len(msgs) != 3 {
		t.Fatalf("delivered %d: %+v", len(msgs), msgs)
	}
	if m := msgs[0]; m.Text != "hello 你好" || !m.Direct || m.Conversation != "ay" {
		t.Fatalf("text = %+v", m)
	}
	if data, err := msgs[1].Attachments[0].Fetch(ctx); err != nil || string(data) != "JPEG" {
		t.Fatalf("image = %q %v", data, err)
	}
	if msgs[2].Attachments[0].Name != "voice.amr" {
		t.Fatalf("voice = %+v", msgs[2])
	}
	fake.take()

	// An expired token is refreshed once.
	fake.mu.Lock()
	fake.valid = "revoked"
	fake.mu.Unlock()
	long := strings.Repeat("字", 1000) // 3000 bytes: two messages
	if err := a.Send(ctx, channel.Address{Conversation: "ay"}, long, ""); err != nil {
		t.Fatal(err)
	}
	if err := a.SendFile(ctx, channel.Address{Conversation: "ay"}, "c.png", "image/png", []byte("PNG"), "chart"); err != nil {
		t.Fatal(err)
	}
	calls := fake.take()
	var paths []string
	for _, c := range calls {
		paths = append(paths, c.path)
	}
	want := "/cgi-bin/message/send /cgi-bin/message/send /cgi-bin/media/upload?type=image&name=c.png /cgi-bin/message/send /cgi-bin/message/send"
	if strings.Join(paths, " ") != want {
		t.Fatalf("calls = %v", paths)
	}
	first := calls[0].body["text"].(map[string]any)["content"].(string)
	if len(first) > 2048 || calls[0].body["agentid"] != float64(1000002) || calls[0].body["msgtype"] != "text" {
		t.Fatalf("first part = %d bytes, %v", len(first), calls[0].body)
	}
	if calls[3].body["image"].(map[string]any)["media_id"] != "mid1" {
		t.Fatalf("image = %v", calls[3].body)
	}

	for _, who := range []string{"gone", "blocked"} {
		err := a.Send(ctx, channel.Address{Conversation: who}, "x", "")
		if !errors.As(err, new(channel.Undeliverable)) {
			t.Fatalf("%s: %v", who, err)
		}
		if who == "blocked" && !strings.Contains(err.Error(), "trusted IPs") {
			t.Fatalf("blocked: %v", err)
		}
	}
}

func TestSplitBytes(t *testing.T) {
	parts := splitBytes(strings.Repeat("ab\n", 10)+strings.Repeat("é", 10), 16)
	var joined string
	for _, p := range parts {
		if len(p) > 16 {
			t.Fatalf("part %q is %d bytes", p, len(p))
		}
		joined += p
	}
	if joined != strings.Repeat("ab\n", 10)+strings.Repeat("é", 10) {
		t.Fatal("split lost text")
	}
}

func TestBadSecretIsFatal(t *testing.T) {
	srv := httptest.NewServer(&fakeWeCom{})
	defer srv.Close()
	a := New(Config{CorpID: "corp1", Secret: "wrong", Token: "t", EncodingAESKey: aesKey, APIURL: srv.URL})
	if err := a.Run(context.Background(), &inbox{}); !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("err = %v, want Fatal", err)
	}
	if err := New(Config{Secret: "s", Token: "t", EncodingAESKey: "nope"}).Run(context.Background(), &inbox{}); !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("bad key: %v", err)
	}
}
