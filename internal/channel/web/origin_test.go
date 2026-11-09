package web

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A page on another origin must not be able to drive the agent through the
// local server, with or without a token configured: no chat turn, no
// dashboard change, no MCP or OpenAI-style call, no WebSocket.
func TestOtherOriginCannotDriveTheAgent(t *testing.T) {
	for _, tok := range []string{"", token} {
		r := newRig(t, tok)
		evil := "http://evil.example"
		chat := `{"conversation":"c1","text":"run rm -rf ~"}`

		// A script on evil.example posting JSON (the preflight is skipped
		// here, as a no-cors or DNS-pinned request would).
		if code, _ := r.do(t, "POST", "/api/chat", tok, "application/json", chat, "Origin", evil); code != http.StatusForbidden {
			t.Fatalf("token %q: cross-origin chat: %d", tok, code)
		}
		// A plain form post, which browsers send without a preflight.
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		_ = mw.WriteField("conversation", "c1")
		_ = mw.WriteField("text", "run it")
		mw.Close()
		if code, _ := r.do(t, "POST", "/api/chat", tok, mw.FormDataContentType(), body.String(),
			"Origin", evil, "X-Habakkuk-Client", "x"); code != http.StatusForbidden {
			t.Fatalf("token %q: cross-origin form post: %d", tok, code)
		}
		// "null" origins: sandboxed frames, file:// pages.
		if code, _ := r.do(t, "POST", "/api/chat", tok, "application/json", chat, "Origin", "null"); code != http.StatusForbidden {
			t.Fatalf("token %q: null origin: %d", tok, code)
		}
		// Another port on localhost is another origin.
		if code, _ := r.do(t, "POST", "/api/chat", tok, "application/json", chat, "Origin", "http://127.0.0.1:1"); code != http.StatusForbidden {
			t.Fatalf("token %q: other localhost port: %d", tok, code)
		}
		// Browsers without Origin on a GET still say Sec-Fetch-Site.
		if code, _ := r.do(t, "GET", "/api/agents", tok, "", "", "Sec-Fetch-Site", "cross-site", "Sec-Fetch-Mode", "cors"); code != http.StatusForbidden {
			t.Fatalf("token %q: cross-site fetch: %d", tok, code)
		}
		for _, path := range []string{"/v1/chat/completions", "/mcp", "/api/board", "/api/approvals/x"} {
			if code, _ := r.do(t, "POST", path, tok, "text/plain", "{}", "Origin", evil); code != http.StatusForbidden {
				t.Fatalf("token %q: cross-origin POST %s: %d", tok, path, code)
			}
		}
		time.Sleep(50 * time.Millisecond)
		if n := r.model.count(); n != 0 {
			t.Fatalf("token %q: a foreign page started %d model calls", tok, n)
		}

		// The same page, from its own origin, works.
		if code, out := r.do(t, "POST", "/api/chat", tok, "application/json", chat, "Origin", r.ts.URL); code != http.StatusAccepted {
			t.Fatalf("token %q: same-origin chat: %d %v", tok, code, out)
		}
		// A link from another site to the chat page still opens it.
		if code, _ := r.do(t, "GET", "/", "", "", "", "Sec-Fetch-Site", "cross-site", "Sec-Fetch-Mode", "navigate"); code != http.StatusOK {
			t.Fatalf("token %q: navigation from another site: %d", tok, code)
		}
	}
}

func TestPreflightIsNeverGranted(t *testing.T) {
	r := newRig(t, "")
	req, _ := http.NewRequest(http.MethodOptions, r.ts.URL+"/api/chat", nil)
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden || res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("preflight: %d, allow-origin %q", res.StatusCode, res.Header.Get("Access-Control-Allow-Origin"))
	}
	if res.Header.Get("Cross-Origin-Resource-Policy") != "same-origin" {
		t.Fatal("no Cross-Origin-Resource-Policy")
	}
}

func TestPublicURLIsSameOrigin(t *testing.T) {
	r := newRig(t, token)
	r.s.PublicURL = "https://habakkuk.example.com"
	// Behind a proxy the Host the server sees is its own address.
	if code, out := r.do(t, "GET", "/api/agents", token, "", "", "Origin", "https://habakkuk.example.com"); code != http.StatusOK {
		t.Fatalf("public origin: %d %v", code, out)
	}
	if code, _ := r.do(t, "GET", "/api/agents", token, "", "", "Origin", "http://habakkuk.example.com"); code != http.StatusForbidden {
		t.Fatal("public host over another scheme accepted")
	}
}

// Device endpoints (pairing, the node WebSocket) refuse browser pages.
func TestNodeEndpointsRefuseOtherOrigins(t *testing.T) {
	public := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "public") })
	s := &Server{Log: quiet, Token: "tok", Nodes: &NodeEndpoints{Public: public}}
	h := s.Handler()
	for _, c := range []struct {
		method, path string
		hdr          map[string]string
	}{
		{"POST", "/nodes/pair", map[string]string{"Origin": "http://evil.example", "Content-Type": "text/plain"}},
		{"GET", "/nodes", map[string]string{"Origin": "http://evil.example", "Connection": "Upgrade", "Upgrade": "websocket",
			"Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ=="}},
	} {
		req := httptest.NewRequest(c.method, "http://127.0.0.1:8484"+c.path, strings.NewReader("{}"))
		for k, v := range c.hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s from another origin: %d %q", c.method, c.path, w.Code, w.Body)
		}
	}
	// A device (no Origin) gets through.
	req := httptest.NewRequest("POST", "http://127.0.0.1:8484/nodes/pair", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("device pairing: %d", w.Code)
	}
}
