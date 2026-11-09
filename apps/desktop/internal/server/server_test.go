package server

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/fakehabakkuk"
)

// start runs a Server on a real loopback port and returns it with a client
// that has been through the entry URL (so it holds the cookie).
func start(t *testing.T) (*Server, *http.Client, *http.Client) {
	t.Helper()
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "api:"+r.Method+" "+r.URL.Path)
	})
	s := New(api, nil)
	ln, err := s.Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	hs := &httptest.Server{Listener: ln, Config: &http.Server{Handler: s.Handler()}}
	hs.Start()
	t.Cleanup(func() { hs.CloseClientConnections(); hs.Close() })

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	res, err := c.Get(s.EntryURL("/desktop/"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 || res.Request.URL.Path != "/desktop/" {
		t.Fatalf("entry: %d at %s", res.StatusCode, res.Request.URL)
	}
	return s, c, &http.Client{Timeout: 10 * time.Second}
}

func get(t *testing.T, c *http.Client, url string, hdr ...string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), res.Header
}

func TestSessionAndHost(t *testing.T) {
	s, c, anon := start(t)

	if code, _, _ := get(t, anon, s.URL("/desktop/")); code != http.StatusForbidden {
		t.Fatalf("no cookie: %d", code)
	}
	if code, _, _ := get(t, anon, s.URL("/app/")); code != http.StatusForbidden {
		t.Fatalf("no cookie on the proxy: %d", code)
	}
	if code, _, _ := get(t, anon, s.URL("/desktop/enter?key=wrong&next=/")); code != http.StatusForbidden {
		t.Fatalf("wrong key: %d", code)
	}
	// A rebinding attack arrives with another Host.
	req, _ := http.NewRequest(http.MethodGet, s.URL("/desktop/"), nil)
	req.Host = "evil.example:" + strings.Split(s.Addr(), ":")[1]
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("wrong host: %d", res.StatusCode)
	}

	code, body, h := get(t, c, s.URL("/desktop/"))
	if code != 200 || !strings.Contains(body, "<title>Habakkuk") || !strings.Contains(h.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("page: %d %q", code, h)
	}
	if code, _, _ := get(t, c, s.URL("/desktop/nope.js")); code != 404 {
		t.Fatalf("missing file: %d", code)
	}

	// An open redirect is not possible through next.
	jar, _ := cookiejar.New(nil)
	noFollow := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, next := range []string{"//evil.example/", "https://evil.example/", "/\\evil"} {
		res, err := noFollow.Get(strings.Replace(s.EntryURL("/x"), "next=%2Fx", "next="+next, 1))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if loc := res.Header.Get("Location"); loc != "/desktop/" {
			t.Errorf("next %q redirected to %q", next, loc)
		}
		if ck := res.Header.Get("Set-Cookie"); !strings.Contains(ck, "HttpOnly") || !strings.Contains(ck, "SameSite=Strict") {
			t.Errorf("cookie = %q", ck)
		}
	}
}

func TestAPIGuard(t *testing.T) {
	s, c, _ := start(t)
	if code, body, _ := get(t, c, s.URL("/desktop/api/state")); code != 200 || body != "api:GET /state" {
		t.Fatalf("api: %d %q", code, body)
	}
	post := func(ct string, hdr ...string) int {
		req, _ := http.NewRequest(http.MethodPost, s.URL("/desktop/api/x"), strings.NewReader("{}"))
		req.Header.Set("Content-Type", ct)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := post("application/json"); code != 200 {
		t.Fatalf("json post: %d", code)
	}
	if code := post("text/plain"); code != http.StatusUnsupportedMediaType {
		t.Fatalf("form post: %d", code)
	}
	if code := post("application/json", "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Fatalf("cross-site: %d", code)
	}
}

func TestProxy(t *testing.T) {
	s, c, _ := start(t)
	f := fakehabakkuk.New("tok")
	defer f.Close()

	// Nothing connected yet.
	jar := c.Jar
	noFollow := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := noFollow.Get(s.URL("/app/"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/desktop/" {
		t.Fatalf("no target /app/: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if code, body, _ := get(t, c, s.URL("/api/overview")); code != 503 || !strings.Contains(body, "no Habakkuk") {
		t.Fatalf("no target api: %d %q", code, body)
	}

	if err := s.SetTarget("ftp://x", ""); err == nil {
		t.Fatal("bad target accepted")
	}
	if err := s.SetTarget(f.URL, "tok"); err != nil {
		t.Fatal(err)
	}
	if s.Target() != f.URL {
		t.Fatalf("Target = %q", s.Target())
	}
	code, body, _ := get(t, c, s.URL("/app/"))
	if code != 200 || !strings.Contains(body, "dashboard") {
		t.Fatalf("dashboard: %d %q", code, body)
	}
	// The stored token replaces whatever the page sends; the app's cookie
	// does not reach Habakkuk.
	code, body, _ = get(t, c, s.URL("/api/app"), "Authorization", "Bearer stale")
	if code != 200 || !strings.Contains(body, `"default":"habakkuk"`) {
		t.Fatalf("api through proxy: %d %q", code, body)
	}
	if hdr := f.LastHeader(); hdr.Get("Cookie") != "" || hdr.Get("Authorization") != "Bearer tok" {
		t.Fatalf("forwarded headers = %v", hdr)
	}

	// Without a stored token, the page's own passes through.
	s.SetTarget(f.URL, "")
	if code, _, _ := get(t, c, s.URL("/api/app")); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code, _, _ := get(t, c, s.URL("/api/app"), "Authorization", "Bearer tok"); code != 200 {
		t.Fatalf("page token: %d", code)
	}
	s.SetTarget(f.URL, "tok")

	// Event streams arrive as they are written, not when they end.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.URL("/api/chat/c1/events?after=0"), nil)
	sres, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer sres.Body.Close()
	go func() {
		time.Sleep(100 * time.Millisecond)
		f.Push("c1", "assistant", "streamed", time.Now())
	}()
	sc := bufio.NewScanner(sres.Body)
	found := false
	for sc.Scan() {
		if strings.Contains(sc.Text(), `"text":"streamed"`) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("the streamed event never arrived through the proxy")
	}
	cancel()

	// Switching connections takes effect on the next request.
	g := fakehabakkuk.New("other")
	defer g.Close()
	s.SetTarget(g.URL, "other")
	if code, _, _ := get(t, c, s.URL("/api/overview")); code != 200 {
		t.Fatalf("after switch: %d", code)
	}

	// An unreachable Habakkuk is a 502 the dashboard can show.
	s.SetTarget("http://127.0.0.1:1", "")
	if code, body, _ := get(t, c, s.URL("/api/overview")); code != 502 || !strings.Contains(body, `"error"`) {
		t.Fatalf("unreachable api: %d %q", code, body)
	}
	if code, body, _ := get(t, c, s.URL("/app/")); code != 502 || !strings.Contains(body, "Connections and logs") {
		t.Fatalf("unreachable page: %d %q", code, body)
	}
	s.SetTarget("", "")
	if s.Target() != "" {
		t.Fatal("target not cleared")
	}
}

func TestPrefixedTarget(t *testing.T) {
	s, c, _ := start(t)
	// A Habakkuk behind a reverse proxy under a path.
	mux := http.NewServeMux()
	mux.HandleFunc("/habakkuk/api/app", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "prefixed") })
	up := httptest.NewServer(mux)
	defer up.Close()
	s.SetTarget(up.URL+"/habakkuk", "")
	if code, body, _ := get(t, c, s.URL("/api/app")); code != 200 || body != "prefixed" {
		t.Fatalf("prefixed: %d %q", code, body)
	}
}
