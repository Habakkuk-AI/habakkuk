package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

// methods records every Bot API call with its JSON body.
type methods struct {
	mu      sync.Mutex
	calls   []string
	bodies  map[string]map[string]any
	updates string
}

func (m *methods) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, method)
	m.bodies[method] = body
	switch method {
	case "getMe":
		w.Write([]byte(`{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"Habakkuk","username":"habakkuk_bot"}}`))
	case "getUpdates":
		ups := m.updates
		m.updates = "[]"
		w.Write([]byte(`{"ok":true,"result":` + ups + `}`))
	default:
		w.Write([]byte(`{"ok":true,"result":true}`))
	}
}

func (m *methods) body(method string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bodies[method]
}

func TestButtonsAndPress(t *testing.T) {
	api := &methods{bodies: map[string]map[string]any{}, updates: `[
		{"update_id":1,"callback_query":{"id":"q1","from":{"id":7,"first_name":"Ay"},"data":"fy|abc|0",
			"message":{"message_id":55,"chat":{"id":7,"type":"private"},"text":"Approval needed"}}}]`}
	srv := httptest.NewServer(api)
	defer srv.Close()
	a := New("T", srv.URL)
	ctx := context.Background()
	rows := [][]channel.Button{{{Text: "Approve", Data: "fy|abc|0"}, {Text: "Deny", Data: "fy|abc|1"}}}
	if err := a.SendButtons(ctx, channel.Address{Conversation: "7", Thread: "3"}, "<b>Approve?</b>", rows); err != nil {
		t.Fatal(err)
	}
	b := api.body("sendMessage")
	kb := b["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	first := kb[0].([]any)[0].(map[string]any)
	if len(kb) != 1 || first["text"] != "Approve" || first["callback_data"] != "fy|abc|0" || b["message_thread_id"] != float64(3) {
		t.Fatalf("sendMessage = %v", b)
	}

	in := &inbox{}
	rctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	a.Run(rctx, in)
	in.mu.Lock()
	defer in.mu.Unlock()
	if len(in.msgs) != 1 {
		t.Fatalf("msgs = %+v", in.msgs)
	}
	if m := in.msgs[0]; m.Callback != "fy|abc|0" || m.Sender != "7" || m.Conversation != "7" || !m.Direct || m.Text != "" {
		t.Fatalf("press = %+v", m)
	}
	if b := api.body("answerCallbackQuery"); b["callback_query_id"] != "q1" {
		t.Fatalf("answer = %v", b)
	}
	if b := api.body("editMessageReplyMarkup"); b["message_id"] != float64(55) {
		t.Fatalf("buttons not removed: %v", b)
	}
	if b := api.body("getUpdates"); !strings.Contains(strings.Join(toStrings(b["allowed_updates"]), ","), "callback_query") {
		t.Fatalf("allowed_updates = %v", b["allowed_updates"])
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestReactionAndPoll(t *testing.T) {
	api := &methods{bodies: map[string]map[string]any{}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	a := New("T", srv.URL)
	ctx := context.Background()
	to := channel.Address{Conversation: "-5"}
	if err := a.React(ctx, to, "12", "👀"); err != nil {
		t.Fatal(err)
	}
	b := api.body("setMessageReaction")
	r := b["reaction"].([]any)[0].(map[string]any)
	if b["message_id"] != float64(12) || r["emoji"] != "👀" || r["type"] != "emoji" {
		t.Fatalf("reaction = %v", b)
	}
	if err := a.React(ctx, to, "12", ""); err != nil {
		t.Fatal(err)
	}
	if b := api.body("setMessageReaction"); len(b["reaction"].([]any)) != 0 {
		t.Fatalf("clear = %v", b)
	}
	if err := a.SendPoll(ctx, to, "Lunch?", []string{"Pizza", "Sushi"}, true); err != nil {
		t.Fatal(err)
	}
	p := api.body("sendPoll")
	opts := p["options"].([]any)
	if p["question"] != "Lunch?" || len(opts) != 2 || opts[1].(map[string]any)["text"] != "Sushi" ||
		p["allows_multiple_answers"] != true || p["is_anonymous"] != false {
		t.Fatalf("poll = %v", p)
	}
}
