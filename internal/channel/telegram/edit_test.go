package telegram

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

func TestEditorPostsEditsAndDeletes(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls = append(calls, method)
		mu.Unlock()
		switch {
		case method == "sendMessage":
			if body["disable_notification"] != true {
				t.Errorf("draft would notify: %v", body)
			}
			w.Write([]byte(`{"ok":true,"result":{"message_id":321}}`))
		case method == "editMessageText" && body["text"] == "same":
			w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`))
		case method == "editMessageText":
			if body["message_id"] != float64(321) {
				t.Errorf("edit of %v", body["message_id"])
			}
			w.Write([]byte(`{"ok":true,"result":{}}`))
		case method == "deleteMessage" && body["message_id"] == float64(404):
			w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: message to delete not found"}`))
		case method == "deleteMessage":
			w.Write([]byte(`{"ok":true,"result":true}`))
		default:
			w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: unexpected"}`))
		}
	}))
	defer srv.Close()
	a := New("T", srv.URL)
	var ed channel.Editor = a
	ctx := context.Background()
	to := channel.Address{Account: "tg", Conversation: "100", Thread: "7"}
	id, err := ed.Post(ctx, to, "working")
	if err != nil || id != "321" {
		t.Fatalf("post = %q, %v", id, err)
	}
	if err := ed.Edit(ctx, to, id, "working\n→ exec"); err != nil {
		t.Fatal(err)
	}
	if err := ed.Edit(ctx, to, id, "same"); err != nil {
		t.Fatalf("an unchanged edit should succeed: %v", err)
	}
	if err := ed.Delete(ctx, to, id); err != nil {
		t.Fatal(err)
	}
	if err := ed.Delete(ctx, to, "404"); err != nil {
		t.Fatalf("deleting a gone message should succeed: %v", err)
	}
	if got := strings.Join(calls, ","); got != "sendMessage,editMessageText,editMessageText,deleteMessage,deleteMessage" {
		t.Fatalf("calls = %s", got)
	}
}
