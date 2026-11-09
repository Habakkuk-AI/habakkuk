package judge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// Fake serves the decision API: reply builds the answers for a request.
func Fake(t *testing.T, reply func(state any, qs map[string]Question) map[string]Answer) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/systemone" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, `{"detail":"bad"}`, http.StatusUnauthorized)
			return
		}
		var req struct {
			Model     string              `json:"model"`
			State     any                 `json:"state"`
			Questions map[string]Question `json:"questions"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil || req.Model == "" {
			http.Error(w, `{"detail":"bad body"}`, http.StatusUnprocessableEntity)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.14.0", "answers": reply(req.State, req.Questions),
			"usage": map[string]int{"input_tokens": 100, "output_tokens": 5}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAsk(t *testing.T) {
	srv := Fake(t, func(state any, qs map[string]Question) map[string]Answer {
		out := map[string]Answer{}
		for name, q := range qs {
			if q.Type != "noul" || q.Instructions == "" {
				t.Errorf("question %s: %+v", name, q)
			}
			out[name] = Answer{Type: "noul", P: 0.9}
		}
		return out
	})
	var used llm.Usage
	var target llm.Target
	c := &Client{BaseURL: srv.URL, Key: func() string { return "k" },
		OnUsage: func(_ context.Context, t llm.Target, u llm.Usage) { target, used = t, u }}
	ans, err := c.Ask(context.Background(), "hi", map[string]Question{"a": Yes("is it?")})
	if err != nil || ans["a"].P != 0.9 {
		t.Fatalf("%v %+v", err, ans)
	}
	if used.InputTokens != 100 || target.String() != "typesafe/jev-1.14.0" {
		t.Fatalf("usage %+v on %s", used, target)
	}

	if _, err := (&Client{BaseURL: srv.URL, Key: func() string { return "" }}).Ask(context.Background(), "x", map[string]Question{"a": Yes("?")}); !errors.Is(err, ErrNoKey) {
		t.Fatalf("no key: %v", err)
	}
	if _, err := (&Client{BaseURL: srv.URL, Key: func() string { return "wrong" }}).Ask(context.Background(), "x", map[string]Question{"a": Yes("?")}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("bad key: %v", err)
	}
	missing := Fake(t, func(any, map[string]Question) map[string]Answer { return map[string]Answer{} })
	if _, err := (&Client{BaseURL: missing.URL, Key: func() string { return "k" }}).Ask(context.Background(), "x", map[string]Question{"a": Yes("?")}); err == nil {
		t.Fatal("a missing answer must be an error")
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer slow.Close()
	start := time.Now()
	if _, err := (&Client{BaseURL: slow.URL, Key: func() string { return "k" }, Timeout: 50 * time.Millisecond}).Ask(context.Background(), "x", map[string]Question{"a": Yes("?")}); err == nil || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("timeout not honoured: %v after %v", err, time.Since(start))
	}
}
