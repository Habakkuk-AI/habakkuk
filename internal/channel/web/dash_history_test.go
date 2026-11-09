package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
)

func TestDashMemoryHistory(t *testing.T) {
	r := newDashRig(t)
	// Without a history the endpoints say it is not available.
	if code, _ := r.do(t, "GET", "/api/memory-history/habakkuk", token, "", ""); code != http.StatusNotImplemented {
		t.Fatalf("no history: %d", code)
	}
	h, err := history.Open(context.Background(), r.home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	r.s.Dash.Memory.UseHistory(h)

	// The same auth as the rest of the API.
	for _, p := range []string{"/api/memory-history/habakkuk", "/api/memory-history/habakkuk/abcd"} {
		if code, _ := r.do(t, "GET", p, "", "", ""); code != http.StatusUnauthorized {
			t.Errorf("%s without token: %d", p, code)
		}
		if code, _ := r.do(t, "GET", p, token, "", "", "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
			t.Errorf("%s cross-site: %d", p, code)
		}
	}
	if code, _ := r.do(t, "POST", "/api/memory-history/habakkuk/abcd/restore", token, "text/plain", `{}`); code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain restore: %d", code)
	}

	r.send(t, "POST", "/api/memory/habakkuk", `{"title":"Coffee","content":"Flat white"}`, http.StatusCreated)
	r.send(t, "PUT", "/api/memory/habakkuk/coffee", `{"title":"Coffee","content":"Planted nonsense"}`, http.StatusOK)
	out := r.get(t, "/api/memory-history/habakkuk")
	changes := list(out, "changes")
	if len(changes) != 2 {
		t.Fatalf("changes: %v", out)
	}
	latest := changes[0].(map[string]any)
	if latest["by"] != history.ByUser || latest["tool"] != "dashboard" || latest["op"] != "updated" || latest["name"] != "coffee" {
		t.Fatalf("latest: %v", latest)
	}
	id := latest["id"].(string)
	if got := list(r.get(t, "/api/memory-history/habakkuk?file=USER.md"), "changes"); len(got) != 0 {
		t.Fatalf("file filter: %v", got)
	}
	if code, _ := r.do(t, "GET", "/api/memory-history/habakkuk?since=soon", token, "", ""); code != http.StatusBadRequest {
		t.Fatalf("bad since: %d", code)
	}

	one := r.get(t, "/api/memory-history/habakkuk/"+id)
	if d, _ := one["diff"].(string); !strings.Contains(d, "-Flat white") || !strings.Contains(d, "+Planted nonsense") {
		t.Fatalf("diff: %v", one)
	}
	if code, _ := r.do(t, "GET", "/api/memory-history/ghost/"+id, token, "", ""); code != http.StatusNotFound {
		t.Fatalf("unknown agent: %d", code)
	}
	if code, _ := r.do(t, "GET", "/api/memory-history/habakkuk/ffff0000", token, "", ""); code != http.StatusNotFound {
		t.Fatalf("unknown change: %d", code)
	}

	r.send(t, "POST", "/api/memory-history/habakkuk/"+id+"/restore", `{"version":"sideways"}`, http.StatusBadRequest)
	res := r.send(t, "POST", "/api/memory-history/habakkuk/"+id+"/restore", `{"version":"before"}`, http.StatusOK)
	restored, _ := res["restored"].(map[string]any)
	if restored == nil || restored["restores"] != id || restored["by"] != history.ByUser {
		t.Fatalf("restore: %v", res)
	}
	if got := r.get(t, "/api/memory/habakkuk/coffee"); got["body"] != "Flat white" {
		t.Fatalf("after restore: %v", got)
	}
	// Restoring again changes nothing.
	if res := r.send(t, "POST", "/api/memory-history/habakkuk/"+id+"/restore", `{}`, http.StatusOK); res["unchanged"] != true {
		t.Fatalf("second restore: %v", res)
	}
}
