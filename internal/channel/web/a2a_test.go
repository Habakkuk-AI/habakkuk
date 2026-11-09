package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestA2AMounted(t *testing.T) {
	s := &Server{Log: quiet, Token: "tok", A2A: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "a2a "+r.Method+" "+r.URL.Path)
	})}
	h := s.Handler()
	for _, c := range []struct{ method, path, want string }{
		{http.MethodGet, "/.well-known/agent-card.json", "a2a GET /.well-known/agent-card.json"},
		{http.MethodPost, "/a2a", "a2a POST /a2a"},
		{http.MethodPost, "/a2a/ops", "a2a POST /a2a/ops"},
		{http.MethodGet, "/a2a/ops/.well-known/agent-card.json", "a2a GET /a2a/ops/.well-known/agent-card.json"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(c.method, "http://habakkuk.example.com"+c.path, nil))
		if w.Code != http.StatusOK || w.Body.String() != c.want {
			t.Errorf("%s %s: %d %q", c.method, c.path, w.Code, w.Body)
		}
	}
}
