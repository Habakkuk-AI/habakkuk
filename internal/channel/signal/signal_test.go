package signal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

type rpcCall struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

type fakeDaemon struct {
	mu     sync.Mutex
	calls  []rpcCall
	events []string
}

func (f *fakeDaemon) sent(method string) []rpcCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []rpcCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v1/check":
		w.WriteHeader(http.StatusOK)
		return
	case "/api/v1/events":
		if r.URL.Query().Get("account") != "+1000" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range f.events {
			w.Write([]byte("event:receive\ndata:" + ev + "\n\n"))
		}
		w.Write([]byte(":\n\n")) // keepalive, then the daemon goes away
		return
	}
	var req struct {
		ID json.RawMessage `json:"id"`
		rpcCall
	}
	json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.calls = append(f.calls, req.rpcCall)
	f.mu.Unlock()
	result := func(v string) { w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":` + v + `}`)) }
	recipient := ""
	if rs, ok := req.Params["recipient"].([]any); ok {
		recipient = rs[0].(string)
	}
	switch req.Method {
	case "version":
		result(`{"version":"0.13.9"}`)
	case "getUserStatus":
		result(`[{"recipient":"+1000","number":"+1000","uuid":"bot-uuid","isRegistered":true}]`)
	case "getAttachment":
		result(`{"data":"` + base64.StdEncoding.EncodeToString([]byte("JPEG:"+req.Params["id"].(string))) + `"}`)
	case "send":
		switch recipient {
		case "+1404":
			result(`{"timestamp":5,"results":[{"recipientAddress":{"number":"+1404"},"type":"UNREGISTERED_FAILURE"}]}`)
		case "+1429":
			w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"error":{"code":-5,"message":"Failed to send message due to rate limiting","data":{"response":{"timestamp":0,"results":[{"type":"RATE_LIMIT_FAILURE","retryAfterSeconds":90}]}}}}`))
		default:
			result(`{"timestamp":6,"results":[{"recipientAddress":{"number":"` + recipient + `"},"type":"SUCCESS"}]}`)
		}
	default:
		result(`{}`)
	}
}

func env(source, uuid, name string, ts int, data string) string {
	return `{"envelope":{"source":"` + source + `","sourceNumber":"` + source + `","sourceUuid":"` + uuid + `","sourceName":"` + name +
		`","timestamp":` + itoa(ts) + `,"dataMessage":` + data + `},"account":"+1000"}`
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

type inbox struct {
	mu    sync.Mutex
	msgs  []channel.Message
	conn  string
	beats int
}

func (i *inbox) Deliver(_ context.Context, m channel.Message) {
	i.mu.Lock()
	i.msgs = append(i.msgs, m)
	i.mu.Unlock()
}
func (i *inbox) Beat()              { i.mu.Lock(); i.beats++; i.mu.Unlock() }
func (i *inbox) Connected(d string) { i.mu.Lock(); i.conn = d; i.mu.Unlock() }

func TestReceiveEvents(t *testing.T) {
	group := `"groupInfo":{"groupId":"R3JvdXA=","type":"DELIVER"}`
	f := &fakeDaemon{events: []string{
		env("+1555", "u1", "Ay", 10, `{"timestamp":10,"message":"hello"}`),
		env("+1666", "u2", "Bo", 11, `{"timestamp":11,"message":"￼ what's up","mentions":[{"name":"+1000","number":"+1000","uuid":"bot-uuid","start":0,"length":1}],`+group+`}`),
		env("+1666", "u2", "Bo", 12, `{"timestamp":12,"message":"just chatting",`+group+`}`),
		env("+1666", "u2", "Bo", 13, `{"timestamp":13,"message":"Habakkuk, deploy it",`+group+`}`),
		env("+1666", "u2", "Bo", 14, `{"timestamp":14,"message":"look","quote":{"id":3,"authorNumber":"+1000","authorUuid":"bot-uuid"},"attachments":[{"contentType":"image/jpeg","filename":"p.jpg","id":"att1","size":4}],`+group+`}`),
		env("+1000", "bot-uuid", "Me", 15, `{"timestamp":15,"message":"my own echo"}`),
		`{"envelope":{"sourceNumber":"+1555","sourceUuid":"u1","timestamp":16,"receiptMessage":{"isRead":true}},"account":"+1000"}`,
		`{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"sourceNumber":"","sourceUuid":"u9","sourceName":"Hidden","timestamp":17,"dataMessage":{"message":"no number"}}}}`,
		env("+1666", "u2", "Bo", 18, `{"timestamp":18,"message":"￼ can you help","mentions":[{"name":"Cy","number":"+1777","uuid":"u3","start":0,"length":1}],`+group+`}`),
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	a := New(srv.URL, "+1000", "habakkuk")
	in := &inbox{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.Run(ctx, in); err == nil || !strings.Contains(err.Error(), "stream ended") {
		t.Fatalf("run ended with %v", err)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.conn != "+1000 (signal-cli 0.13.9)" || in.beats == 0 {
		t.Fatalf("connected=%q beats=%d", in.conn, in.beats)
	}
	if len(in.msgs) != 7 {
		t.Fatalf("got %d messages: %+v", len(in.msgs), in.msgs)
	}
	if m := in.msgs[0]; !m.Direct || m.Conversation != "+1555" || m.Sender != "u1" || m.SenderAlt != "1555" || m.ID != "10:+1555" || m.Text != "hello" {
		t.Fatalf("dm: %+v", m)
	}
	if m := in.msgs[1]; m.Direct || !m.Mentioned || m.Conversation != "group.R3JvdXA=" || m.Text != "what's up" {
		t.Fatalf("mention: %+v", m)
	}
	if m := in.msgs[2]; m.Mentioned {
		t.Fatalf("plain group message marked as mention: %+v", m)
	}
	if m := in.msgs[3]; !m.Mentioned || m.Text != "deploy it" {
		t.Fatalf("name prefix: %+v", m)
	}
	m := in.msgs[4]
	if !m.Mentioned || len(m.Attachments) != 1 || m.Attachments[0].Name != "p.jpg" {
		t.Fatalf("reply with photo: %+v", m)
	}
	data, err := m.Attachments[0].Fetch(context.Background())
	if err != nil || string(data) != "JPEG:att1" {
		t.Fatalf("download: %q %v", data, err)
	}
	if get := f.sent("getAttachment"); len(get) != 1 || get[0].Params["groupId"] != "R3JvdXA=" {
		t.Fatalf("getAttachment: %+v", get)
	}
	if m := in.msgs[5]; !m.Direct || m.Conversation != "u9" || m.Sender != "u9" || m.SenderAlt != "" {
		t.Fatalf("hidden number: %+v", m)
	}
	if m := in.msgs[6]; m.Mentioned || m.Text != "@Cy can you help" {
		t.Fatalf("other mention: %+v", m)
	}
}

func TestSendSplitStylesAndFailures(t *testing.T) {
	f := &fakeDaemon{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	a := New(srv.URL, "+1000", "habakkuk")
	ctx := context.Background()
	if err := a.Send(ctx, channel.Address{Conversation: "+1555"}, a.Format("**hi** there"), "10:+1555"); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("line of text\n", 400) // 5200 characters
	chunks := channel.Split(long, a.MaxLen()*85/100)
	for _, c := range chunks {
		if err := a.Send(ctx, channel.Address{Conversation: "group.R3JvdXA="}, a.Format(c), ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Typing(ctx, channel.Address{Conversation: "group.R3JvdXA="}); err != nil {
		t.Fatal(err)
	}
	var ra channel.RetryAfter
	if err := a.Send(ctx, channel.Address{Conversation: "+1429"}, "x", ""); !errors.As(err, &ra) || ra.After != 90*time.Second {
		t.Fatalf("rate limit: %v", err)
	}
	if err := a.Send(ctx, channel.Address{Conversation: "+1404"}, "x", ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("unregistered: %v", err)
	}
	if err := a.Send(ctx, channel.Address{Conversation: "group."}, "x", ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("empty group: %v", err)
	}
	sends := f.sent("send")
	first := sends[0].Params
	styles, _ := first["textStyle"].([]any)
	if first["message"] != "hi there" || len(styles) != 1 || styles[0] != "0:2:BOLD" || first["account"] != "+1000" ||
		first["quoteTimestamp"] != float64(10) || first["quoteAuthor"] != "+1555" {
		t.Fatalf("first send: %v", first)
	}
	if len(chunks) < 3 {
		t.Fatalf("%d chunks", len(chunks))
	}
	for _, s := range sends[1 : 1+len(chunks)] {
		if s.Params["groupId"] != "R3JvdXA=" || len([]rune(s.Params["message"].(string))) > 2000 || s.Params["quoteTimestamp"] != nil {
			t.Fatalf("chunk: %v", s.Params)
		}
	}
	if typing := f.sent("sendTyping"); len(typing) != 1 || typing[0].Params["groupId"] != "R3JvdXA=" {
		t.Fatalf("typing: %+v", typing)
	}
}

func TestSendFileAsDataURI(t *testing.T) {
	f := &fakeDaemon{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	a := New(srv.URL, "+1000", "habakkuk")
	if err := a.SendFile(context.Background(), channel.Address{Conversation: "+1555"}, "chart.png", "image/png", []byte("PNG"), "*Q3*"); err != nil {
		t.Fatal(err)
	}
	p := f.sent("send")[0].Params
	atts, _ := p["attachments"].([]any)
	want := "data:image/png;filename=chart.png;base64," + base64.StdEncoding.EncodeToString([]byte("PNG"))
	if len(atts) != 1 || atts[0] != want || p["message"] != "Q3" {
		t.Fatalf("send: %v", p)
	}
	if err := a.SendFile(context.Background(), channel.Address{Conversation: "+1404"}, "a.txt", "text/plain", []byte("a"), ""); !errors.As(err, new(channel.Undeliverable)) {
		t.Fatalf("unregistered: %v", err)
	}
}

func TestStyled(t *testing.T) {
	plain, styles := styled("# Title\n- **bold** and _it_ and snake_case_name\n```go\ncode\n```\n[x](http://u) a * b * c")
	if plain != "Title\n• bold and it and snake_case_name\ncode\nx (http://u) a * b * c" {
		t.Fatalf("plain = %q", plain)
	}
	want := []string{"0:5:BOLD", "8:4:BOLD", "17:2:ITALIC", "40:4:MONOSPACE"}
	if !slices.Equal(styles, want) {
		t.Fatalf("styles = %v, want %v", styles, want)
	}
	// Offsets count UTF-16 units: the emoji takes two.
	if _, styles := styled("😀 **b**"); !slices.Equal(styles, []string{"3:1:BOLD"}) {
		t.Fatalf("emoji offsets: %v", styles)
	}
}

func TestCheck(t *testing.T) {
	srv := httptest.NewServer(&fakeDaemon{})
	defer srv.Close()
	if err := Check(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if err := Check(context.Background(), "http://127.0.0.1:1"); err == nil {
		t.Fatal("no error for a daemon that is not running")
	}
}
