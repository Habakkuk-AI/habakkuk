package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

type restLog struct {
	mu     sync.Mutex
	calls  []string // "METHOD path"
	bodies []map[string]any
}

func (l *restLog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	l.mu.Lock()
	l.calls = append(l.calls, r.Method+" "+strings.TrimPrefix(r.URL.EscapedPath(), "/api/v10"))
	l.bodies = append(l.bodies, body)
	l.mu.Unlock()
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages") {
		w.Write([]byte(`{"id":"500"}`))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func newRESTAdapter(t *testing.T) (*Adapter, *restLog) {
	log := &restLog{}
	srv := httptest.NewServer(log)
	t.Cleanup(srv.Close)
	return New("tok", srv.URL+"/api/v10"), log
}

func TestDiscordButtonsPressReactAndPoll(t *testing.T) {
	a, log := newRESTAdapter(t)
	ctx := context.Background()
	to := channel.Address{Conversation: "123"}
	rows := [][]channel.Button{{{Text: "Approve", Data: "fy|n|0", Style: channel.ButtonPrimary}, {Text: "Deny", Data: "fy|n|1", Style: channel.ButtonDanger}}}
	if err := a.SendButtons(ctx, to, "Approve?", rows); err != nil {
		t.Fatal(err)
	}
	comps := log.bodies[0]["components"].([]any)
	buttons := comps[0].(map[string]any)["components"].([]any)
	b0, b1 := buttons[0].(map[string]any), buttons[1].(map[string]any)
	if log.calls[0] != "POST /channels/123/messages" || b0["custom_id"] != "fy|n|0" || b0["style"] != float64(stylePrimary) || b1["style"] != float64(styleDanger) {
		t.Fatalf("post %v %v", log.calls, log.bodies[0])
	}

	raw := json.RawMessage(`{"id":"777","token":"tkn","type":3,"channel_id":"123","guild_id":"9",
		"data":{"custom_id":"fy|n|1"},"member":{"nick":"Ay","user":{"id":"42","username":"ay"}},"message":{"content":"Approve?"}}`)
	m, ok := a.press(ctx, raw)
	if !ok || m.Callback != "fy|n|1" || m.Sender != "42" || m.SenderName != "Ay" || m.Conversation != "123" || m.Direct {
		t.Fatalf("press = %+v %v", m, ok)
	}
	if last := log.calls[len(log.calls)-1]; last != "POST /interactions/777/tkn/callback" || log.bodies[len(log.bodies)-1]["type"] != float64(7) {
		t.Fatalf("interaction answer = %v %v", last, log.bodies[len(log.bodies)-1])
	}
	if _, ok := a.press(ctx, json.RawMessage(`{"type":2,"data":{"name":"slash"}}`)); ok {
		t.Fatal("a slash command became a press")
	}

	n := len(log.calls)
	for _, e := range []string{"👀", "👍"} {
		if err := a.React(ctx, to, "456", e); err != nil {
			t.Fatal(err)
		}
	}
	got := strings.Join(log.calls[n:], ",")
	want := "PUT /channels/123/messages/456/reactions/%F0%9F%91%80/@me,DELETE /channels/123/messages/456/reactions/%F0%9F%91%80/@me,PUT /channels/123/messages/456/reactions/%F0%9F%91%8D/@me"
	if got != want {
		t.Fatalf("reactions:\n%s\nwant\n%s", got, want)
	}

	if err := a.SendPoll(ctx, to, "Lunch?", []string{"Pizza", "Sushi"}, false); err != nil {
		t.Fatal(err)
	}
	poll := log.bodies[len(log.bodies)-1]["poll"].(map[string]any)
	answers := poll["answers"].([]any)
	if poll["question"].(map[string]any)["text"] != "Lunch?" || len(answers) != 2 || poll["allow_multiselect"] != false {
		t.Fatalf("poll = %v", poll)
	}
}
