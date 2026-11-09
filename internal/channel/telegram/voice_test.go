package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

func TestSendVoice(t *testing.T) {
	api := &fakeAPI{token: "T"}
	// Chat 8 refuses voice notes (a privacy setting).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendVoice") {
			r.ParseMultipartForm(1 << 20)
			if r.FormValue("chat_id") == "8" {
				w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: VOICE_MESSAGES_FORBIDDEN"}`))
				return
			}
		}
		api.ServeHTTP(w, r)
	}))
	defer srv.Close()
	a := New("T", srv.URL)
	ctx := context.Background()
	if err := a.SendVoice(ctx, channel.Address{Conversation: "7", Thread: "3"}, []byte("OggS..."), 12, "<b>hi</b>"); err != nil {
		t.Fatal(err)
	}
	if err := a.SendVoice(ctx, channel.Address{Conversation: "8"}, []byte("OggS..."), 0, ""); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.files) != 2 {
		t.Fatalf("uploads: %v", api.files)
	}
	v := api.files[0]
	if v["method"] != "sendVoice" || v["field"] != "voice" || v["name"] != "voice.ogg" || v["duration"] != "12" ||
		v["message_thread_id"] != "3" || v["caption"] != "<b>hi</b>" || v["parse_mode"] != "HTML" || v["data"] != "OggS..." {
		t.Fatalf("voice: %v", v)
	}
	if f := api.files[1]; f["method"] != "sendDocument" || f["name"] != "voice.ogg" || f["chat_id"] != "8" {
		t.Fatalf("fallback: %v", f)
	}
}
