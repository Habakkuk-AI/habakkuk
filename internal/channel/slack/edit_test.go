package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

func TestEditorPostsEditsAndDeletes(t *testing.T) {
	var mu sync.Mutex
	got := map[string]map[string]any{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth.test", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true,"team_id":"TA","team":"Alpha","user_id":"UBOT"}`))
	})
	record := func(name, answer string) {
		mux.HandleFunc("/api/"+name, func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			got[name] = body
			mu.Unlock()
			if name == "chat.delete" && body["ts"] == "gone" {
				w.Write([]byte(`{"ok":false,"error":"message_not_found"}`))
				return
			}
			w.Write([]byte(answer))
		})
	}
	record("chat.postMessage", `{"ok":true,"channel":"C1","ts":"1700.0001"}`)
	record("chat.update", `{"ok":true}`)
	record("chat.delete", `{"ok":true}`)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := New("xapp", []string{"xoxb-a"}, srv.URL+"/api")
	var ed channel.Editor = a
	ctx := context.Background()
	to := channel.Address{Account: "slack", Conversation: "TA/C1", Thread: "1699.0001"}
	id, err := ed.Post(ctx, to, "working")
	if err != nil || id != "1700.0001" {
		t.Fatalf("post = %q, %v", id, err)
	}
	if got["chat.postMessage"]["thread_ts"] != "1699.0001" {
		t.Fatalf("draft not in the thread: %v", got["chat.postMessage"])
	}
	if err := ed.Edit(ctx, to, id, "working more"); err != nil {
		t.Fatal(err)
	}
	if u := got["chat.update"]; u["channel"] != "C1" || u["ts"] != id || u["text"] != "working more" {
		t.Fatalf("update = %v", u)
	}
	if err := ed.Delete(ctx, to, id); err != nil {
		t.Fatal(err)
	}
	if err := ed.Delete(ctx, to, "gone"); err != nil {
		t.Fatalf("deleting a gone message should succeed: %v", err)
	}
}
