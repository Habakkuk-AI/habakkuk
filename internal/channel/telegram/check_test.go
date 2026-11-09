package telegram

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCheckPeeksWithoutConfirming(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		switch r.URL.Path {
		case "/botT/getMe":
			io.WriteString(w, `{"ok":true,"result":{"id":9,"is_bot":true,"username":"nova_bot"}}`)
		case "/botT/getUpdates":
			io.WriteString(w, `{"ok":true,"result":[
				{"update_id":1,"message":{"from":{"id":5,"first_name":"Ada","username":"ada"},"chat":{"type":"private"}}},
				{"update_id":2,"message":{"from":{"id":6,"first_name":"Grp"},"chat":{"type":"group"}}},
				{"update_id":3,"message":{"from":{"id":5,"first_name":"Ada","username":"ada"},"chat":{"type":"private"}}}]}`)
		default:
			io.WriteString(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
		}
	}))
	defer srv.Close()
	name, senders, err := New("T", srv.URL).Check(context.Background())
	if err != nil || name != "nova_bot" || len(senders) != 1 || senders[0].ID != "5" || senders[0].Name != "Ada (@ada)" {
		t.Fatalf("%q %+v %v", name, senders, err)
	}
	mu.Lock()
	peek := bodies[1]
	mu.Unlock()
	if strings.Contains(peek, "offset") {
		t.Fatalf("the peek confirmed updates: %s", peek)
	}
	if _, _, err := New("bad", srv.URL).Check(context.Background()); err == nil {
		t.Fatal("bad token passed")
	}
}
