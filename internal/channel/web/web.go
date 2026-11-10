// Package web is Habakkuk's HTTP surface: a chat page, a JSON API whose
// replies stream as Server-Sent Events, and webhooks. The chat is a channel
// adapter like any other, so web messages go through the gateway and get
// its idempotent turns, durable deliveries, approvals and commands.
package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
)

//go:embed index.html
var page []byte

const (
	maxBody    = 1 << 20 // request bodies: chat messages and webhooks
	maxText    = 100_000 // characters in one chat message
	keepalive  = 20 * time.Second
	batch      = 200 // events read per query while catching up
	pruneEvery = 6 * time.Hour
)

var conversationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// Server serves the HTTP surface and is the adapter for the web channel
// account. Its HTTP server runs on its own, outside the channel supervisor,
// so health checks and webhooks keep answering while the adapter restarts.
type Server struct {
	Account string // channel account of type web
	Addr    string // host:port to listen on
	// PublicURL is how browsers reach this server through a proxy; pages
	// from its origin count as same-origin.
	PublicURL string
	Token     string // bearer token; empty is only allowed on loopback
	TokenEnv  string // the variable Token came from, for error messages
	Hooks     map[string]*Hook
	Gateway   *channel.Gateway
	Store     *Store
	Log       *slog.Logger
	Retention time.Duration // messages older than this are pruned; 0 keeps them
	MCP       http.Handler  // Habakkuk's MCP server at /mcp (Streamable HTTP); nil is off
	Dash      *Dash         // stores behind the dashboard at /app; nil leaves only its chat parts
	// Metrics serves GET /metrics (Prometheus text); nil is off. It needs
	// the token unless MetricsPublic is set.
	Metrics       http.Handler
	MetricsPublic bool
	// Nodes serves device nodes at /nodes and /api/nodes; nil is off.
	Nodes *NodeEndpoints

	// A2A serves the Agent2Agent protocol at /a2a and the Agent Card at
	// /.well-known/agent-card.json; it checks its own token. nil is off.
	A2A http.Handler

	mu        sync.Mutex
	inbox     channel.Inbox // set while the supervisor runs the adapter
	hub       hub
	endpoints map[string]http.Handler // channel account -> its webhook handler
	rt        realtimeState           // Live mode's transcript lines and running calls (realtime.go)
}

// Mount serves a channel adapter that receives over HTTP at
// /channels/<account>. The adapter checks each request's signature or token
// itself, so these paths skip the API token and the loopback-only rule.
func (s *Server) Mount(account string, h http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.endpoints == nil {
		s.endpoints = map[string]http.Handler{}
	}
	s.endpoints[account] = h
}

// Listen opens the listening socket, refusing an address other machines can
// reach unless a token guards the API.
func (s *Server) Listen() (net.Listener, error) {
	host, _, err := net.SplitHostPort(s.Addr)
	if err != nil {
		return nil, fmt.Errorf("http listen %q: %w", s.Addr, err)
	}
	if s.Token == "" && s.TokenEnv != "" {
		return nil, fmt.Errorf("http: token_env names %s, which is not set", s.TokenEnv)
	}
	if s.Token == "" && !config.IsLoopback(host) {
		return nil, fmt.Errorf("http: refusing to serve %s without a token; set http.token_env", s.Addr)
	}
	return net.Listen("tcp", s.Addr)
}

// Serve answers HTTP on ln until ctx ends, pruning old messages as it goes.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// Request contexts end with ctx, so open event streams close on
		// shutdown instead of holding it up.
		BaseContext: func(net.Listener) context.Context { return ctx },
		ErrorLog:    slog.NewLogLogger(s.Log.Handler(), slog.LevelWarn),
	}
	go s.prune(ctx)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return nil
	}
}

func (s *Server) prune(ctx context.Context) {
	if s.Retention <= 0 {
		return
	}
	t := time.NewTicker(pruneEvery)
	defer t.Stop()
	for {
		if n, err := s.Store.Prune(ctx, time.Now().Add(-s.Retention)); err != nil && ctx.Err() == nil {
			s.Log.Warn("prune web messages failed", "err", err)
		} else if n > 0 {
			s.Log.Info("pruned web messages", "n", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Handler routes the HTTP surface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /api/agents", s.auth(s.agents))
	mux.HandleFunc("POST /api/chat", s.auth(s.chat))
	mux.HandleFunc("GET /api/chat/{conversation}/events", s.auth(s.events))
	mux.HandleFunc("POST /hooks/{name}", s.hook)
	mux.HandleFunc("GET /v1/models", s.v1(s.listModels))
	mux.HandleFunc("GET /v1/models/{id...}", s.v1(s.getModel))
	mux.HandleFunc("POST /v1/chat/completions", s.v1(s.chatCompletions))
	mux.HandleFunc("POST /v1/responses", s.v1(s.responses))
	if s.MCP != nil {
		mux.Handle("/mcp", s.auth(s.MCP.ServeHTTP))
	}
	if s.A2A != nil {
		mux.Handle("GET /.well-known/agent-card.json", s.A2A)
		mux.Handle("/a2a", s.A2A)
		mux.Handle("/a2a/", s.A2A)
	}
	s.dashRoutes(mux)
	s.nodeAPIRoutes(mux)
	if s.Metrics != nil {
		if s.MetricsPublic {
			mux.Handle("GET /metrics", s.Metrics)
		} else {
			mux.Handle("GET /metrics", s.auth(s.Metrics.ServeHTTP))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.endpoints) == 0 && !s.hasNodes() {
		return s.guard(mux)
	}
	root := http.NewServeMux()
	root.Handle("/", s.guard(mux))
	s.nodePublicRoutes(root)
	for name, h := range s.endpoints {
		prefix := "/channels/" + name
		h = http.StripPrefix(prefix, endpointHeaders(h))
		root.Handle(prefix, h)
		root.Handle(prefix+"/", h)
	}
	return root
}

func endpointHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		next.ServeHTTP(w, r)
	})
}

// guard sets defensive headers and, when no token is configured, only
// answers requests addressed to a loopback name, so a web page cannot reach
// the tokenless API through DNS rebinding.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if why := s.foreign(r); why != "" {
			httpError(w, http.StatusForbidden, why)
			return
		}
		if s.Token == "" {
			host, _, err := net.SplitHostPort(r.Host)
			if err != nil {
				host = r.Host
			}
			if !config.IsLoopback(strings.Trim(host, "[]")) {
				httpError(w, http.StatusForbidden, "this server has no token, so it only answers on localhost")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// foreign says why a request comes from a page on another origin, or "".
// Browsers send Origin with every cross-origin request, every POST and every
// WebSocket handshake, and Sec-Fetch-Site with every request; a page on
// another site (or another port of localhost) can therefore never drive the
// agent, read the dashboard's data or open a socket here, token or not.
// CORS is never granted: there is no Access-Control-Allow-Origin anywhere.
// Programs (curl, SDKs, webhook senders, devices) send neither header and
// are judged by the token alone.
func (s *Server) foreign(r *http.Request) string {
	if o := r.Header.Get("Origin"); o != "" && !s.sameOrigin(r, o) {
		return "cross-origin request refused (Origin " + shortOrigin(o) + ")"
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site", "same-site":
		// Following a link to the chat page or dashboard is fine; anything
		// else a foreign page makes the browser send is not.
		nav := r.Header.Get("Sec-Fetch-Mode") == "navigate" && (r.Method == http.MethodGet || r.Method == http.MethodHead)
		if !nav || r.Header.Get("Upgrade") != "" {
			return "cross-site request refused"
		}
	}
	return ""
}

// sameOrigin reports whether origin is this server as the browser sees it:
// the Host it asked for, or the configured public URL.
func (s *Server) sameOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false // includes "null" from sandboxed frames and files
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	// A proxy on the way (tailscale serve, a TLS terminator) names the
	// host the browser asked for. Pages cannot set this header on a
	// cross-origin request without a preflight, which is never granted.
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" && strings.EqualFold(u.Host, strings.TrimSpace(strings.Split(fwd, ",")[0])) {
		return true
	}
	if s.PublicURL != "" {
		if p, err := url.Parse(s.PublicURL); err == nil && strings.EqualFold(p.Scheme, u.Scheme) && strings.EqualFold(p.Host, u.Host) {
			return true
		}
	}
	return false
}

func shortOrigin(o string) string {
	if len(o) > 100 {
		return o[:100] + "..."
	}
	return o
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" {
			tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || !sameSecret(tok, s.Token) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="habakkuk"`)
				httpError(w, http.StatusUnauthorized, "missing or wrong token")
				return
			}
		}
		next(w, r)
	}
}

// sameSecret compares in constant time. Hashing first hides the length too.
func sameSecret(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

func (s *Server) page(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; "+
		"connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Write(page)
}

type healthChannel struct {
	Account string    `json:"account"`
	State   string    `json:"state"`
	Since   time.Time `json:"since"`
}

type health struct {
	Status   string          `json:"status"` // ok | degraded (a channel is not connected) | error
	Channels []healthChannel `json:"channels"`
	Queue    struct {
		Pending int `json:"pending"`
		Running int `json:"running"`
	} `json:"queue"`
}

// health needs no token, so it reports states only, never error text.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	out := health{Status: "ok", Channels: []healthChannel{}}
	sts, err := s.Gateway.Store.Statuses(r.Context())
	if err == nil {
		out.Queue.Pending, out.Queue.Running, err = s.Gateway.Queue.Depth(r.Context())
	}
	if err != nil {
		s.Log.Error("health check failed", "err", err)
		out.Status = "error"
		writeJSON(w, http.StatusServiceUnavailable, out)
		return
	}
	for _, st := range sts {
		if _, ok := s.Gateway.Channels[st.Account]; !ok {
			continue // removed from config since
		}
		out.Channels = append(out.Channels, healthChannel{Account: st.Account, State: st.State, Since: st.Since.UTC()})
		if st.State != "connected" {
			out.Status = "degraded"
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) agents(w http.ResponseWriter, _ *http.Request) {
	names := make([]string, 0, len(s.Gateway.Runner.Agents))
	for name := range s.Gateway.Runner.Agents {
		names = append(names, name)
	}
	slices.Sort(names)
	def := ""
	if ch := s.Gateway.Channels[s.Account]; ch != nil {
		def = ch.Agent
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": names, "default": def})
}

type chatRequest struct {
	Conversation string `json:"conversation"`
	Text         string `json:"text"`
	Agent        string `json:"agent"`
	ID           string `json:"id"` // optional client message id; resending it is a no-op
	// Spoken: the text is a transcript of Live mode's speech, never set by
	// a client.
	Spoken bool `json:"-"`
}

// chat hands a message to the gateway and returns the turn it started, or
// no turn when the message was a command or an answer to an approval.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	// A JSON content type forces browsers to preflight cross-site requests,
	// which this server never approves.
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt == "multipart/form-data" {
		s.chatUpload(w, r) // files and voice from the dashboard
		return
	}
	if mt != "application/json" {
		httpError(w, http.StatusUnsupportedMediaType, "send application/json")
		return
	}
	var req chatRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	switch {
	case !conversationID.MatchString(req.Conversation):
		httpError(w, http.StatusBadRequest, "conversation must be 1-128 letters, digits, '.', '_', ':' or '-'")
		return
	case req.Text == "":
		httpError(w, http.StatusBadRequest, "text is empty")
		return
	case utf8.RuneCountInString(req.Text) > maxText:
		httpError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("text is longer than %d characters", maxText))
		return
	case len(req.ID) > 128:
		httpError(w, http.StatusBadRequest, "id is longer than 128 characters")
		return
	}
	if _, ok := s.Gateway.Runner.Agents[req.Agent]; req.Agent != "" && !ok {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("unknown agent %q", req.Agent))
		return
	}
	in := s.currentInbox()
	if in == nil {
		httpError(w, http.StatusServiceUnavailable, "the web channel is starting; try again shortly")
		return
	}
	// The work below must not stop halfway because the client went away.
	ctx := context.WithoutCancel(r.Context())
	to := channel.Address{Account: s.Account, Conversation: req.Conversation}
	if req.Agent == "" {
		// A conversation keeps the agent it was started with.
		if conv, err := s.Gateway.Store.Conversation(ctx, to); err == nil && conv != nil {
			req.Agent = conv.Agent
		}
	}
	if req.ID == "" {
		req.ID = jobs.NewID("web")
	}
	if _, err := s.Store.Append(ctx, req.Conversation, "user", req.ID, req.Text); err != nil {
		s.Log.Error("record web message failed", "err", err)
		httpError(w, http.StatusInternalServerError, "could not record the message")
		return
	}
	s.hub.notify(req.Conversation, false)
	m := channel.Message{
		Account: s.Account, Conversation: req.Conversation, ID: req.ID, Sender: "web",
		Text: req.Text, Direct: true, Agent: req.Agent,
	}
	in.Deliver(ctx, m)
	out := map[string]string{"message": req.ID, "turn": ""}
	if j, err := s.Gateway.Queue.Get(ctx, channel.TurnID(m)); err == nil && j != nil {
		out["turn"] = j.ID
	}
	writeJSON(w, http.StatusAccepted, out)
}

// events streams a conversation's messages after ?after=N (or the
// Last-Event-ID a reconnecting EventSource sends), then new ones as they
// are delivered, with "typing" events while a turn runs.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	conv := r.PathValue("conversation")
	if !conversationID.MatchString(conv) {
		httpError(w, http.StatusBadRequest, "bad conversation id")
		return
	}
	after, err := cursor(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, "after must be a message sequence number")
		return
	}
	rc := http.NewResponseController(w)
	sub := s.hub.subscribe(conv) // before the first read, so nothing slips between
	defer s.hub.unsubscribe(conv, sub)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // reverse proxies: do not buffer the stream
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")

	ctx := r.Context()
	tick := time.NewTicker(keepalive)
	defer tick.Stop()
	for {
		for {
			evs, err := s.Store.After(ctx, conv, after, batch)
			if err != nil {
				if ctx.Err() == nil {
					s.Log.Warn("read web messages failed", "err", err)
				}
				return
			}
			for _, e := range evs {
				data, _ := json.Marshal(e)
				fmt.Fprintf(w, "id: %d\nevent: message\ndata: %s\n\n", e.Seq, data)
				after = e.Seq
			}
			if len(evs) < batch {
				break
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-sub.wake:
		case <-sub.typing:
			fmt.Fprint(w, "event: typing\ndata: {}\n\n")
		case <-tick.C:
			fmt.Fprint(w, ": keepalive\n\n")
		}
	}
}

func cursor(r *http.Request) (int64, error) {
	v := r.URL.Query().Get("after")
	if v == "" {
		v = r.Header.Get("Last-Event-ID")
	}
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err == nil && n < 0 {
		err = errors.New("negative")
	}
	return n, err
}

// The channel adapter side.

// Run makes the server's chat endpoint live until ctx ends. There is no
// connection to keep; it beats so the supervisor sees it healthy.
func (s *Server) Run(ctx context.Context, in channel.Inbox) error {
	s.mu.Lock()
	s.inbox = in
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.inbox == in {
			s.inbox = nil
		}
		s.mu.Unlock()
	}()
	in.Connected("http " + s.Addr)
	t := time.NewTicker(keepalive)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			in.Beat()
		}
	}
}

func (s *Server) currentInbox() channel.Inbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inbox
}

// Send records a reply; open event streams pick it up at once and clients
// that are away read it when they reconnect.
func (s *Server) Send(ctx context.Context, to channel.Address, text, _ string) error {
	if !conversationID.MatchString(to.Conversation) {
		return channel.Undeliverable{Err: fmt.Errorf("web: bad conversation id %q", to.Conversation)}
	}
	if _, err := s.Store.Append(ctx, to.Conversation, "assistant", "", text); err != nil {
		return err
	}
	s.hub.notify(to.Conversation, false)
	return nil
}

// Typing tells open streams that a turn is running.
func (s *Server) Typing(_ context.Context, to channel.Address) error {
	s.hub.notify(to.Conversation, true)
	return nil
}

// Format keeps Markdown: the chat page renders it.
func (s *Server) Format(md string) string { return md }

// MaxLen is large: a web reply never needs splitting.
func (s *Server) MaxLen() int { return 200_000 }

func (s *Server) StaleAfter() time.Duration { return 3 * keepalive }

// hub wakes the event streams open on a conversation.
type hub struct {
	mu   sync.Mutex
	subs map[string]map[*sub]struct{}
}

type sub struct{ wake, typing chan struct{} }

func (h *hub) subscribe(conv string) *sub {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs == nil {
		h.subs = map[string]map[*sub]struct{}{}
	}
	if h.subs[conv] == nil {
		h.subs[conv] = map[*sub]struct{}{}
	}
	s := &sub{wake: make(chan struct{}, 1), typing: make(chan struct{}, 1)}
	h.subs[conv][s] = struct{}{}
	return s
}

func (h *hub) unsubscribe(conv string, s *sub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs[conv], s)
	if len(h.subs[conv]) == 0 {
		delete(h.subs, conv)
	}
}

func (h *hub) notify(conv string, typing bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs[conv] {
		ch := s.wake
		if typing {
			ch = s.typing
		}
		select {
		case ch <- struct{}{}:
		default: // already pending
		}
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body, ok := readBody(w, r)
	if !ok {
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		httpError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return false
	}
	return true
}

// readBody reads at most maxBody bytes, with a deadline so a slow client
// cannot hold a connection open.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			httpError(w, http.StatusRequestEntityTooLarge, "body is larger than 1 MB")
		} else {
			httpError(w, http.StatusBadRequest, "could not read the body")
		}
		return nil, false
	}
	return body, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
