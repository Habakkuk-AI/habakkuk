package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMountedEndpointsSkipTheAPIGuard(t *testing.T) {
	s := &Server{Log: quiet}
	s.Mount("sms", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "sms "+r.URL.Path)
	}))
	h := s.Handler()
	for path, want := range map[string]string{
		"/channels/sms":             "sms /",
		"/channels/sms/":            "sms /",
		"/channels/sms/media/abc":   "sms /media/abc",
		"/channels/other":           "",
		"/channels/sms-not-a-child": "",
	} {
		r := httptest.NewRequest(http.MethodPost, "http://habakkuk.example.com"+path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if want == "" {
			if w.Code == http.StatusOK {
				t.Errorf("%s answered %q", path, w.Body)
			}
			continue
		}
		if w.Code != http.StatusOK || w.Body.String() != want || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: %d %q", path, w.Code, w.Body)
		}
	}
	// Without a token the API still answers only loopback names.
	r := httptest.NewRequest(http.MethodGet, "http://habakkuk.example.com/api/agents", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("api through a public name: %d", w.Code)
	}
}
