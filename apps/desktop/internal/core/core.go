// Package core is the desktop app without its windows: it switches between
// connections, runs and supervises a local hk serve, points the loopback
// proxy at the active Habakkuk, watches it for notifications and status, and
// serves the app's own JSON API. The native shell (package main) implements
// UI and calls in from the tray, hotkey and notifications.
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/connections"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/habakkukapi"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/local"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/notify"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/server"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/supervisor"
)

// UI is what the core asks of the native shell. Methods may be called from
// any goroutine.
type UI interface {
	// ShowMain shows the main window at path on the loopback server, e.g.
	// /app/#/approvals or /desktop/.
	ShowMain(path string)
	// Notify shows a desktop notification.
	Notify(n notify.Notice)
	// Changed says the state (connections, status, local serve) changed, so
	// the tray should be redrawn.
	Changed()
	// MainFocused reports whether the main window has focus (the user sees
	// the dashboard, so replies need no notification).
	MainFocused() bool
	// AskVisible reports whether the quick ask window is showing.
	AskVisible() bool
	SetHotkey(accel string) error
	SetStartAtLogin(on bool) error
}

// Controller ties the pieces together. Set the exported fields, then call
// Start.
type Controller struct {
	Conns   *connections.Manager
	Server  *server.Server
	UI      UI
	Log     *slog.Logger
	Version string

	// FindBinary locates habakkuk for a local connection (default
	// local.FindBinary).
	FindBinary func(explicit string) (string, error)
	// Tune adjusts each watcher before it runs (tests shorten intervals).
	Tune func(*notify.Watcher)
	// Grace is how long a local serve gets to stop (default 10s).
	Grace time.Duration

	mu       sync.Mutex
	ctx      context.Context
	gen      int // bumped by every (re)connect, so stale callbacks are ignored
	status   notify.Status
	problem  string
	endpoint local.Endpoint
	client   *habakkukapi.Client
	cancelW  context.CancelFunc
	sups     map[string]*supervisor.Supervisor // local connections' serve, by connection id
	external bool                              // the active local serve runs outside the app
	askConv  string
}

// Start connects to the active connection. ctx bounds everything the
// controller runs.
func (c *Controller) Start(ctx context.Context) {
	c.mu.Lock()
	c.ctx = ctx
	if c.sups == nil {
		c.sups = map[string]*supervisor.Supervisor{}
	}
	c.mu.Unlock()
	c.apply()
}

func (c *Controller) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.New(slog.DiscardHandler)
}

// apply (re)connects to the active connection from scratch.
func (c *Controller) apply() {
	conn, ok := c.Conns.Active()
	c.mu.Lock()
	c.gen++
	gen := c.gen
	c.stopWatcherLocked()
	c.status, c.problem, c.external, c.endpoint, c.client = notify.Status{}, "", false, local.Endpoint{}, nil
	c.mu.Unlock()
	if !ok {
		c.disconnect()
		return
	}
	var ep local.Endpoint
	var err error
	switch conn.Kind {
	case connections.Remote:
		ep.URL = conn.URL
		ep.Token, err = c.Conns.Token(conn.ID)
	case connections.Local:
		ep, err = c.prepareLocal(conn)
	}
	if err != nil {
		c.mu.Lock()
		if gen == c.gen {
			c.problem = err.Error()
		}
		c.mu.Unlock()
		c.disconnect()
		return
	}
	c.connect(gen, ep)
}

// disconnect points the proxy nowhere.
func (c *Controller) disconnect() {
	_ = c.Server.SetTarget("", "") // clearing cannot fail
	c.UI.Changed()
}

// connect points the proxy and a new watcher at ep, unless a newer
// connect happened meanwhile.
func (c *Controller) connect(gen int, ep local.Endpoint) {
	c.mu.Lock()
	if gen != c.gen || c.ctx == nil {
		c.mu.Unlock()
		return
	}
	c.stopWatcherLocked()
	if err := c.Server.SetTarget(ep.URL, ep.Token); err != nil {
		c.problem = err.Error()
		c.mu.Unlock()
		c.UI.Changed()
		return
	}
	c.endpoint = ep
	c.client = &habakkukapi.Client{BaseURL: ep.URL, Token: ep.Token}
	wctx, cancel := context.WithCancel(c.ctx)
	c.cancelW = cancel
	w := &notify.Watcher{
		Client: c.client,
		Notify: c.notify,
		Status: func(st notify.Status) { c.setStatus(gen, st) },
		Quiet:  c.quiet,
		Log:    c.Log,
	}
	if c.Tune != nil {
		c.Tune(w)
	}
	c.mu.Unlock()
	go w.Run(wctx)
	c.UI.Changed()
}

func (c *Controller) stopWatcherLocked() {
	if c.cancelW != nil {
		c.cancelW()
		c.cancelW = nil
	}
}

func (c *Controller) setStatus(gen int, st notify.Status) {
	c.mu.Lock()
	if gen != c.gen {
		c.mu.Unlock()
		return
	}
	changed := c.status != st
	c.status = st
	c.mu.Unlock()
	if changed {
		c.UI.Changed()
	}
}

func (c *Controller) notify(n notify.Notice) {
	if c.Conns.Settings().Quiet {
		return
	}
	c.UI.Notify(n)
}

func (c *Controller) quiet(conv string) bool {
	if c.UI.MainFocused() {
		return true
	}
	c.mu.Lock()
	ask := c.askConv
	c.mu.Unlock()
	return conv == ask && c.UI.AskVisible()
}

// prepareLocal checks a local connection, starts its serve unless it is
// manual or already running outside the app, and resolves its endpoint.
func (c *Controller) prepareLocal(conn connections.Connection) (local.Endpoint, error) {
	home := local.Home(conn.Home)
	if _, err := local.ReadHTTP(home); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return local.Endpoint{}, fmt.Errorf("no Habakkuk is set up in %s yet: run `hk setup`, then add an http section", home)
		}
		return local.Endpoint{}, err
	}
	sup, err := c.supervisor(conn)
	if err != nil && !conn.Manual {
		return local.Endpoint{}, err
	}
	now := time.Now()
	if hb, herr := local.ReadHeartbeat(home); herr == nil && hb.Alive(now) && (sup == nil || hb.PID != sup.Status().PID) {
		c.mu.Lock()
		c.external = true
		c.mu.Unlock()
		return local.Resolve(home, "", now)
	}
	addr := ""
	if sup != nil {
		if !conn.Manual {
			if err := sup.Start(); err != nil {
				return local.Endpoint{}, fmt.Errorf("start hk serve: %w", err)
			}
		}
		addr = sup.Status().Addr
	}
	return local.Resolve(home, addr, now)
}

// supervisor returns the connection's supervisor. A stopped one is made
// afresh, so an edited home or binary takes effect.
func (c *Controller) supervisor(conn connections.Connection) (*supervisor.Supervisor, error) {
	c.mu.Lock()
	sup := c.sups[conn.ID]
	if sup != nil && sup.Status().State == supervisor.Stopped {
		delete(c.sups, conn.ID)
		sup = nil
	}
	c.mu.Unlock()
	if sup != nil {
		return sup, nil
	}
	find := c.FindBinary
	if find == nil {
		find = local.FindBinary
	}
	bin, err := find(conn.Binary)
	if err != nil {
		return nil, err
	}
	id := conn.ID
	sup = &supervisor.Supervisor{
		Binary: bin,
		Env:    []string{"HABAKKUK_HOME=" + local.Home(conn.Home), "PATH=" + local.SearchPath()},
		Grace:  c.Grace,
		Log:    c.log().With("connection", conn.Name),
	}
	sup.OnChange = func(st supervisor.Status) { c.localChanged(id, st) }
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev := c.sups[id]; prev != nil {
		return prev, nil
	}
	c.sups[id] = sup
	return sup, nil
}

// localChanged follows a supervised serve: when it prints its address,
// the proxy moves there.
func (c *Controller) localChanged(id string, st supervisor.Status) {
	act, ok := c.Conns.Active()
	if ok && act.ID == id && st.State == supervisor.Running && st.Addr != "" {
		c.mu.Lock()
		gen, cur := c.gen, c.endpoint.URL
		c.mu.Unlock()
		if ep, err := local.Resolve(local.Home(act.Home), st.Addr, time.Now()); err == nil && ep.URL != cur {
			c.connect(gen, ep)
		}
	}
	c.UI.Changed()
}

// Activate switches to another connection and shows it.
func (c *Controller) Activate(id string) error {
	if _, err := c.Conns.Activate(id); err != nil {
		return err
	}
	c.apply()
	c.UI.ShowMain("/desktop/#wait")
	return nil
}

// Reconnect applies the active connection again (after an edit).
func (c *Controller) Reconnect() { c.apply() }

// StartLocal starts the active local connection's serve (resume).
func (c *Controller) StartLocal() error {
	conn, ok := c.Conns.Active()
	if !ok || conn.Kind != connections.Local {
		return errors.New("the active connection is not on this computer")
	}
	sup, err := c.supervisor(conn)
	if err != nil {
		return err
	}
	if err := sup.Start(); err != nil {
		return err
	}
	c.apply()
	return nil
}

// StopLocal stops the active local connection's serve (pause).
func (c *Controller) StopLocal() error {
	conn, ok := c.Conns.Active()
	if !ok || conn.Kind != connections.Local {
		return errors.New("the active connection is not on this computer")
	}
	c.mu.Lock()
	sup := c.sups[conn.ID]
	c.mu.Unlock()
	if sup == nil {
		return errors.New("hk serve was not started by the app")
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.grace()+5*time.Second)
	defer cancel()
	err := sup.Stop(ctx)
	c.UI.Changed()
	return err
}

func (c *Controller) grace() time.Duration {
	if c.Grace > 0 {
		return c.Grace
	}
	return 10 * time.Second
}

// Approve answers an approval on the active connection.
func (c *Controller) Approve(ctx context.Context, id string, approve bool) error {
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return errors.New("not connected")
	}
	return client.Answer(ctx, id, approve)
}

// SetAskConversation records the quick ask window's conversation.
func (c *Controller) SetAskConversation(conv string) {
	c.mu.Lock()
	c.askConv = conv
	c.mu.Unlock()
}

// Shutdown stops watching and stops every serve the app started.
func (c *Controller) Shutdown(ctx context.Context) {
	c.mu.Lock()
	c.gen++
	c.stopWatcherLocked()
	sups := make([]*supervisor.Supervisor, 0, len(c.sups))
	for _, s := range c.sups {
		sups = append(sups, s)
	}
	c.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range sups {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Stop(ctx); err != nil {
				c.log().Warn("stop hk serve", "err", err)
			}
		}()
	}
	wg.Wait()
}

// ConnView is a connection as the UI sees it.
type ConnView struct {
	connections.Connection
	HasToken bool `json:"has_token"`
}

// LocalView is the active local connection's serve.
type LocalView struct {
	supervisor.Status
	External bool   `json:"external"`
	Home     string `json:"home"`
	Binary   string `json:"binary,omitempty"`
}

// State is everything the tray and the connections page show.
type State struct {
	Version     string               `json:"version"`
	Active      string               `json:"active"`
	ActiveName  string               `json:"active_name"`
	ActiveKind  string               `json:"active_kind"`
	Connections []ConnView           `json:"connections"`
	Settings    connections.Settings `json:"settings"`
	Status      notify.Status        `json:"status"`
	Problem     string               `json:"problem,omitempty"`
	Local       *LocalView           `json:"local"`
}

// Snapshot returns the current state.
func (c *Controller) Snapshot() State {
	st := State{Version: c.Version, Settings: c.Conns.Settings(), Connections: []ConnView{}}
	for _, conn := range c.Conns.List() {
		st.Connections = append(st.Connections, ConnView{Connection: conn, HasToken: conn.Kind == connections.Remote && c.Conns.HasToken(conn.ID)})
	}
	act, ok := c.Conns.Active()
	c.mu.Lock()
	st.Status, st.Problem = c.status, c.problem
	sup := c.sups[act.ID]
	external := c.external
	c.mu.Unlock()
	if !ok {
		return st
	}
	st.Active, st.ActiveName, st.ActiveKind = act.ID, act.Name, act.Kind
	if act.Kind == connections.Local {
		lv := &LocalView{Status: supervisor.Status{State: supervisor.Stopped}, External: external, Home: local.Home(act.Home)}
		if sup != nil {
			lv.Status = sup.Status()
			lv.Binary = sup.Binary
		}
		st.Local = lv
	}
	return st
}

// Logs returns the active local serve's output after line n.
func (c *Controller) Logs(after int64) ([]supervisor.Line, int64) {
	act, ok := c.Conns.Active()
	c.mu.Lock()
	sup := c.sups[act.ID]
	c.mu.Unlock()
	if !ok || sup == nil {
		return nil, 0
	}
	return sup.Lines(after)
}
