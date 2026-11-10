package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Hub is the server side of nodes: it accepts node connections, keeps the
// live ones, and sends them invocations.
type Hub struct {
	Store *Store
	Log   *slog.Logger
	// Heartbeat is how often each connection is pinged and its node checked
	// against the store, so a node revoked by another process (hk nodes
	// revoke) is cut off within one beat (default 20s).
	Heartbeat time.Duration
	// PairTTL is how long a code made from the dashboard works (default 10m).
	PairTTL time.Duration

	mu    sync.Mutex
	peers map[string]*peer
	fails []time.Time // recent failed pairings and logins, for throttling
	seq   atomic.Uint64
}

// peer is one live node connection.
type peer struct {
	name, platform, secret string
	caps                   []string
	since                  time.Time
	send                   chan Frame
	done                   chan struct{}
	closeOnce              sync.Once

	mu      sync.Mutex
	waiting map[string]chan *Result
}

func (p *peer) close() { p.closeOnce.Do(func() { close(p.done) }) }

func (p *peer) deliver(id string, r *Result) {
	p.mu.Lock()
	ch := p.waiting[id]
	delete(p.waiting, id)
	p.mu.Unlock()
	if ch != nil {
		ch <- r
	}
}

// Live describes a connected node.
type Live struct {
	Name     string    `json:"name"`
	Platform string    `json:"platform"`
	Caps     []string  `json:"caps"`
	Since    time.Time `json:"since"`
}

func (h *Hub) heartbeat() time.Duration {
	if h.Heartbeat > 0 {
		return h.Heartbeat
	}
	return 20 * time.Second
}

func (h *Hub) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

// Online returns the connected nodes by name.
func (h *Hub) Online() map[string]Live {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]Live, len(h.peers))
	for name, p := range h.peers {
		out[name] = Live{Name: name, Platform: p.platform, Caps: slices.Clone(p.caps), Since: p.since}
	}
	return out
}

func (h *Hub) peer(name string) *peer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.peers[name]
}

// attach makes p the node's connection, closing any older one.
func (h *Hub) attach(p *peer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.peers == nil {
		h.peers = map[string]*peer{}
	}
	if old := h.peers[p.name]; old != nil {
		old.close()
	}
	h.peers[p.name] = p
}

func (h *Hub) detach(p *peer) {
	h.mu.Lock()
	if h.peers[p.name] == p {
		delete(h.peers, p.name)
	}
	h.mu.Unlock()
	p.close()
}

// Disconnect drops a node's connection, if it has one (after a revoke).
func (h *Hub) Disconnect(name string) {
	if p := h.peer(name); p != nil {
		h.detach(p)
	}
}

// Errors from Invoke.
var (
	ErrOffline = errors.New("node is not connected")
	ErrNoCap   = errors.New("node does not offer that command")
)

// Invoke runs command on a connected node and waits for its result, until
// timeout or ctx ends.
func (h *Hub) Invoke(ctx context.Context, node, command string, args json.RawMessage, timeout time.Duration) (*Result, error) {
	p := h.peer(node)
	if p == nil {
		return nil, fmt.Errorf("%w: %s", ErrOffline, node)
	}
	if !slices.Contains(p.caps, command) {
		return nil, fmt.Errorf("%w: %s offers %s, not %s", ErrNoCap, node, strings.Join(p.caps, ", "), command)
	}
	if ok, err := h.Store.Verify(ctx, p.name, p.secret); err != nil || !ok {
		h.detach(p)
		return nil, fmt.Errorf("%w: %s is no longer paired", ErrOffline, node)
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	id := "inv" + strconv.FormatUint(h.seq.Add(1), 10)
	answer := make(chan *Result, 1)
	p.mu.Lock()
	if p.waiting == nil {
		p.waiting = map[string]chan *Result{}
	}
	p.waiting[id] = answer
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.waiting, id); p.mu.Unlock() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case p.send <- Frame{Type: "invoke", ID: id, Command: command, Args: args}:
	case <-p.done:
		return nil, fmt.Errorf("%w: %s disconnected", ErrOffline, node)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("%s did not take the request within %s", node, timeout)
	}
	select {
	case r := <-answer:
		if r == nil {
			r = &Result{}
		}
		return r, nil
	case <-p.done:
		return nil, fmt.Errorf("%w: %s disconnected before answering", ErrOffline, node)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("%s did not answer within %s", node, timeout)
	}
}

// throttled reports whether too many pairings or logins failed in the last
// minute; record notes one more failure.
func (h *Hub) throttled() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	cut := time.Now().Add(-time.Minute)
	h.fails = slices.DeleteFunc(h.fails, func(t time.Time) bool { return t.Before(cut) })
	return len(h.fails) >= 20
}

func (h *Hub) record() {
	h.mu.Lock()
	h.fails = append(h.fails, time.Now())
	h.mu.Unlock()
}

// login checks a node's name and secret and returns its new connection.
func (h *Hub) login(ctx context.Context, name, secret, platform string, caps []string) (*peer, error) {
	if h.throttled() {
		return nil, errors.New("too many failed attempts; wait a minute")
	}
	ok, err := h.Store.Verify(ctx, name, secret)
	if err != nil {
		return nil, err
	}
	if !ok {
		h.record()
		return nil, errors.New("unknown node or wrong secret; pair again with a new code")
	}
	var clean []string
	for _, c := range caps {
		if validCommand(c) && !slices.Contains(clean, c) {
			clean = append(clean, c)
		}
	}
	slices.Sort(clean)
	if len(platform) > 64 {
		platform = platform[:64]
	}
	p := &peer{name: name, platform: platform, secret: secret, caps: clean, since: time.Now(),
		send: make(chan Frame, 16), done: make(chan struct{})}
	if err := h.Store.Seen(ctx, name, platform, clean); err != nil {
		h.log().Warn("record node failed", "node", name, "err", err)
	}
	return p, nil
}

func validCommand(c string) bool {
	if len(c) > 64 {
		return false
	}
	return commandRE.MatchString(c)
}

// Public serves the endpoints devices use, under /nodes. Every request
// carries a pairing code or a node's secret, so they sit outside the API
// token.
func (h *Hub) Public() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /nodes", h.serveWS)
	mux.HandleFunc("POST /nodes/pair", h.pair)
	mux.HandleFunc("GET /nodes/events", h.events)
	mux.HandleFunc("POST /nodes/result", h.result)
	return mux
}

// Admin serves the dashboard's node API under /api/nodes; the caller puts
// it behind the API token.
func (h *Hub) Admin() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/nodes", h.list)
	mux.HandleFunc("POST /api/nodes/pair", h.newCode)
	mux.HandleFunc("DELETE /api/nodes/{name}", h.revoke)
	return mux
}

type pairRequest struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
}

func (h *Hub) pair(w http.ResponseWriter, r *http.Request) {
	var req pairRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "send JSON {code, name}")
		return
	}
	if h.throttled() {
		jsonError(w, http.StatusTooManyRequests, "too many failed attempts; wait a minute")
		return
	}
	if !ValidName(req.Name) {
		jsonError(w, http.StatusBadRequest, "name: use letters, digits, '.', '_' or '-' (up to 63)")
		return
	}
	secret, err := h.Store.Redeem(r.Context(), req.Code, req.Name, req.Platform)
	if errors.Is(err, ErrBadCode) {
		h.record()
		jsonError(w, http.StatusForbidden, err.Error())
		return
	}
	if err != nil {
		h.log().Error("pair node failed", "err", err)
		jsonError(w, http.StatusInternalServerError, "pairing failed")
		return
	}
	h.Disconnect(req.Name) // a re-paired name drops the old device
	h.log().Info("node paired", "node", req.Name, "platform", req.Platform)
	writeJSON(w, http.StatusOK, map[string]string{"name": req.Name, "secret": secret})
}

// serveWS holds a `hk node run` connection: hello, then invocations
// down and results up until either side goes away.
func (h *Hub) serveWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil) // browsers from other origins are refused
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	c.SetReadLimit(MaxFrame)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	hctx, hcancel := context.WithTimeout(ctx, 10*time.Second)
	var hello Frame
	err = wsjson.Read(hctx, c, &hello)
	hcancel()
	if err != nil || hello.Type != "hello" {
		_ = c.Close(websocket.StatusPolicyViolation, "expected hello")
		return
	}
	p, err := h.login(ctx, hello.Name, hello.Secret, hello.Platform, hello.Caps)
	if err != nil {
		_ = wsjson.Write(ctx, c, Frame{Type: "error", Error: err.Error()})
		_ = c.Close(websocket.StatusPolicyViolation, "not paired")
		return
	}
	if err := wsjson.Write(ctx, c, Frame{Type: "welcome", Caps: p.caps}); err != nil {
		return
	}
	h.attach(p)
	defer h.detach(p)
	h.log().Info("node connected", "node", p.name, "platform", p.platform, "caps", strings.Join(p.caps, ","))
	defer h.log().Info("node disconnected", "node", p.name)

	go func() { // writer
		for {
			select {
			case f := <-p.send:
				if err := wsjson.Write(ctx, c, f); err != nil {
					cancel()
					return
				}
			case <-p.done:
				cancel()
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	go h.beat(ctx, cancel, p, func(ctx context.Context) error { return c.Ping(ctx) })

	for {
		var f Frame
		if err := wsjson.Read(ctx, c, &f); err != nil {
			return
		}
		switch f.Type {
		case "result":
			p.deliver(f.ID, f.Result)
		case "ping":
			select {
			case p.send <- Frame{Type: "pong"}:
			default:
			}
		}
	}
}

// beat pings a connection and re-checks its node's pairing every
// heartbeat, ending the connection when either fails.
func (h *Hub) beat(ctx context.Context, cancel func(), p *peer, ping func(context.Context) error) {
	t := time.NewTicker(h.heartbeat())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.done:
			return
		case <-t.C:
		}
		if ping != nil {
			pctx, pcancel := context.WithTimeout(ctx, h.heartbeat())
			err := ping(pctx)
			pcancel()
			if err != nil {
				cancel()
				return
			}
		}
		if ok, err := h.Store.Verify(ctx, p.name, p.secret); err == nil && !ok {
			h.log().Info("node revoked; disconnecting", "node", p.name)
			h.detach(p)
			cancel()
			return
		}
	}
}

// nodeAuth reads a browser node's credentials: X-Habakkuk-Node names it and
// the Authorization header carries its secret.
func nodeAuth(r *http.Request) (name, secret string) {
	secret, _ = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return r.Header.Get("X-Habakkuk-Node"), secret
}

// events holds a browser node's connection: Server-Sent Events carry
// invocations down, and results come back through POST /nodes/result.
// Browsers cannot set headers on a websocket, so this is how the dashboard
// authenticates with its node secret.
func (h *Hub) events(w http.ResponseWriter, r *http.Request) {
	name, secret := nodeAuth(r)
	var caps []string
	if v := r.URL.Query().Get("caps"); v != "" {
		caps = strings.Split(v, ",")
	}
	platform := r.URL.Query().Get("platform")
	if platform == "" {
		platform = "browser"
	}
	p, err := h.login(r.Context(), name, secret, platform, caps)
	if err != nil {
		jsonError(w, http.StatusUnauthorized, err.Error())
		return
	}
	rc := http.NewResponseController(w)
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	write := func(f Frame) error {
		data, _ := json.Marshal(f)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.Type, data); err != nil {
			return err
		}
		return rc.Flush()
	}
	if write(Frame{Type: "welcome", Caps: p.caps}) != nil {
		return
	}
	h.attach(p)
	defer h.detach(p)
	h.log().Info("node connected", "node", p.name, "platform", p.platform, "caps", strings.Join(p.caps, ","))
	defer h.log().Info("node disconnected", "node", p.name)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// The keepalive comment is the ping: a write to a dead client fails.
	go h.beat(ctx, cancel, p, nil)
	t := time.NewTicker(h.heartbeat())
	defer t.Stop()
	for {
		select {
		case f := <-p.send:
			if write(f) != nil {
				return
			}
		case <-t.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case <-p.done:
			return
		case <-ctx.Done():
			return
		}
	}
}

// result takes a browser node's answer to an invocation.
func (h *Hub) result(w http.ResponseWriter, r *http.Request) {
	name, secret := nodeAuth(r)
	p := h.peer(name)
	if p == nil || !sameSecret(p.secret, secret) {
		h.record()
		jsonError(w, http.StatusUnauthorized, "this node is not connected")
		return
	}
	var f Frame
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxFrame)).Decode(&f); err != nil {
		jsonError(w, http.StatusBadRequest, "send JSON {id, result}")
		return
	}
	p.deliver(f.ID, f.Result)
	w.WriteHeader(http.StatusNoContent)
}

// NodeInfo is a paired node with its live state, for the dashboard and the
// nodes_list tool.
type NodeInfo struct {
	Node
	Online bool `json:"online"`
}

// List returns every paired node with whether it is connected; a live
// node's capabilities are the ones it offers now.
func (h *Hub) List(ctx context.Context) ([]NodeInfo, error) {
	paired, err := h.Store.List(ctx)
	if err != nil {
		return nil, err
	}
	live := h.Online()
	out := make([]NodeInfo, 0, len(paired))
	for _, n := range paired {
		info := NodeInfo{Node: n}
		if l, ok := live[n.Name]; ok {
			info.Online, info.Caps = true, l.Caps
			if l.Platform != "" {
				info.Platform = l.Platform
			}
		}
		out = append(out, info)
	}
	return out, nil
}

func (h *Hub) list(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.List(r.Context())
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

func (h *Hub) newCode(w http.ResponseWriter, r *http.Request) {
	ttl := h.PairTTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	code, expires, err := h.Store.NewCode(r.Context(), ttl)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": code, "expires": expires.UTC()})
}

func (h *Hub) revoke(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ok, err := h.Store.Revoke(r.Context(), name)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		jsonError(w, http.StatusNotFound, "no node "+strconv.Quote(name))
		return
	}
	h.Disconnect(name)
	h.log().Info("node revoked", "node", name)
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
