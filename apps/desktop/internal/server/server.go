// Package server is the desktop app's loopback web server. The app's
// windows load everything from it:
//
//   - /desktop/ is the app's own pages (connections, logs, quick ask) and
//     /desktop/api/ their JSON API;
//   - every other path is passed to the connected Habakkuk with its token
//     added, so its dashboard at /app/ runs unchanged, in one origin, and the
//     token never reaches the page.
//
// Only the app's windows can use it: it answers only requests addressed to
// its own loopback address (no DNS rebinding), and only with a session
// cookie the app's windows get by opening a one-time-per-launch URL. The
// cookie is SameSite=Strict, so other pages in other browsers cannot ride on
// it; changes must be JSON, which browsers cannot send cross-site without a
// preflight this server never approves.
package server

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
)

//go:embed ui
var ui embed.FS

const cookieName = "habakkuk_desktop"

// Server is the loopback server. Create it with New.
type Server struct {
	// API serves /desktop/api/ (with that prefix stripped).
	API http.Handler
	Log *slog.Logger

	key    string
	addr   atomic.Value // string, host:port once listening
	target atomic.Pointer[target]
	proxy  *httputil.ReverseProxy
}

type target struct {
	url   *url.URL
	token string
}

// New returns a server with a fresh session key.
func New(api http.Handler, log *slog.Logger) *Server {
	b := make([]byte, 32)
	rand.Read(b)
	s := &Server{API: api, Log: log, key: hex.EncodeToString(b)}
	s.addr.Store("")
	s.proxy = &httputil.ReverseProxy{
		Rewrite:       s.rewrite,
		FlushInterval: -1, // event streams pass through as they come
		ErrorHandler:  s.proxyError,
	}
	return s
}

// Listen opens 127.0.0.1:port, or any free port when port is 0 or taken.
func (s *Server) Listen(port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil && port != 0 {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return nil, err
	}
	s.addr.Store(ln.Addr().String())
	return ln, nil
}

// Addr is the listening address, host:port.
func (s *Server) Addr() string { return s.addr.Load().(string) }

// Port is the listening port.
func (s *Server) Port() int {
	_, p, _ := net.SplitHostPort(s.Addr())
	n, _ := strconv.Atoi(p)
	return n
}

// EntryURL is the URL a window opens to get the session cookie and land on
// next (a path on this server, e.g. /app/ or /desktop/).
func (s *Server) EntryURL(next string) string {
	return "http://" + s.Addr() + "/desktop/enter?key=" + s.key + "&next=" + url.QueryEscape(next)
}

// URL is a plain URL on this server, for a window that already has the
// cookie.
func (s *Server) URL(p string) string { return "http://" + s.Addr() + p }

// SetTarget points the proxy at a Habakkuk; an empty base disconnects it.
func (s *Server) SetTarget(base, token string) error {
	if base == "" {
		s.target.Store(nil)
		return nil
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("bad Habakkuk URL %q", base)
	}
	s.target.Store(&target{url: u, token: token})
	return nil
}

// Target returns the base URL the proxy points at, "" when none.
func (s *Server) Target() string {
	if t := s.target.Load(); t != nil {
		return t.url.String()
	}
	return ""
}

// Handler routes every request.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /desktop/enter", s.enter)
	mux.Handle("/desktop/api/", s.session(apiGuard(http.StripPrefix("/desktop/api", s.API))))
	mux.Handle("/desktop/{file...}", s.session(http.HandlerFunc(s.uiFile)))
	mux.Handle("/", s.session(http.HandlerFunc(s.forward)))
	return s.hostGuard(mux)
}

// hostGuard answers only requests addressed to this server's own address.
func (s *Server) hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr := s.Addr()
		_, port, _ := net.SplitHostPort(addr)
		if r.Host != addr && r.Host != "localhost:"+port {
			http.Error(w, "wrong host", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) enter(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !s.validKey(q.Get("key")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	next := q.Get("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		next = "/desktop/"
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.key, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) validKey(k string) bool {
	return len(k) == len(s.key) && subtle.ConstantTimeCompare([]byte(k), []byte(s.key)) == 1
}

// session refuses requests without the app's cookie.
func (s *Server) session(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil || !s.validKey(c.Value) {
			http.Error(w, "this server only answers Habakkuk Desktop's own windows", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// apiGuard refuses what a browser marks as cross-site and changes whose body
// is not JSON.
func apiGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
				http.Error(w, "send application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

var uiTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".svg":  "image/svg+xml",
}

const uiCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func (s *Server) uiFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := r.PathValue("file")
	if name == "" {
		name = "index.html"
	}
	if strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	data, err := ui.ReadFile("ui/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", uiTypes[path.Ext(name)])
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Security-Policy", uiCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	w.Write(data)
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request) {
	if s.target.Load() == nil {
		if r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/app") {
			http.Redirect(w, r, "/desktop/", http.StatusSeeOther)
			return
		}
		writeError(w, http.StatusServiceUnavailable, "no Habakkuk is connected")
		return
	}
	if r.URL.Path == "/" {
		http.Redirect(w, r, "/app/", http.StatusSeeOther)
		return
	}
	s.proxy.ServeHTTP(w, r)
}

func (s *Server) rewrite(pr *httputil.ProxyRequest) {
	t := s.target.Load()
	if t == nil { // switched off between forward and here
		pr.Out.URL = &url.URL{Scheme: "http", Host: "127.0.0.1:1"}
		return
	}
	pr.SetURL(t.url)
	h := pr.Out.Header
	h.Del("Cookie") // the app's session is not Habakkuk's business
	h.Del("Origin")
	h.Del("Referer")
	if t.token != "" {
		h.Set("Authorization", "Bearer "+t.token)
	}
	// Without a stored token the page's own (from its token prompt) passes.
}

func (s *Server) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return // the window went away
	}
	if s.Log != nil {
		s.Log.Warn("proxy", "path", r.URL.Path, "err", err)
	}
	msg := "cannot reach Habakkuk at " + s.Target()
	if strings.HasPrefix(r.URL.Path, "/app") && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", uiCSP)
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Habakkuk</title><link rel="stylesheet" href="/desktop/desktop.css">`+
			`<main class="page"><h1>Habakkuk is not answering</h1><p>%s.</p><p><a href="/desktop/">Connections and logs</a></p></main>`, html.EscapeString(msg))
		return
	}
	writeError(w, http.StatusBadGateway, msg)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	data, _ := json.Marshal(map[string]string{"error": msg})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(data)
}
