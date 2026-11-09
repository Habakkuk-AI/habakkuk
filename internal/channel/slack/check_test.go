package slack

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/auth.test" && tok == "Bearer xoxb-good":
			io.WriteString(w, `{"ok":true,"team":"Acme","team_id":"T1","user_id":"U1"}`)
		case r.URL.Path == "/apps.connections.open" && tok == "Bearer xapp-good":
			io.WriteString(w, `{"ok":true,"url":"wss://example.invalid/link"}`)
		default:
			io.WriteString(w, `{"ok":false,"error":"invalid_auth"}`)
		}
	}))
	defer srv.Close()
	teams, err := New("xapp-good", []string{"xoxb-good"}, srv.URL).Check(context.Background())
	if err != nil || len(teams) != 1 || teams[0] != "Acme" {
		t.Fatalf("%v %v", teams, err)
	}
	for _, a := range []*Adapter{New("xapp-bad", []string{"xoxb-good"}, srv.URL), New("xapp-good", []string{"xoxb-bad"}, srv.URL), New("", nil, srv.URL)} {
		if _, err := a.Check(context.Background()); err == nil {
			t.Fatal("bad tokens passed")
		}
	}
}
