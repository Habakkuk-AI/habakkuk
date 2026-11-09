package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// Dedicated is a browser launched for one long task outside the browser
// toolset, such as attending a meeting: its own process and profile, one
// tab, and the same URL guard on every request. It is not shared with an
// agent's browser and is never closed for being idle.
type Dedicated struct {
	b *browser
	t *tab

	mu       sync.Mutex
	bindings map[string]func(payload string)
	unsub    func()
	done     chan struct{}
	once     sync.Once
}

// DedicatedOptions configure LaunchDedicated.
type DedicatedOptions struct {
	Path     string   // the browser binary; "" finds one
	Profile  string   // user data directory, created with mode 0700; the caller removes it
	Headless bool     // run without a window
	Args     []string // extra command-line flags (fake media devices, for example)
	Guard    *Guard   // checks every request; nil refuses private addresses only
	// Language is the interface language the browser and its pages are
	// asked for (default en-US).
	Language string
	Log      *slog.Logger
}

// LaunchDedicated starts a browser and takes over its first tab. Headless
// Chrome announces itself in its user agent, which some sites refuse, so the
// tab presents the ordinary Chrome user agent instead.
func LaunchDedicated(ctx context.Context, opts DedicatedOptions) (*Dedicated, error) {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Guard == nil {
		opts.Guard = NewGuard(nil, nil, nil)
	}
	if opts.Language == "" {
		opts.Language = "en-US"
	}
	headless := opts.Headless
	m := &Manager{log: opts.Log, guard: opts.Guard, agents: map[string]*instance{},
		cfg: config.Browser{Mode: "launch", Path: opts.Path, Headless: &headless}}
	args := append(slices.Clone(opts.Args), "--lang="+opts.Language)
	b, err := startDedicated(ctx, m, opts, args)
	if err != nil && runtime.GOOS == "linux" && !slices.Contains(args, "--no-sandbox") {
		opts.Log.Warn("browser failed to start; retrying without Chrome's sandbox", "err", err)
		b, err = startDedicated(ctx, m, opts, append(args, "--no-sandbox"))
	}
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	t := b.active
	b.mu.Unlock()
	d := &Dedicated{b: b, t: t, bindings: map[string]func(string){}, done: make(chan struct{})}
	d.unsub = b.c.on(d.onEvent)
	go func() {
		<-b.c.done
		d.once.Do(func() { close(d.done) })
	}()
	if err := d.prepare(ctx, opts.Language); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func startDedicated(ctx context.Context, m *Manager, opts DedicatedOptions, args []string) (*browser, error) {
	bin, err := FindBinary(opts.Path)
	if err != nil {
		return nil, err
	}
	p, t, err := launch(bin, opts.Profile, opts.Headless, args)
	if err != nil {
		return nil, err
	}
	b := &browser{m: m, tabs: map[string]*tab{}, sessions: map[string]string{}, proc: p, c: newConn(t)}
	b.unsub = b.c.on(b.onEvent)
	if err := b.init(ctx); err != nil {
		err = fmt.Errorf("starting the browser: %w", err)
		if s := b.stderr(); s != "" {
			err = fmt.Errorf("%w\n%s", err, s)
		}
		b.shutdown()
		return nil, err
	}
	return b, nil
}

// prepare turns on the runtime events bindings need and replaces a headless
// user agent.
func (d *Dedicated) prepare(ctx context.Context, lang string) error {
	if err := d.b.c.call(ctx, d.t.session, "Runtime.enable", nil, nil); err != nil {
		return err
	}
	var v struct {
		UserAgent string `json:"userAgent"`
	}
	if err := d.b.c.call(ctx, "", "Browser.getVersion", nil, &v); err != nil {
		return err
	}
	ua := strings.ReplaceAll(v.UserAgent, "HeadlessChrome", "Chrome")
	return d.b.c.call(ctx, d.t.session, "Emulation.setUserAgentOverride", map[string]any{
		"userAgent": ua, "acceptLanguage": lang + "," + strings.SplitN(lang, "-", 2)[0] + ";q=0.9",
	}, nil)
}

func (d *Dedicated) onEvent(m message) {
	if m.Method != "Runtime.bindingCalled" || m.SessionID != d.t.session {
		return
	}
	var p struct {
		Name    string `json:"name"`
		Payload string `json:"payload"`
	}
	if json.Unmarshal(m.Params, &p) != nil {
		return
	}
	d.mu.Lock()
	fn := d.bindings[p.Name]
	d.mu.Unlock()
	if fn != nil {
		fn(p.Payload)
	}
}

// Navigate opens url in the tab and waits (up to 30 seconds) for it to load.
func (d *Dedicated) Navigate(ctx context.Context, url string) error {
	_, err := d.b.navigate(ctx, d.t, url)
	return err
}

// URL is the tab's current address.
func (d *Dedicated) URL(ctx context.Context) string {
	_, u := d.b.pageInfo(ctx, d.t)
	return u
}

// Eval runs a JavaScript expression in the page, awaiting a promise, and
// returns its value as JSON.
func (d *Dedicated) Eval(ctx context.Context, expr string) (json.RawMessage, error) {
	return d.b.eval(ctx, d.t, expr)
}

// InsertText types text into the focused element.
func (d *Dedicated) InsertText(ctx context.Context, text string) error {
	return d.b.c.call(ctx, d.t.session, "Input.insertText", map[string]any{"text": text}, nil)
}

// Click presses and releases the left mouse button at a point of the
// viewport, in CSS pixels.
func (d *Dedicated) Click(ctx context.Context, x, y float64) error {
	for _, typ := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		p := map[string]any{"type": typ, "x": x, "y": y}
		if typ != "mouseMoved" {
			p["button"], p["clickCount"] = "left", 1
		}
		if err := d.b.c.call(ctx, d.t.session, "Input.dispatchMouseEvent", p, nil); err != nil {
			return err
		}
	}
	return nil
}

// Press sends a key or shortcut ("Enter", "c", "Control+d") to the page.
func (d *Dedicated) Press(ctx context.Context, key string) error {
	return d.b.key(ctx, d.t, key)
}

// Bind adds a function name to every page the tab loads; each call of it
// with a string runs fn with that string. fn runs on the connection's read
// loop, so it must return quickly and must not call the browser.
func (d *Dedicated) Bind(ctx context.Context, name string, fn func(payload string)) error {
	d.mu.Lock()
	d.bindings[name] = fn
	d.mu.Unlock()
	return d.b.c.call(ctx, d.t.session, "Runtime.addBinding", map[string]any{"name": name}, nil)
}

// Done is closed when the browser exits or the connection to it breaks.
func (d *Dedicated) Done() <-chan struct{} { return d.done }

// Close shuts the browser down and waits for it to exit.
func (d *Dedicated) Close() {
	if d.unsub != nil {
		d.unsub()
	}
	d.b.shutdown()
	d.once.Do(func() { close(d.done) })
	// Give the process a moment to release the profile.
	if d.b.proc != nil {
		select {
		case <-d.b.proc.exited:
		case <-time.After(5 * time.Second):
		}
	}
}
