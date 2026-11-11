package slack

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

type recordingSlack struct {
	mu    sync.Mutex
	calls []string // "method name-or-ts"
	last  map[string]map[string]any
}

func (s *recordingSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	if method == "auth.test" {
		w.Write([]byte(`{"ok":true,"team_id":"TA","team":"Alpha","user_id":"UBOT"}`))
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := method
	if n, ok := body["name"].(string); ok {
		entry += " " + n
	}
	s.calls = append(s.calls, entry)
	s.last[method] = body
	if method == "reactions.remove" && body["name"] == "missing" {
		w.Write([]byte(`{"ok":false,"error":"no_reaction"}`))
		return
	}
	w.Write([]byte(`{"ok":true,"ts":"1.2"}`))
}

func newRecordingSlack(t *testing.T) (*Adapter, *recordingSlack) {
	rec := &recordingSlack{last: map[string]map[string]any{}}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	a := New("xapp", []string{"xoxb-a"}, srv.URL+"/api")
	if err := a.loadTeams(context.Background()); err != nil {
		t.Fatal(err)
	}
	return a, rec
}

func TestSlackButtonsAndPress(t *testing.T) {
	a, rec := newRecordingSlack(t)
	ctx := context.Background()
	to := channel.Address{Conversation: "TA/C1", Thread: "9.9"}
	rows := [][]channel.Button{{{Text: "Approve", Data: "fy|n|0", Style: channel.ButtonPrimary}, {Text: "Deny", Data: "fy|n|1", Style: channel.ButtonDanger}}}
	if err := a.SendButtons(ctx, to, "*Approve?*", rows); err != nil {
		t.Fatal(err)
	}
	post := rec.last["chat.postMessage"]
	blocks := post["blocks"].([]any)
	actions := blocks[1].(map[string]any)["elements"].([]any)
	first, second := actions[0].(map[string]any), actions[1].(map[string]any)
	if post["thread_ts"] != "9.9" || first["value"] != "fy|n|0" || first["style"] != "primary" || second["style"] != "danger" ||
		first["action_id"] == second["action_id"] {
		t.Fatalf("post = %v", post)
	}
	payload := json.RawMessage(`{"type":"block_actions","team":{"id":"TA"},"user":{"id":"U7","name":"sam"},
		"channel":{"id":"C1"},"message":{"ts":"5.5","thread_ts":"9.9","text":"Approve?"},
		"actions":[{"value":"fy|n|1","action_ts":"6.6"}]}`)
	m, ok := a.press(ctx, payload)
	if !ok || m.Callback != "fy|n|1" || m.Conversation != "TA/C1" || m.Thread != "9.9" || m.Sender != "U7" || m.Direct {
		t.Fatalf("press = %+v %v", m, ok)
	}
	if up := rec.last["chat.update"]; up["ts"] != "5.5" || len(up["blocks"].([]any)) != 1 {
		t.Fatalf("buttons not removed: %v", up)
	}
	if _, ok := a.press(ctx, json.RawMessage(`{"type":"view_submission"}`)); ok {
		t.Fatal("a non-button interaction became a message")
	}
}

func TestSlackReactionsReplace(t *testing.T) {
	a, rec := newRecordingSlack(t)
	ctx := context.Background()
	to := channel.Address{Conversation: "TA/C1"}
	for _, e := range []string{"👀", "👍", ""} {
		if err := a.React(ctx, to, "5.5", e); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"reactions.add eyes", "reactions.remove eyes", "reactions.add +1", "reactions.remove +1"}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if strings.Join(rec.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v", rec.calls)
	}
	if err := a.React(ctx, to, "5.5", "🦄"); err == nil {
		t.Fatal("unknown emoji accepted")
	}
}
