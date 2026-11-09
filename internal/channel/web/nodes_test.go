package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNodeEndpointsAreMounted(t *testing.T) {
	echo := func(tag string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tag+" "+r.URL.Path) })
	}
	s := &Server{Log: quiet, Token: "tok", Nodes: &NodeEndpoints{Public: echo("public"), Admin: echo("admin")}}
	h := s.Handler()
	do := func(method, path, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://habakkuk.example.com"+path, strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// Devices carry their own credentials: no API token needed.
	for _, path := range []string{"/nodes", "/nodes/pair", "/nodes/events"} {
		if w := do(http.MethodGet, path, ""); w.Code != http.StatusOK || w.Body.String() != "public "+path {
			t.Errorf("%s: %d %q", path, w.Code, w.Body)
		}
	}
	// The dashboard's API needs the token.
	if w := do(http.MethodGet, "/api/nodes", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("/api/nodes without a token: %d", w.Code)
	}
	if w := do(http.MethodGet, "/api/nodes", "tok"); w.Code != http.StatusOK || w.Body.String() != "admin /api/nodes" {
		t.Errorf("/api/nodes: %d %q", w.Code, w.Body)
	}
	if w := do(http.MethodPost, "/api/nodes/pair", "tok"); w.Code != http.StatusOK {
		t.Errorf("/api/nodes/pair: %d %q", w.Code, w.Body)
	}
	if w := do(http.MethodDelete, "/api/nodes/laptop", "tok"); w.Code != http.StatusOK || w.Body.String() != "admin /api/nodes/laptop" {
		t.Errorf("DELETE /api/nodes/laptop: %d %q", w.Code, w.Body)
	}

	// Without nodes, /nodes is not served.
	s2 := &Server{Log: quiet, Token: "tok"}
	r := httptest.NewRequest(http.MethodGet, "http://habakkuk.example.com/nodes", nil)
	w := httptest.NewRecorder()
	s2.Handler().ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Fatalf("/nodes answered with nodes off: %q", w.Body)
	}
}

func TestPairPageIsServed(t *testing.T) {
	s := &Server{Log: quiet}
	for _, name := range []string{"pair.html", "nodes.js"} {
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/app/"+name, nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK || w.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
}
