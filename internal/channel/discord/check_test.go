package discord

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/@me" || r.Header.Get("Authorization") != "Bot good" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"message":"401: Unauthorized","code":0}`)
			return
		}
		io.WriteString(w, `{"id":"1","username":"nova"}`)
	}))
	defer srv.Close()
	if name, err := New("good", srv.URL).Check(context.Background()); err != nil || name != "nova" {
		t.Fatalf("%q %v", name, err)
	}
	if _, err := New("bad", srv.URL).Check(context.Background()); err == nil {
		t.Fatal("bad token passed")
	}
}
