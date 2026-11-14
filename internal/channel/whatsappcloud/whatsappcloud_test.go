package whatsappcloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/metagraph"
)

type call struct {
	method, path string
	body         map[string]any
	form         map[string]string
}

type fakeGraph struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []call
}

func (f *fakeGraph) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"Invalid OAuth access token","code":190}}`)
		return
	}
	c := call{method: r.Method, path: r.URL.Path}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		r.ParseMultipartForm(1 << 20)
		c.form = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			c.form[k] = v[0]
		}
		if fh := r.MultipartForm.File["file"]; len(fh) == 1 {
			c.form["file"] = fh[0].Filename + ";" + fh[0].Header.Get("Content-Type")
		}
	} else if r.Method == http.MethodPost {
		json.NewDecoder(r.Body).Decode(&c.body)
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	switch {
	case r.URL.Path == "/PN1" && r.Method == http.MethodGet:
		io.WriteString(w, `{"display_phone_number":"+1 555 0100","verified_name":"Habakkuk","id":"PN1"}`)
	case r.URL.Path == "/MEDIA1":
		io.WriteString(w, `{"url":"`+f.srv.URL+`/dl/MEDIA1","mime_type":"image/jpeg"}`)
	case r.URL.Path == "/dl/MEDIA1":
		io.WriteString(w, "JPEG")
	case r.URL.Path == "/PN1/media":
		io.WriteString(w, `{"id":"up1"}`)
	case r.URL.Path == "/PN1/messages":
		if c.body["to"] == "15550000002" && c.body["type"] != "template" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"Re-engagement message","code":131047,"error_data":{"details":"more than 24 hours"}}}`)
			return
		}
		io.WriteString(w, `{"messages":[{"id":"wamid.out"}]}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"message":"unknown path","code":100}}`)
	}
}

func (f *fakeGraph) got() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func (f *fakeGraph) reset() {
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
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

func start(t *testing.T, cfg Config) (*Adapter, *fakeGraph, *inbox, *time.Time) {
	t.Helper()
	fake := &fakeGraph{}
	fake.srv = httptest.NewServer(fake)
	t.Cleanup(fake.srv.Close)
	cfg.PhoneNumberID, cfg.Token, cfg.AppSecret, cfg.VerifyToken, cfg.APIURL = "PN1", "tok", "appsecret", "verify-me", fake.srv.URL
	if cfg.StatePath == "" {
		cfg.StatePath = filepath.Join(t.TempDir(), "whatsappcloud-wa.json")
	}
	a := New(cfg)
	now := time.Unix(1_800_000_000, 0)
	a.Now = func() time.Time { return now }
	in := &inbox{up: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = a.Run(ctx, in) }()
	select {
	case d := <-in.up:
		if d != "WhatsApp Habakkuk +1 555 0100" {
			t.Fatalf("connected as %q", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not connected")
	}
	fake.reset()
	return a, fake, in, &now
}

func post(a *Adapter, secret, body string) int {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("X-Hub-Signature-256", metagraph.Sign(secret, []byte(body)))
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w.Code
}

func inbound(from, id, ts, msg string) string {
	return `{"object":"whatsapp_business_account","entry":[{"id":"WABA","changes":[{"field":"messages","value":{
"messaging_product":"whatsapp","metadata":{"display_phone_number":"15550100","phone_number_id":"PN1"},
"contacts":[{"profile":{"name":"Sam"},"wa_id":"` + from + `"}],
"messages":[{"from":"` + from + `","id":"` + id + `","timestamp":"` + ts + `",` + msg + `}]}}]}]}`
}

func TestHandshake(t *testing.T) {
	a := New(Config{VerifyToken: "verify-me"})
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/?hub.mode=subscribe&hub.verify_token=verify-me&hub.challenge=12345", nil))
	if w.Code != http.StatusOK || w.Body.String() != "12345" {
		t.Fatalf("handshake: %d %q", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/?hub.mode=subscribe&hub.verify_token=nope&hub.challenge=12345", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("wrong token: %d", w.Code)
	}
}

func TestInboundAndSend(t *testing.T) {
	a, fake, in, now := start(t, Config{})
	ctx := context.Background()
	ts := "1800000000"
	body := `{"object":"whatsapp_business_account","entry":[{"id":"WABA","changes":[{"field":"messages","value":{
"messaging_product":"whatsapp","metadata":{"phone_number_id":"PN1"},
"contacts":[{"profile":{"name":"Sam"},"wa_id":"15550000001"}],
"messages":[
 {"from":"15550000001","id":"wamid.1","timestamp":"` + ts + `","type":"text","text":{"body":" hello "}},
 {"from":"15550000001","id":"wamid.2","timestamp":"` + ts + `","type":"image","image":{"id":"MEDIA1","mime_type":"image/jpeg","caption":"look"}},
 {"from":"15550000001","id":"wamid.3","timestamp":"` + ts + `","type":"audio","audio":{"id":"MEDIA1","mime_type":"audio/ogg; codecs=opus","voice":true}},
 {"from":"15550000001","id":"wamid.4","timestamp":"` + ts + `","type":"interactive","interactive":{"type":"button_reply","button_reply":{"id":"cb:1|0","title":"Approve"}}},
 {"from":"15550000001","id":"wamid.5","timestamp":"` + ts + `","type":"reaction","reaction":{"message_id":"x","emoji":"👍"}}
]}}]},
{"id":"WABA","changes":[{"field":"messages","value":{"metadata":{"phone_number_id":"OTHER"},
"messages":[{"from":"15550000009","id":"wamid.9","timestamp":"` + ts + `","type":"text","text":{"body":"not ours"}}]}}]}]}`
	if code := post(a, "wrong", body); code != http.StatusForbidden {
		t.Fatalf("bad signature: %d", code)
	}
	if code := post(a, "appsecret", body); code != http.StatusOK {
		t.Fatalf("webhook: %d", code)
	}
	msgs := in.take()
	if len(msgs) != 4 {
		t.Fatalf("delivered %d: %+v", len(msgs), msgs)
	}
	if m := msgs[0]; m.Text != "hello" || !m.Direct || m.Conversation != "15550000001" || m.SenderName != "Sam" || m.ID != "wamid.1" {
		t.Fatalf("text = %+v", m)
	}
	if m := msgs[1]; m.Text != "look" || m.Attachments[0].Name != "image.jpg" {
		t.Fatalf("image = %+v", m)
	} else if data, err := m.Attachments[0].Fetch(ctx); err != nil || string(data) != "JPEG" {
		t.Fatalf("image fetch = %q %v", data, err)
	}
	if m := msgs[2]; m.Attachments[0].Name != "voice.ogg" || !strings.HasPrefix(m.Attachments[0].MediaType, "audio/ogg") {
		t.Fatalf("voice = %+v", m)
	}
	if m := msgs[3]; m.Callback != "cb:1|0" || m.Text != "" {
		t.Fatalf("button = %+v", m)
	}
	fake.reset()

	to := channel.Address{Conversation: "15550000001", ReplyTo: "wamid.1"}
	if err := a.Send(ctx, to, "hi back", "wamid.1"); err != nil {
		t.Fatal(err)
	}
	if err := a.Typing(ctx, to); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(5 * time.Second)
	if err := a.Typing(ctx, to); err != nil { // throttled
		t.Fatal(err)
	}
	if err := a.SendFile(ctx, to, "c.png", "image/png", []byte("PNG"), "chart"); err != nil {
		t.Fatal(err)
	}
	if err := a.SendFile(ctx, to, "r.pdf", "application/pdf", []byte("%PDF"), ""); err != nil {
		t.Fatal(err)
	}
	if err := a.SendVoice(ctx, to, []byte("OGG"), 3, "said"); err != nil {
		t.Fatal(err)
	}
	if err := a.SendButtons(ctx, to, "Run it?", [][]channel.Button{{{Text: "Approve", Data: "a"}, {Text: "Deny", Data: "d"}}}); err != nil {
		t.Fatal(err)
	}
	if err := a.SendButtons(ctx, to, "Pick", [][]channel.Button{{{Text: "1", Data: "1"}, {Text: "2", Data: "2"}}, {{Text: "3", Data: "3"}, {Text: "4", Data: "4"}}}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range fake.got() {
		s := c.path
		if c.body != nil {
			s += ":" + str(c.body["type"]) + str(c.body["status"])
		}
		if c.form != nil {
			s += ":" + c.form["type"] + "," + c.form["file"]
		}
		got = append(got, s)
	}
	want := []string{
		"/PN1/messages:text", "/PN1/messages:read",
		"/PN1/media:image/png,c.png;image/png", "/PN1/messages:image",
		"/PN1/media:application/pdf,r.pdf;application/pdf", "/PN1/messages:document",
		"/PN1/media:audio/ogg,voice.ogg;audio/ogg", "/PN1/messages:audio", "/PN1/messages:text",
		"/PN1/messages:interactive", "/PN1/messages:interactive",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("calls:\n got %v\nwant %v", got, want)
	}
	calls := fake.got()
	if ctxID := calls[0].body["context"].(map[string]any)["message_id"]; ctxID != "wamid.1" {
		t.Fatalf("reply context = %v", ctxID)
	}
	if calls[1].body["message_id"] != "wamid.1" || calls[1].body["typing_indicator"] == nil {
		t.Fatalf("typing = %v", calls[1].body)
	}
	if img := calls[3].body["image"].(map[string]any); img["id"] != "up1" || img["caption"] != "chart" {
		t.Fatalf("image = %v", img)
	}
	if doc := calls[5].body["document"].(map[string]any); doc["filename"] != "r.pdf" {
		t.Fatalf("document = %v", doc)
	}
	if typ := calls[9].body["interactive"].(map[string]any)["type"]; typ != "button" {
		t.Fatalf("two choices as %v", typ)
	}
	if typ := calls[10].body["interactive"].(map[string]any)["type"]; typ != "list" {
		t.Fatalf("four choices as %v", typ)
	}

	// A day later the window has closed: with no template the send fails
	// for good, with the reason.
	fake.reset()
	*now = now.Add(25 * time.Hour)
	err := a.Send(ctx, to, "late", "")
	if !errors.As(err, new(channel.Undeliverable)) || !strings.Contains(err.Error(), "24 hours") {
		t.Fatalf("outside window: %v", err)
	}
	if err := a.Typing(ctx, to); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.got()); n != 0 {
		t.Fatalf("%d calls outside the window", n)
	}
}

func TestTemplateOutsideWindow(t *testing.T) {
	a, fake, _, _ := start(t, Config{Template: "come_back", TemplateLanguage: "en"})
	ctx := context.Background()
	to := channel.Address{Conversation: "15550000001"}
	for range 2 {
		err := a.Send(ctx, to, "the answer", "")
		if !errors.As(err, new(channel.Undeliverable)) || !strings.Contains(err.Error(), `"come_back"`) {
			t.Fatalf("invite: %v", err)
		}
	}
	calls := fake.got()
	if len(calls) != 1 || calls[0].body["type"] != "template" {
		t.Fatalf("calls = %+v", calls)
	}
	tmpl := calls[0].body["template"].(map[string]any)
	if tmpl["name"] != "come_back" || tmpl["components"] != nil {
		t.Fatalf("template = %v", tmpl)
	}

	// The customer writes back: the window opens and the invitation resets.
	fake.reset()
	if code := post(a, "appsecret", inbound("15550000001", "wamid.7", "1800000000", `"type":"text","text":{"body":"ok"}`)); code != http.StatusOK {
		t.Fatal(code)
	}
	if err := a.Send(ctx, to, "the answer", ""); err != nil {
		t.Fatal(err)
	}
	if calls := fake.got(); len(calls) != 1 || calls[0].body["type"] != "text" {
		t.Fatalf("calls = %+v", calls)
	}

	// The state survives a restart.
	b := New(a.cfg)
	b.Now = a.Now
	if err := b.load(); err != nil || !b.InWindow("15550000001") || b.InWindow("15550000002") {
		t.Fatalf("reloaded state: %v %+v", err, b.state)
	}
}

func TestTemplateCarriesText(t *testing.T) {
	a, fake, _, _ := start(t, Config{Template: "reply", TemplateText: true})
	ctx := context.Background()
	// WhatsApp says the window closed though Habakkuk thought it open.
	if code := post(a, "appsecret", inbound("15550000002", "wamid.8", "1800000000", `"type":"text","text":{"body":"hi"}`)); code != http.StatusOK {
		t.Fatal(code)
	}
	if err := a.Send(ctx, channel.Address{Conversation: "15550000002"}, "line one\n\nline    two", ""); err != nil {
		t.Fatal(err)
	}
	calls := fake.got()
	if len(calls) != 2 || calls[0].body["type"] != "text" || calls[1].body["type"] != "template" {
		t.Fatalf("calls = %+v", calls)
	}
	tmpl := calls[1].body["template"].(map[string]any)
	param := tmpl["components"].([]any)[0].(map[string]any)["parameters"].([]any)[0].(map[string]any)
	if param["text"] != "line one line two" || tmpl["language"].(map[string]any)["code"] != "en_US" {
		t.Fatalf("template = %v", tmpl)
	}
	if a.InWindow("15550000002") {
		t.Fatal("window still open after 131047")
	}
}

func TestBadTokenIsFatal(t *testing.T) {
	srv := httptest.NewServer(&fakeGraph{})
	defer srv.Close()
	a := New(Config{PhoneNumberID: "PN1", Token: "nope", AppSecret: "s", VerifyToken: "v", APIURL: srv.URL})
	if err := a.Run(context.Background(), &inbox{}); !errors.As(err, new(channel.Fatal)) {
		t.Fatalf("err = %v, want Fatal", err)
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
