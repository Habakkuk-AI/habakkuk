// Package browser gives agents a web browser over the Chrome DevTools
// Protocol: a headless Chrome or Chromium Habakkuk starts per agent, or tabs in
// a Chrome someone already runs. Pages are read as compact accessibility
// snapshots with numbered refs that the action tools take. Every request a
// driven tab makes passes the URL guard, and page content is always handed to
// the model as untrusted data.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// Manager owns one browser per agent, started on the first call and closed
// after it has been idle for the configured time.
type Manager struct {
	cfg   config.Browser
	home  string
	guard *Guard
	log   *slog.Logger

	mu     sync.Mutex
	agents map[string]*instance
	closed bool
}

// New returns a manager for cfg (defaults applied); home is $HABAKKUK_HOME,
// under which launched browsers keep their profiles.
func New(cfg config.Browser, home string, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{
		cfg: cfg, home: home, log: log, agents: map[string]*instance{},
		guard: NewGuard(cfg.AllowedURLs, cfg.BlockedURLs, cfg.AllowPrivate),
	}
}

// Guard is the URL guard every driven tab's requests pass.
func (m *Manager) Guard() *Guard { return m.guard }

type instance struct {
	m     *Manager
	agent string
	mu    sync.Mutex // one call at a time per agent
	b     *browser
	last  time.Time
	idle  *time.Timer
}

var errShutDown = errors.New("the browser toolset is shut down")

func (m *Manager) instance(agent string) (*instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errShutDown
	}
	in := m.agents[agent]
	if in == nil {
		in = &instance{m: m, agent: agent}
		m.agents[agent] = in
	}
	return in, nil
}

// do runs fn with the agent's browser, starting it (again) when needed, under
// a deadline of timeout (the configured per-call timeout when zero). Notes
// about what happened in the background (a popup, a dialog, a blocked
// request) are put before fn's output.
func (m *Manager) do(ctx context.Context, agent string, timeout time.Duration, fn func(ctx context.Context, b *browser) (string, error)) (string, error) {
	if agent == "" {
		agent = "default"
	}
	in, err := m.instance(agent)
	if err != nil {
		return "", err
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if timeout <= 0 {
		timeout = m.cfg.Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var restarted string
	if in.b != nil && in.b.c.closed() {
		m.log.Warn("browser stopped; starting a new one", "agent", agent, "err", in.b.c.err, "stderr", in.b.stderr())
		in.b.shutdown()
		in.b = nil
		restarted = "[the browser had stopped, so this call started a new one; earlier tabs are gone]"
	}
	if in.b == nil {
		b, err := m.start(ctx, agent)
		if err != nil {
			return "", err
		}
		in.b = b
	}
	out, err := fn(ctx, in.b)
	if errors.Is(err, errClosed) {
		err = fmt.Errorf("the browser stopped during this call (%s); the next call starts a new one", in.b.stderr())
	}
	in.last = time.Now()
	if in.idle == nil {
		in.idle = time.AfterFunc(m.cfg.IdleTimeout, in.closeIfIdle)
	} else {
		in.idle.Reset(m.cfg.IdleTimeout)
	}
	notes := in.b.takeNotes()
	if restarted != "" {
		notes = append([]string{restarted}, notes...)
	}
	if len(notes) > 0 {
		out = strings.Join(notes, "\n") + "\n\n" + out
	}
	return out, err
}

func (in *instance) closeIfIdle() {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.b == nil {
		return
	}
	if wait := in.m.cfg.IdleTimeout - time.Since(in.last); wait > 0 {
		in.idle.Reset(wait)
		return
	}
	in.m.log.Info("closing idle browser", "agent", in.agent)
	in.b.shutdown()
	in.b = nil
}

// Close shuts every agent's browser down.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	agents := m.agents
	m.agents = map[string]*instance{}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, in := range agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in.mu.Lock()
			defer in.mu.Unlock()
			if in.idle != nil {
				in.idle.Stop()
			}
			if in.b != nil {
				in.b.shutdown()
				in.b = nil
			}
		}()
	}
	wg.Wait()
}

var agentDirName = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// startTimeout is how long a browser may take to start and accept the
// first commands, separate from the timeout of the action that starts it.
var startTimeout = time.Minute

func (m *Manager) start(ctx context.Context, agent string) (*browser, error) {
	return m.startWith(ctx, agent, &m.cfg)
}

// startWith starts a browser with the given settings.
func (m *Manager) startWith(ctx context.Context, agent string, cfg *config.Browser) (*browser, error) {
	b := &browser{m: m, tabs: map[string]*tab{}, sessions: map[string]string{}}
	switch cfg.Mode {
	case "connect":
		u, err := wsURL(ctx, cfg.CDPURL)
		if err != nil {
			return nil, err
		}
		t, err := dialWS(ctx, u)
		if err != nil {
			return nil, fmt.Errorf("connecting to the browser at %s: %w", cfg.CDPURL, err)
		}
		b.connect = true
		b.c = newConn(t)
	default:
		bin, err := FindBinary(cfg.Path)
		if err != nil {
			return nil, err
		}
		profile := filepath.Join(m.home, "browser", agentDirName.ReplaceAllString(agent, "_"))
		p, t, err := launch(bin, profile, *cfg.Headless, cfg.Args)
		if err != nil {
			return nil, err
		}
		b.proc = p
		b.c = newConn(t)
	}
	b.unsub = b.c.on(b.onEvent)
	// Starting gets its own time budget: a cold browser on a slow host
	// can take longer than the action that asked for it is allowed.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), startTimeout)
	stop := context.AfterFunc(ctx, func() {
		if errors.Is(ctx.Err(), context.Canceled) {
			cancel()
		}
	})
	err := b.init(sctx)
	stop()
	cancel()
	if err != nil && b.proc != nil && runtime.GOOS == "linux" && !slices.Contains(cfg.Args, "--no-sandbox") {
		// Many Linux hosts (containers, CI runners, distributions that
		// restrict unprivileged user namespaces) cannot give Chrome its
		// sandbox, and it crashes at once. Try again without it.
		m.log.Warn("browser failed to start; retrying without Chrome's sandbox", "agent", agent, "err", err)
		b.shutdown()
		next := *cfg
		next.Args = append(slices.Clone(cfg.Args), "--no-sandbox")
		return m.startWith(ctx, agent, &next)
	}
	if err != nil {
		err = fmt.Errorf("starting the browser: %w", err)
		if s := b.stderr(); s != "" {
			err = fmt.Errorf("%w\n%s", err, s)
		}
		b.shutdown()
		return nil, err
	}
	m.log.Info("browser started", "agent", agent, "mode", cfg.Mode)
	return b, nil
}

// browser is one agent's browser connection and the tabs it drives.
type browser struct {
	m       *Manager
	c       *conn
	proc    *process // nil when connected to someone else's browser
	connect bool
	unsub   func()

	mu      sync.Mutex
	tabs    map[string]*tab // by target id, attached tabs only
	order   []string        // target ids in the order first seen, for stable tab numbers
	active  *tab
	notes   []string
	blocked map[string]int // subresource requests refused since the last call, by tab
	// sessions are the browser-level sessions of all page targets, which
	// attach automatically; a tab is a session Habakkuk drives.
	sessions map[string]string // target id -> session id
}

type tab struct {
	target  string
	session string
	owned   bool // opened by Habakkuk; closed when the browser shuts down in connect mode
	refs    *refTable

	acceptDialogs bool // accept confirm and prompt dialogs during the current action
}

// init attaches to page targets and, for a launched browser, takes over
// its first page.
//
// Every page attaches automatically, and a new one starts paused: a popup a
// driven tab opens is resumed only once the URL guard watches it, so its
// first request cannot slip past; any other page is resumed at once.
func (b *browser) init(ctx context.Context) error {
	if err := b.c.call(ctx, "", "Target.setDiscoverTargets", map[string]any{"discover": true}, nil); err != nil {
		return err
	}
	if err := b.c.call(ctx, "", "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true,
		"filter": []map[string]any{{"type": "page", "exclude": false}},
	}, nil); err != nil {
		return err
	}
	if b.connect {
		return nil
	}
	// The launched browser opens about:blank; wait for its page target.
	for {
		targets, err := b.pages(ctx)
		if err != nil {
			return err
		}
		if len(targets) > 0 {
			t, err := b.attach(ctx, targets[0].TargetID, true)
			if err != nil {
				return err
			}
			b.mu.Lock()
			b.active = t
			b.mu.Unlock()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

type targetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	OpenerID string `json:"openerId"`
	Attached bool   `json:"attached"`
}

// session returns the browser-level session of a page target, attaching
// to it when auto-attach has not (yet) delivered one.
func (b *browser) session(ctx context.Context, targetID string) (string, error) {
	wait, cancel := shortly(ctx, 2*time.Second)
	defer cancel()
	for {
		b.mu.Lock()
		s, ok := b.sessions[targetID]
		b.mu.Unlock()
		if ok {
			return s, nil
		}
		select {
		case <-wait.Done():
			var res struct {
				SessionID string `json:"sessionId"`
			}
			if err := b.c.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, &res); err != nil {
				return "", err
			}
			b.mu.Lock()
			b.sessions[targetID] = res.SessionID
			b.mu.Unlock()
			return res.SessionID, nil
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// pages lists the browser's page targets in the order Habakkuk first saw
// them, without DevTools windows.
func (b *browser) pages(ctx context.Context) ([]targetInfo, error) {
	var res struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	if err := b.c.call(ctx, "", "Target.getTargets", nil, &res); err != nil {
		return nil, err
	}
	var pages []targetInfo
	for _, t := range res.TargetInfos {
		if t.Type == "page" && !strings.HasPrefix(t.URL, "devtools://") {
			pages = append(pages, t)
		}
	}
	b.mu.Lock()
	for _, t := range pages {
		if !slices.Contains(b.order, t.TargetID) {
			b.order = append(b.order, t.TargetID)
		}
	}
	live := map[string]bool{}
	for _, t := range pages {
		live[t.TargetID] = true
	}
	b.order = slices.DeleteFunc(b.order, func(id string) bool { return !live[id] })
	order := slices.Clone(b.order)
	b.mu.Unlock()
	slices.SortStableFunc(pages, func(x, y targetInfo) int {
		return slices.Index(order, x.TargetID) - slices.Index(order, y.TargetID)
	})
	return pages, nil
}

// attach starts driving a page target: page events, the URL guard on every
// request (also in its frames and workers), and dialog handling.
func (b *browser) attach(ctx context.Context, targetID string, owned bool) (*tab, error) {
	b.mu.Lock()
	if t := b.tabs[targetID]; t != nil {
		b.mu.Unlock()
		return t, nil
	}
	b.mu.Unlock()
	session, err := b.session(ctx, targetID)
	if err != nil {
		return nil, err
	}
	return b.drive(ctx, targetID, session, owned)
}

// drive makes the session of a page target a tab.
func (b *browser) drive(ctx context.Context, targetID, session string, owned bool) (*tab, error) {
	t := &tab{target: targetID, session: session, owned: owned, refs: newRefTable()}
	b.mu.Lock()
	b.tabs[targetID] = t
	if !slices.Contains(b.order, targetID) {
		b.order = append(b.order, targetID)
	}
	b.mu.Unlock()
	if err := b.setupSession(ctx, t.session, true); err != nil {
		b.mu.Lock()
		delete(b.tabs, targetID)
		b.mu.Unlock()
		return nil, err
	}
	return t, nil
}

// fetchAll intercepts every request at the request stage, for the guard.
var fetchAll = map[string]any{"patterns": []map[string]any{{"urlPattern": "*", "requestStage": "Request"}}}

// setupSession prepares a page, or a frame or worker inside one. Related
// targets (out-of-process frames, workers) and popups attach paused, so the
// guard is in place before their first request. The order matters: a paused
// target answers Fetch and Target commands, but Page.enable needs its
// renderer, which waits until it is resumed.
func (b *browser) setupSession(ctx context.Context, session string, page bool) error {
	if err := b.c.call(ctx, session, "Fetch.enable", fetchAll, nil); err != nil && page {
		return err
	}
	_ = b.c.call(ctx, session, "Target.setAutoAttach",
		map[string]any{"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true}, nil)
	// A target attached paused waits for this; one attached running
	// ignores it.
	_ = b.c.call(ctx, session, "Runtime.runIfWaitingForDebugger", nil, nil)
	if page {
		return b.c.call(ctx, session, "Page.enable", nil, nil)
	}
	return nil
}

// onEvent runs on the connection's read loop: it must not block.
func (b *browser) onEvent(m message) {
	switch m.Method {
	case "Fetch.requestPaused":
		go b.decide(m)
	case "Target.attachedToTarget":
		var p struct {
			SessionID          string     `json:"sessionId"`
			TargetInfo         targetInfo `json:"targetInfo"`
			WaitingForDebugger bool       `json:"waitingForDebugger"`
		}
		if json.Unmarshal(m.Params, &p) != nil {
			return
		}
		if m.SessionID != "" {
			// A frame or worker of a driven page: guard it, then let it run.
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = b.setupSession(ctx, p.SessionID, false)
			}()
			return
		}
		if p.TargetInfo.Type != "page" {
			return
		}
		b.mu.Lock()
		b.sessions[p.TargetInfo.TargetID] = p.SessionID
		opener := b.tabs[p.TargetInfo.OpenerID]
		b.mu.Unlock()
		switch {
		case opener != nil:
			go b.adoptPopup(p.TargetInfo.TargetID, p.SessionID)
		case p.WaitingForDebugger:
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = b.c.call(ctx, p.SessionID, "Runtime.runIfWaitingForDebugger", nil, nil)
			}()
		}
	case "Target.targetDestroyed", "Target.detachedFromTarget":
		var p struct {
			TargetID  string `json:"targetId"`
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(m.Params, &p) != nil {
			return
		}
		b.mu.Lock()
		for id, s := range b.sessions {
			if id == p.TargetID || s == p.SessionID {
				delete(b.sessions, id)
			}
		}
		for id, t := range b.tabs {
			if id == p.TargetID || (p.SessionID != "" && t.session == p.SessionID) {
				delete(b.tabs, id)
				if b.active == t {
					b.active = nil
				}
			}
		}
		b.mu.Unlock()
	case "Page.frameNavigated":
		var p struct {
			Frame struct {
				ParentID string `json:"parentId"`
			} `json:"frame"`
		}
		if json.Unmarshal(m.Params, &p) == nil && p.Frame.ParentID == "" {
			if t := b.bySession(m.SessionID); t != nil {
				t.refs.reset()
			}
		}
	case "Page.javascriptDialogOpening":
		var p struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if json.Unmarshal(m.Params, &p) != nil {
			return
		}
		t := b.bySession(m.SessionID)
		accept := p.Type == "alert" || p.Type == "beforeunload"
		if t != nil {
			b.mu.Lock()
			accept = accept || t.acceptDialogs
			b.mu.Unlock()
		}
		verb := "dismissed"
		if accept {
			verb = "accepted"
		}
		b.note(fmt.Sprintf("[the page showed a %s dialog, %s: %q]", p.Type, verb, clip(p.Message, 300)))
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = b.c.call(ctx, m.SessionID, "Page.handleJavaScriptDialog", map[string]any{"accept": accept}, nil)
		}()
	}
}

// decide lets a paused request continue or fails it, by the guard.
func (b *browser) decide(m message) {
	var p struct {
		RequestID    string `json:"requestId"`
		ResourceType string `json:"resourceType"`
		Request      struct {
			URL string `json:"url"`
		} `json:"request"`
	}
	if json.Unmarshal(m.Params, &p) != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	page := p.ResourceType == "Document"
	if err := b.m.guard.Check(ctx, p.Request.URL, page); err != nil {
		if page {
			b.note("[blocked: " + err.Error() + "]")
		} else {
			b.mu.Lock()
			if b.blocked == nil {
				b.blocked = map[string]int{}
			}
			b.blocked[hostOf(p.Request.URL)]++
			b.mu.Unlock()
		}
		_ = b.c.call(ctx, m.SessionID, "Fetch.failRequest", map[string]any{"requestId": p.RequestID, "errorReason": "BlockedByClient"}, nil)
		return
	}
	_ = b.c.call(ctx, m.SessionID, "Fetch.continueRequest", map[string]any{"requestId": p.RequestID}, nil)
}

// adoptPopup drives a tab a driven page opened. It arrives paused, so the
// guard is in place before it loads anything.
func (b *browser) adoptPopup(targetID, session string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t, err := b.drive(ctx, targetID, session, true)
	if err != nil {
		return
	}
	b.mu.Lock()
	b.active = t
	b.mu.Unlock()
	b.note("[the page opened a new tab, which is now the current tab]")
}

func (b *browser) bySession(session string) *tab {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range b.tabs {
		if t.session == session {
			return t
		}
	}
	return nil
}

func (b *browser) note(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.notes) < 20 && !slices.Contains(b.notes, s) {
		b.notes = append(b.notes, s)
	}
}

func (b *browser) takeNotes() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	notes := b.notes
	b.notes = nil
	if len(b.blocked) > 0 {
		hosts := make([]string, 0, len(b.blocked))
		n := 0
		for h, c := range b.blocked {
			hosts = append(hosts, h)
			n += c
		}
		slices.Sort(hosts)
		if len(hosts) > 5 {
			hosts = append(hosts[:5], "...")
		}
		notes = append(notes, fmt.Sprintf("[blocked %d page requests to %s (private, loopback or blocked addresses)]", n, strings.Join(hosts, ", ")))
		b.blocked = nil
	}
	return notes
}

// current is the tab actions apply to, opening one when there is none.
func (b *browser) current(ctx context.Context) (*tab, error) {
	b.mu.Lock()
	t := b.active
	b.mu.Unlock()
	if t != nil {
		return t, nil
	}
	return b.newTab(ctx)
}

func (b *browser) newTab(ctx context.Context) (*tab, error) {
	var res struct {
		TargetID string `json:"targetId"`
	}
	if err := b.c.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &res); err != nil {
		return nil, err
	}
	t, err := b.attach(ctx, res.TargetID, true)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.active = t
	b.mu.Unlock()
	return t, nil
}

// closeTarget closes a tab and waits until the browser has let go of it,
// so a listing right after no longer shows it.
func (b *browser) closeTarget(ctx context.Context, id string) error {
	w := b.c.wait(func(m message) bool {
		if m.Method != "Target.targetDestroyed" {
			return false
		}
		var p struct {
			TargetID string `json:"targetId"`
		}
		return json.Unmarshal(m.Params, &p) == nil && p.TargetID == id
	})
	defer w.cancel()
	if err := b.c.call(ctx, "", "Target.closeTarget", map[string]any{"targetId": id}, nil); err != nil {
		return err
	}
	wctx, cancel := shortly(ctx, 5*time.Second)
	defer cancel()
	_, _ = w.next(wctx, b.c)
	return nil
}

func (b *browser) stderr() string {
	if b.proc == nil {
		return ""
	}
	return b.proc.stderr.String()
}

// shutdown closes the browser Habakkuk started, or, in someone else's browser,
// closes the tabs Habakkuk opened and lets go of the others.
func (b *browser) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !b.c.closed() {
		if b.connect {
			// Stop holding new pages, and let go of every page.
			_ = b.c.call(ctx, "", "Target.setAutoAttach", map[string]any{"autoAttach": false, "waitForDebuggerOnStart": false}, nil)
			b.mu.Lock()
			var owned []string
			for _, t := range b.tabs {
				if t.owned {
					owned = append(owned, t.target)
				}
			}
			sessions := make([]string, 0, len(b.sessions))
			for _, s := range b.sessions {
				sessions = append(sessions, s)
			}
			b.mu.Unlock()
			for _, s := range sessions {
				_ = b.c.call(ctx, "", "Target.detachFromTarget", map[string]any{"sessionId": s}, nil)
			}
			for _, id := range owned {
				_ = b.closeTarget(ctx, id)
			}
		} else {
			_ = b.c.call(ctx, "", "Browser.close", nil, nil)
		}
	}
	if b.unsub != nil {
		b.unsub()
	}
	b.c.close()
	if b.proc != nil {
		b.proc.stop(3 * time.Second)
	}
}

func hostOf(raw string) string {
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return s
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
