// Package mcp connects Habakkuk to Model Context Protocol servers and lends
// their tools to agents. A server is a command Habakkuk spawns (stdio) or a URL
// it connects to (Streamable HTTP). The manager connects every server at
// start, reconnects with backoff when one dies, and never lets a broken
// server block startup or other tools: its tools return an error result
// saying the server is unavailable.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

const (
	connectTimeout   = 30 * time.Second // initialize plus tools/list
	defaultTimeout   = 120 * time.Second
	terminateTimeout = 2 * time.Second // after closing a child's stdin, before SIGTERM
	healthyAfter     = time.Minute     // a session this old resets the backoff when it dies
	maxBackoff       = 5 * time.Minute
	maxPages         = 100 // tools/list pages followed before giving up on a server
	clientVersion    = "0.4"
)

var errClosed = errors.New("shutting down")

// Manager owns the connections to every configured MCP server and registers
// their tools in toolset "mcp:<server>".
type Manager struct {
	// StartTimeout bounds how long Start waits for servers to come up.
	StartTimeout time.Duration
	// Tokens supplies OAuth tokens to servers with auth: oauth; set it
	// before Start.
	Tokens Tokens

	reg     *tools.Registry
	secret  func(string) string
	log     *slog.Logger
	servers map[string]*server
	start   sync.Once
	started atomic.Bool // StartBackground ran

	ctx    context.Context // canceled by Close; bounds connects and retries
	cancel context.CancelFunc
	mu     sync.Mutex // guards closed and wg.Add
	closed bool
	wg     sync.WaitGroup

	// dial replaces the configured transport; tests use in-memory pipes.
	dial func(s *server) (sdk.Transport, error)
}

// New builds a manager. It connects nothing until Start or a tool call.
// secret resolves the variable names in env and headers.
func New(cfgs map[string]*config.MCPServer, reg *tools.Registry, secret func(string) string, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := &Manager{StartTimeout: 20 * time.Second, reg: reg, secret: secret, log: log, servers: map[string]*server{}}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	for name, c := range cfgs {
		s := &server{m: m, name: name, cfg: c, risk: parseRisk(c.Risk), names: map[string]string{}, first: make(chan struct{})}
		s.approve = sliceSet(c.Approve)
		if len(c.Allow) > 0 {
			s.allow = sliceSet(c.Allow)
		}
		if c.Auth == "oauth" {
			s.oauth = &oauthHandler{s: s}
		}
		m.servers[name] = s
	}
	return m
}

// Start connects to every server in parallel and returns once each has
// answered or StartTimeout has passed, whichever is first. Servers that are
// not up keep retrying in the background; their tools are registered when
// they connect. Later calls only wait.
func (m *Manager) Start(ctx context.Context) {
	m.StartBackground()
	ctx, cancel := context.WithTimeout(ctx, m.StartTimeout)
	defer cancel()
	for _, s := range m.servers {
		select {
		case <-s.first:
		case <-ctx.Done():
			return
		}
	}
}

// StartBackground connects to every server in the background and returns
// at once, so a slow server never delays the rest of startup. Turns that
// use a server still connecting wait for it with Pending. Later calls (and
// Start) do not connect again.
func (m *Manager) StartBackground() {
	m.start.Do(func() {
		m.started.Store(true)
		for _, s := range m.servers {
			if !m.spawn(func() { s.warm(s.first) }) {
				close(s.first)
			}
		}
	})
}

// Pending waits up to wait for the servers among toolsets ("mcp:<name>")
// that are still making their first connection, and returns the names of
// those still connecting. Before StartBackground nothing is pending: tool
// calls connect on demand.
func (m *Manager) Pending(ctx context.Context, toolsets []string, wait time.Duration) []string {
	if !m.started.Load() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var out []string
	for _, ts := range toolsets {
		name, ok := strings.CutPrefix(ts, "mcp:")
		s := m.servers[name]
		if !ok || s == nil {
			continue
		}
		select {
		case <-s.first:
		case <-ctx.Done():
			select {
			case <-s.first:
			default:
				out = append(out, name)
			}
		}
	}
	return out
}

// Close stops every server (closing stdin, then signalling and killing the
// child's whole process group) and waits for background work to finish.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	var wg sync.WaitGroup
	for _, s := range m.servers {
		s.mu.Lock()
		s.closed = true
		c := s.conn
		s.conn = nil
		s.mu.Unlock()
		if c != nil {
			wg.Add(1)
			go func() { defer wg.Done(); c.close() }()
		}
	}
	wg.Wait()
	m.wg.Wait()
}

// spawn runs f in the background unless the manager is closing.
func (m *Manager) spawn(f func()) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.wg.Add(1)
	go func() { defer m.wg.Done(); f() }()
	return true
}

// register adds a server's tools to the registry. Names already given keep
// their spelling, so a reconnect does not churn the tool list.
func (m *Manager) register(s *server, list []*sdk.Tool) {
	s.mu.Lock()
	var add []*tool
	for _, t := range list {
		add = append(add, s.toolFor(t))
	}
	s.mu.Unlock()
	for _, t := range add {
		m.reg.RegisterOrReplace("mcp:"+s.name, t)
	}
}

// Status is one server's state, for `hk mcp`.
type Status struct {
	Name      string
	Transport string // stdio | http
	State     string // connected | connecting | error | idle
	Error     string
	Tools     []ToolStatus // from the last successful tools/list, after allow
}

type ToolStatus struct {
	Name   string // Habakkuk's name, mcp__<server>__<tool>
	Remote string // the server's name
	Risk   string
}

func (m *Manager) Status() []Status {
	var out []Status
	for _, name := range slices.Sorted(maps.Keys(m.servers)) {
		s := m.servers[name]
		st := Status{Name: name, Transport: "stdio"}
		if s.cfg.URL != "" {
			st.Transport = "http"
		}
		s.mu.Lock()
		switch {
		case s.conn != nil:
			st.State = "connected"
		case s.connecting != nil:
			st.State = "connecting"
		case s.lastErr != nil:
			st.State = "error"
		default:
			st.State = "idle"
		}
		if s.lastErr != nil && s.conn == nil {
			st.Error = s.lastErr.Error()
		}
		for _, t := range s.tools {
			ft := s.toolFor(t)
			st.Tools = append(st.Tools, ToolStatus{Name: ft.spec.Name, Remote: t.Name, Risk: riskName(ft.risk)})
		}
		s.mu.Unlock()
		slices.SortFunc(st.Tools, func(a, b ToolStatus) int { return strings.Compare(a.Name, b.Name) })
		out = append(out, st)
	}
	return out
}

type server struct {
	m       *Manager
	name    string
	cfg     *config.MCPServer
	risk    tools.Risk
	approve map[string]bool
	allow   map[string]bool // nil exposes every tool
	oauth   *oauthHandler   // auth: oauth; nil otherwise
	first   chan struct{}   // closed when the first connection attempt ends

	mu         sync.Mutex
	conn       *conn
	connecting chan struct{} // closed when the connect in flight finishes
	tools      []*sdk.Tool
	names      map[string]string // remote name -> Habakkuk name, never forgotten
	lastErr    error
	failures   int
	nextTry    time.Time
	closed     bool
}

// conn is one live session; cmd is set for stdio servers.
type conn struct {
	cs     *sdk.ClientSession
	cmd    *exec.Cmd
	stderr *tailBuffer
	since  time.Time
}

func (c *conn) close() {
	if c.cs != nil {
		_ = c.cs.Close()
	}
	c.killGroup()
}

// killGroup kills whatever the server left behind (npx wrappers, helpers).
func (c *conn) killGroup() {
	if c.cmd != nil && c.cmd.Process != nil {
		_ = platform.KillGroup(c.cmd.Process.Pid)
	}
}

func (s *server) timeout() time.Duration {
	if s.cfg.Timeout > 0 {
		return s.cfg.Timeout
	}
	return defaultTimeout
}

// session returns the live session, connecting first if needed. While a
// server is backing off after failures it returns the last error at once
// rather than making the caller wait.
func (s *server) session(ctx context.Context) (*conn, error) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, errClosed
		}
		if c := s.conn; c != nil {
			s.mu.Unlock()
			return c, nil
		}
		ch := s.connecting
		if ch == nil {
			if wait := time.Until(s.nextTry); wait > 0 {
				err := fmt.Errorf("%w (next attempt in %s)", s.lastErr, wait.Round(time.Second))
				s.mu.Unlock()
				return nil, err
			}
			ch = make(chan struct{})
			s.connecting = ch
			s.mu.Unlock()
			if !s.m.spawn(func() { s.connect(ch) }) {
				s.mu.Lock()
				s.connecting = nil
				s.mu.Unlock()
				close(ch)
				return nil, errClosed
			}
		} else {
			s.mu.Unlock()
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// warm keeps trying to connect until the first success, so a server that is
// down at start gets its tools registered when it comes up.
func (s *server) warm(first chan struct{}) {
	signal := sync.OnceFunc(func() { close(first) })
	defer signal()
	for {
		_, err := s.session(s.m.ctx)
		signal()
		if err == nil || s.m.ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		wait := max(time.Until(s.nextTry), 100*time.Millisecond)
		s.mu.Unlock()
		select {
		case <-time.After(wait):
		case <-s.m.ctx.Done():
			return
		}
	}
}

func (s *server) connect(done chan struct{}) {
	defer close(done)
	ctx, cancel := context.WithTimeout(s.m.ctx, connectTimeout)
	defer cancel()
	c, list, err := s.dial(ctx)
	s.mu.Lock()
	s.connecting = nil
	if err != nil {
		s.failed(err, false)
		s.mu.Unlock()
		s.m.log.Warn("mcp server unavailable", "server", s.name, "err", err)
		return
	}
	if s.closed {
		s.mu.Unlock()
		c.close()
		return
	}
	s.conn, s.tools, s.lastErr = c, list, nil
	s.mu.Unlock()
	s.m.register(s, list)
	s.m.log.Info("mcp server connected", "server", s.name, "tools", len(list))
	s.m.spawn(func() {
		_ = c.cs.Wait()
		s.drop(c, errors.New("the server closed the connection"))
	})
}

// failed records a failure and schedules the next attempt. The first death of
// a healthy server reconnects at once; repeated failures back off.
func (s *server) failed(err error, died bool) {
	delay := min(time.Second<<min(s.failures, 9), maxBackoff)
	if died && s.failures == 0 {
		delay = 0
	}
	s.failures++
	s.lastErr = err
	s.nextTry = time.Now().Add(delay)
}

// drop forgets a session that died or failed at the transport level, so the
// next call reconnects.
func (s *server) drop(c *conn, err error) {
	s.mu.Lock()
	if s.conn != c {
		s.mu.Unlock()
		return
	}
	s.conn = nil
	if time.Since(c.since) > healthyAfter {
		s.failures = 0
	}
	err = withStderr(err, c.stderr)
	s.failed(err, true)
	s.mu.Unlock()
	s.m.log.Warn("mcp server disconnected", "server", s.name, "err", err)
	if !s.m.spawn(c.close) {
		c.close()
	}
}

func (s *server) dial(ctx context.Context) (*conn, []*sdk.Tool, error) {
	c := &conn{}
	var t sdk.Transport
	var err error
	switch {
	case s.m.dial != nil:
		t, err = s.m.dial(s)
	case s.cfg.Command != "":
		t, c.cmd, c.stderr, err = s.stdio()
	default:
		t, err = s.http()
	}
	if err != nil {
		return nil, nil, err
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "habakkuk", Version: clientVersion}, nil)
	c.cs, err = client.Connect(ctx, t, nil)
	if err != nil {
		c.killGroup()
		return nil, nil, withStderr(fmt.Errorf("connect: %w", err), c.stderr)
	}
	c.since = time.Now()
	list, err := listTools(ctx, c.cs)
	if err != nil {
		c.close()
		return nil, nil, withStderr(fmt.Errorf("tools/list: %w", err), c.stderr)
	}
	var exposed []*sdk.Tool
	for _, t := range list {
		if s.allow == nil || s.allow[t.Name] {
			exposed = append(exposed, t)
		}
	}
	for name := range s.allow {
		if !slices.ContainsFunc(list, func(t *sdk.Tool) bool { return t.Name == name }) {
			s.m.log.Warn("mcp allow names a tool the server does not offer", "server", s.name, "tool", name)
		}
	}
	return c, exposed, nil
}

func listTools(ctx context.Context, cs *sdk.ClientSession) ([]*sdk.Tool, error) {
	var out []*sdk.Tool
	params := &sdk.ListToolsParams{}
	for range maxPages {
		res, err := cs.ListTools(ctx, params)
		if err != nil {
			return nil, err
		}
		out = append(out, res.Tools...)
		if res.NextCursor == "" {
			return out, nil
		}
		params = &sdk.ListToolsParams{Cursor: res.NextCursor}
	}
	return nil, fmt.Errorf("more than %d pages of tools", maxPages)
}

// stdio builds the child process. It gets PATH and HOME plus the configured
// variables, never Habakkuk's whole environment, and its own process group so
// Close can kill everything it started.
func (s *server) stdio() (sdk.Transport, *exec.Cmd, *tailBuffer, error) {
	var env, missing []string
	keys := []string{"PATH", "HOME"}
	if platform.IsWindows {
		keys = platform.MinimalEnvKeys() // node, npx and python do not start without SystemRoot and APPDATA
	}
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(s.cfg.Env)) {
		v := s.m.secret(s.cfg.Env[k])
		if v == "" {
			missing = append(missing, s.cfg.Env[k])
		}
		env = append(env, k+"="+v)
	}
	if len(missing) > 0 {
		return nil, nil, nil, fmt.Errorf("%s not set in the environment or .env", strings.Join(missing, ", "))
	}
	cmd := exec.Command(s.cfg.Command, s.cfg.Args...)
	cmd.Env = env
	stderr := &tailBuffer{}
	cmd.Stderr = stderr
	platform.SetProcessGroup(cmd)
	cmd.WaitDelay = terminateTimeout
	return &sdk.CommandTransport{Command: cmd, TerminateDuration: terminateTimeout}, cmd, stderr, nil
}

func (s *server) http() (sdk.Transport, error) {
	h := http.Header{}
	var missing []string
	for _, k := range slices.Sorted(maps.Keys(s.cfg.Headers)) {
		v, unset := s.headerValue(s.cfg.Headers[k])
		missing = append(missing, unset...)
		h.Set(k, v)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s not set in the environment or .env", strings.Join(missing, ", "))
	}
	t := &sdk.StreamableClientTransport{
		Endpoint:             s.cfg.URL,
		HTTPClient:           &http.Client{Transport: &headerTransport{base: http.DefaultTransport, header: h}},
		DisableStandaloneSSE: true, // Habakkuk only needs replies to its own requests
	}
	if s.oauth != nil {
		t.OAuthHandler = s.oauth
	}
	return t, nil
}

// headerValue resolves a configured header. A plain name is the variable
// holding the whole value; a value with ${NAME} references (for example
// "Bearer ${SEARCH_KEY}") has each reference replaced. It returns the
// names that are not set.
func (s *server) headerValue(spec string) (string, []string) {
	if !strings.Contains(spec, "${") {
		v := s.m.secret(spec)
		if v == "" {
			return "", []string{spec}
		}
		return v, nil
	}
	var missing []string
	v := os.Expand(spec, func(name string) string {
		val := s.m.secret(name)
		if val == "" {
			missing = append(missing, name)
		}
		return val
	})
	return v, missing
}

// HeaderVars lists the variables a configured header value needs.
func HeaderVars(spec string) []string {
	if !strings.Contains(spec, "${") {
		return []string{spec}
	}
	var names []string
	os.Expand(spec, func(name string) string {
		names = append(names, name)
		return ""
	})
	return names
}

type headerTransport struct {
	base   http.RoundTripper
	header http.Header
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range t.header {
		req.Header[k] = v
	}
	return t.base.RoundTrip(req)
}

// call runs one tool. Errors the server reports (JSON-RPC errors, isError
// results) leave the session alone; transport failures drop it so the next
// call reconnects.
func (s *server) call(ctx context.Context, name string, input json.RawMessage) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	c, err := s.session(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = errors.New("still connecting")
		}
		return "", fmt.Errorf("MCP server %s is unavailable: %v", s.name, err)
	}
	res, err := c.cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: json.RawMessage(input)})
	if err != nil {
		var werr *jsonrpc.Error
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return "", fmt.Errorf("MCP server %s did not answer within %s", s.name, s.timeout())
		case ctx.Err() != nil:
			return "", ctx.Err()
		case errors.As(err, &werr):
			return "", fmt.Errorf("MCP server %s: %w", s.name, err)
		}
		s.drop(c, err)
		return "", fmt.Errorf("MCP server %s connection failed during the call (%v); the call may or may not have run, so check before retrying. It reconnects on the next call", s.name, err)
	}
	text := formatResult(res)
	if res.IsError {
		if text == "" {
			text = "the tool reported an error without a message"
		}
		return "", errors.New(text)
	}
	return text, nil
}

func parseRisk(s string) tools.Risk {
	switch s {
	case "read":
		return tools.RiskRead
	case "write":
		return tools.RiskWrite
	case "exec":
		return tools.RiskExec
	}
	return tools.RiskNetwork
}

func riskName(r tools.Risk) string {
	return [...]string{"read", "write", "network", "exec"}[r]
}

func sliceSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// tailBuffer keeps the last bytes a child wrote to stderr, for error messages.
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

const tailMax = 4096

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > tailMax {
		t.b = append([]byte(nil), t.b[len(t.b)-tailMax:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}

func withStderr(err error, t *tailBuffer) error {
	if t == nil {
		return err
	}
	tail := t.String()
	if tail == "" {
		return err
	}
	if len(tail) > 300 {
		tail = "..." + tail[len(tail)-300:]
	}
	return fmt.Errorf("%w; stderr: %s", err, tail)
}
