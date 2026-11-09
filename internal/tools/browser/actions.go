package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// pageInfo is the tab's current title and URL.
func (b *browser) pageInfo(ctx context.Context, t *tab) (title, url string) {
	var res struct {
		TargetInfo targetInfo `json:"targetInfo"`
	}
	if b.c.call(ctx, "", "Target.getTargetInfo", map[string]any{"targetId": t.target}, &res) == nil {
		return res.TargetInfo.Title, res.TargetInfo.URL
	}
	return "", ""
}

// status is a one-line description of where the tab is now.
func (b *browser) status(ctx context.Context, t *tab) string {
	title, url := b.pageInfo(ctx, t)
	return fmt.Sprintf("Page: %s (%s)", quote(title, 150), url)
}

// shortly is a context for the part of an action that may run out of time
// without failing it (waiting for a page to finish loading): it ends a
// second before ctx does, so there is time left to report.
func shortly(ctx context.Context, max time.Duration) (context.Context, context.CancelFunc) {
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl) - time.Second; left < max {
			max = left
		}
	}
	return context.WithTimeout(ctx, max)
}

// loaded matches the events that end a navigation in t: a load, a
// same-document change, or a page restored from the back-forward cache.
func loaded(t *tab) func(message) bool {
	return func(m message) bool {
		if m.SessionID != t.session {
			return false
		}
		switch m.Method {
		case "Page.loadEventFired", "Page.navigatedWithinDocument":
			return true
		case "Page.frameNavigated":
			var p struct {
				Frame struct {
					ParentID string `json:"parentId"`
				} `json:"frame"`
				Type string `json:"type"`
			}
			return json.Unmarshal(m.Params, &p) == nil && p.Frame.ParentID == "" && p.Type == "BackForwardCacheRestore"
		}
		return false
	}
}

func (b *browser) navigate(ctx context.Context, t *tab, url string) (string, error) {
	if err := b.m.guard.Check(ctx, url, true); err != nil {
		return "", err
	}
	w := b.c.wait(loaded(t))
	defer w.cancel()
	var res struct {
		LoaderID  string `json:"loaderId"`
		ErrorText string `json:"errorText"`
	}
	if err := b.c.call(ctx, t.session, "Page.navigate", map[string]any{"url": url}, &res); err != nil {
		return "", err
	}
	switch {
	case strings.Contains(res.ErrorText, "ERR_BLOCKED_BY_CLIENT"):
		return "", fmt.Errorf("loading %s was blocked by the URL guard (a redirect to a refused address?)", url)
	case strings.Contains(res.ErrorText, "ERR_ABORTED"):
		return "The URL did not open as a page (it may be a download; use browser_download).\n" + b.status(ctx, t), nil
	case res.ErrorText != "":
		return "", fmt.Errorf("loading %s failed: %s", url, res.ErrorText)
	}
	note := ""
	if res.LoaderID != "" {
		wctx, cancel := shortly(ctx, 30*time.Second)
		if _, err := w.next(wctx, b.c); err != nil && !errors.Is(err, errClosed) {
			note = "\n(the page is still loading; browser_wait can wait for text to appear)"
		}
		cancel()
	}
	return b.status(ctx, t) + note + "\nUse browser_snapshot to read it.", nil
}

// settle runs an action that may start a navigation (a click, Enter) and,
// if one starts within half a second, waits for the page to load.
func (b *browser) settle(ctx context.Context, t *tab, action func() error) (string, error) {
	started := b.c.wait(func(m message) bool {
		if m.SessionID != t.session {
			return false
		}
		return m.Method == "Page.frameRequestedNavigation" || m.Method == "Page.frameStartedLoading" || m.Method == "Page.navigatedWithinDocument"
	})
	defer started.cancel()
	done := b.c.wait(loaded(t))
	defer done.cancel()
	if err := action(); err != nil {
		return "", err
	}
	sctx, cancel := shortly(ctx, 500*time.Millisecond)
	m, err := started.next(sctx, b.c)
	cancel()
	if err != nil {
		return "", nil // no navigation started: nothing to wait for
	}
	if m.Method == "Page.navigatedWithinDocument" {
		return "", nil
	}
	lctx, cancel := shortly(ctx, 30*time.Second)
	defer cancel()
	if _, err := done.next(lctx, b.c); err != nil && !errors.Is(err, errClosed) {
		return "(the new page is still loading)", nil
	}
	return "", nil
}

func (b *browser) node(t *tab, ref int) (refInfo, error) {
	info, ok := t.refs.lookup(ref)
	if !ok {
		return refInfo{}, fmt.Errorf("no element has ref %d on this page; take a new browser_snapshot", ref)
	}
	return info, nil
}

// center scrolls the element into view and returns the middle of its first
// visible box, in viewport coordinates.
func (b *browser) center(ctx context.Context, t *tab, node int64) (float64, float64, error) {
	_ = b.c.call(ctx, t.session, "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": node}, nil)
	var res struct {
		Quads [][]float64 `json:"quads"`
	}
	if err := b.c.call(ctx, t.session, "DOM.getContentQuads", map[string]any{"backendNodeId": node}, &res); err != nil {
		return 0, 0, staleErr(err)
	}
	for _, q := range res.Quads {
		if len(q) != 8 {
			continue
		}
		x := (q[0] + q[2] + q[4] + q[6]) / 4
		y := (q[1] + q[3] + q[5] + q[7]) / 4
		if area(q) > 1 {
			return x, y, nil
		}
	}
	return 0, 0, errors.New("the element is not visible (zero size or hidden); take a new browser_snapshot")
}

func area(q []float64) float64 {
	a := 0.0
	for i := 0; i < 8; i += 2 {
		j := (i + 2) % 8
		a += q[i]*q[j+1] - q[j]*q[i+1]
	}
	if a < 0 {
		a = -a
	}
	return a / 2
}

func staleErr(err error) error {
	s := err.Error()
	if strings.Contains(s, "No node") || strings.Contains(s, "Could not compute") || strings.Contains(s, "not found") {
		return errors.New("the element is gone from the page; take a new browser_snapshot")
	}
	return err
}

func (b *browser) click(ctx context.Context, t *tab, ref int, acceptDialogs bool) (string, error) {
	info, err := b.node(t, ref)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	t.acceptDialogs = acceptDialogs
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		t.acceptDialogs = false
		b.mu.Unlock()
	}()
	note, err := b.settle(ctx, t, func() error { return b.clickNode(ctx, t, info.node) })
	if err != nil {
		return "", err
	}
	return joinLines(fmt.Sprintf("Clicked %s%s [ref=%d].", info.role, nameAttr(info.name), ref), note, b.status(ctx, t)), nil
}

func (b *browser) clickNode(ctx context.Context, t *tab, node int64) error {
	x, y, err := b.center(ctx, t, node)
	if err != nil {
		return err
	}
	for _, typ := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		p := map[string]any{"type": typ, "x": x, "y": y}
		if typ != "mouseMoved" {
			p["button"], p["clickCount"] = "left", 1
		}
		if err := b.c.call(ctx, t.session, "Input.dispatchMouseEvent", p, nil); err != nil {
			return err
		}
	}
	return nil
}

// object returns a Runtime object id for a DOM node, for callFunctionOn.
func (b *browser) object(ctx context.Context, t *tab, node int64) (string, error) {
	var res struct {
		Object struct {
			ObjectID string `json:"objectId"`
		} `json:"object"`
	}
	if err := b.c.call(ctx, t.session, "DOM.resolveNode", map[string]any{"backendNodeId": node}, &res); err != nil {
		return "", staleErr(err)
	}
	return res.Object.ObjectID, nil
}

// callOn runs fn with the node as this and returns its JSON result.
func (b *browser) callOn(ctx context.Context, t *tab, node int64, fn string, args ...any) (json.RawMessage, error) {
	obj, err := b.object(ctx, t, node)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = b.c.call(context.WithoutCancel(ctx), t.session, "Runtime.releaseObject", map[string]any{"objectId": obj}, nil)
	}()
	callArgs := make([]map[string]any, len(args))
	for i, a := range args {
		callArgs[i] = map[string]any{"value": a}
	}
	var res evalResult
	if err := b.c.call(ctx, t.session, "Runtime.callFunctionOn", map[string]any{
		"functionDeclaration": fn, "objectId": obj, "arguments": callArgs,
		"returnByValue": true, "awaitPromise": true,
	}, &res); err != nil {
		return nil, err
	}
	if res.ExceptionDetails != nil {
		return nil, errors.New(res.ExceptionDetails.message())
	}
	return res.Result.Value, nil
}

type evalResult struct {
	Result struct {
		Type                string          `json:"type"`
		Subtype             string          `json:"subtype"`
		Value               json.RawMessage `json:"value"`
		UnserializableValue string          `json:"unserializableValue"`
		Description         string          `json:"description"`
	} `json:"result"`
	ExceptionDetails *exceptionDetails `json:"exceptionDetails"`
}

type exceptionDetails struct {
	Text      string `json:"text"`
	Exception *struct {
		Description string `json:"description"`
	} `json:"exception"`
}

func (r *evalResult) message() string { return r.ExceptionDetails.message() }

func (e *exceptionDetails) message() string {
	if e.Exception != nil && e.Exception.Description != "" {
		return clip(e.Exception.Description, 1000)
	}
	return e.Text
}

const selectContents = `function() {
	this.scrollIntoView({block: "center"});
	this.focus();
	if (typeof this.select === "function" && /^(INPUT|TEXTAREA)$/.test(this.tagName)) {
		this.select();
	} else if (this.isContentEditable) {
		const r = document.createRange();
		r.selectNodeContents(this);
		const s = getSelection();
		s.removeAllRanges();
		s.addRange(r);
	}
	return document.activeElement === this || this.contains(document.activeElement);
}`

const caretToEnd = `function() {
	this.focus();
	if (typeof this.setSelectionRange === "function") {
		try { const n = this.value.length; this.setSelectionRange(n, n); } catch (e) {}
	} else if (this.isContentEditable) {
		const r = document.createRange();
		r.selectNodeContents(this);
		r.collapse(false);
		const s = getSelection();
		s.removeAllRanges();
		s.addRange(r);
	}
	return document.activeElement === this || this.contains(document.activeElement);
}`

func (b *browser) typeText(ctx context.Context, t *tab, ref int, text string, submit, appendText bool) (string, error) {
	info, err := b.node(t, ref)
	if err != nil {
		return "", err
	}
	fn := selectContents
	if appendText {
		fn = caretToEnd
	}
	focused, err := b.callOn(ctx, t, info.node, fn)
	if err != nil {
		return "", err
	}
	if string(focused) != "true" {
		// Not focusable by script (a custom widget): click it instead.
		if err := b.clickNode(ctx, t, info.node); err != nil {
			return "", err
		}
	}
	if text == "" && !appendText {
		if err := b.key(ctx, t, "Backspace"); err != nil {
			return "", err
		}
	} else if text != "" {
		if err := b.c.call(ctx, t.session, "Input.insertText", map[string]any{"text": text}, nil); err != nil {
			return "", err
		}
	}
	msg := fmt.Sprintf("Typed %d characters into %s%s [ref=%d].", utf8.RuneCountInString(text), info.role, nameAttr(info.name), ref)
	if !submit {
		return msg, nil
	}
	note, err := b.settle(ctx, t, func() error { return b.key(ctx, t, "Enter") })
	if err != nil {
		return "", err
	}
	return joinLines(msg+" Pressed Enter.", note, b.status(ctx, t)), nil
}

const selectOption = `function(want) {
	if (this.tagName !== "SELECT") return {error: "not a select"};
	const norm = s => s.replace(/\s+/g, " ").trim().toLowerCase();
	const opts = Array.from(this.options);
	const o = opts.find(o => o.value === want) || opts.find(o => norm(o.label) === norm(want)) ||
		opts.find(o => norm(o.label).includes(norm(want)));
	if (!o) return {error: "no option matches", options: opts.slice(0, 30).map(o => o.label)};
	if (o.disabled) return {error: "that option is disabled"};
	this.focus();
	o.selected = true;
	this.dispatchEvent(new Event("input", {bubbles: true}));
	this.dispatchEvent(new Event("change", {bubbles: true}));
	return {label: o.label, value: o.value};
}`

func (b *browser) selectValue(ctx context.Context, t *tab, ref int, value string) (string, error) {
	info, err := b.node(t, ref)
	if err != nil {
		return "", err
	}
	raw, err := b.callOn(ctx, t, info.node, selectOption, value)
	if err != nil {
		return "", err
	}
	var res struct {
		Error   string   `json:"error"`
		Options []string `json:"options"`
		Label   string   `json:"label"`
		Value   string   `json:"value"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", err
	}
	switch {
	case res.Error == "not a select":
		return "", fmt.Errorf("ref %d is not a <select>; click it and then click the option", ref)
	case res.Options != nil:
		return "", fmt.Errorf("%s %q; the options are: %s", res.Error, value, strings.Join(res.Options, ", "))
	case res.Error != "":
		return "", errors.New(res.Error)
	}
	return fmt.Sprintf("Selected %q (value %q) in %s%s [ref=%d].", res.Label, res.Value, info.role, nameAttr(info.name), ref), nil
}

type keyDef struct {
	key, code string
	vk        int
	text      string
}

var namedKeys = map[string]keyDef{
	"enter": {"Enter", "Enter", 13, "\r"}, "return": {"Enter", "Enter", 13, "\r"},
	"tab": {"Tab", "Tab", 9, ""}, "escape": {"Escape", "Escape", 27, ""}, "esc": {"Escape", "Escape", 27, ""},
	"backspace": {"Backspace", "Backspace", 8, ""}, "delete": {"Delete", "Delete", 46, ""},
	"space": {" ", "Space", 32, " "}, "insert": {"Insert", "Insert", 45, ""},
	"arrowup": {"ArrowUp", "ArrowUp", 38, ""}, "arrowdown": {"ArrowDown", "ArrowDown", 40, ""},
	"arrowleft": {"ArrowLeft", "ArrowLeft", 37, ""}, "arrowright": {"ArrowRight", "ArrowRight", 39, ""},
	"up": {"ArrowUp", "ArrowUp", 38, ""}, "down": {"ArrowDown", "ArrowDown", 40, ""},
	"left": {"ArrowLeft", "ArrowLeft", 37, ""}, "right": {"ArrowRight", "ArrowRight", 39, ""},
	"home": {"Home", "Home", 36, ""}, "end": {"End", "End", 35, ""},
	"pageup": {"PageUp", "PageUp", 33, ""}, "pagedown": {"PageDown", "PageDown", 34, ""},
}

func init() {
	for i := 1; i <= 12; i++ {
		k := fmt.Sprintf("F%d", i)
		namedKeys[strings.ToLower(k)] = keyDef{k, k, 111 + i, ""}
	}
}

// parseKey reads "Enter", "a", "Control+a" or "Shift+Tab" into a key and
// the CDP modifier bits (Alt 1, Control 2, Meta 4, Shift 8).
func parseKey(s string) (keyDef, int, error) {
	parts := strings.Split(s, "+")
	if strings.HasSuffix(s, "++") || s == "+" { // the plus key itself
		parts = append(parts[:len(parts)-2], "+")
	}
	mods := 0
	for _, p := range parts[:len(parts)-1] {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "alt", "option":
			mods |= 1
		case "control", "ctrl":
			mods |= 2
		case "meta", "cmd", "command", "super":
			mods |= 4
		case "shift":
			mods |= 8
		default:
			return keyDef{}, 0, fmt.Errorf("unknown modifier %q (use Control, Shift, Alt or Meta)", p)
		}
	}
	name := parts[len(parts)-1]
	if k, ok := namedKeys[strings.ToLower(name)]; ok {
		return k, mods, nil
	}
	if utf8.RuneCountInString(name) != 1 {
		return keyDef{}, 0, fmt.Errorf("unknown key %q; use a single character or a name like Enter, Tab, Escape, ArrowDown, PageDown", name)
	}
	r, _ := utf8.DecodeRuneInString(name)
	k := keyDef{key: name, text: name}
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		up := strings.ToUpper(name)
		k.code, k.vk = "Key"+up, int(up[0])
		if mods&8 != 0 {
			k.key, k.text = up, up
		}
	case r >= '0' && r <= '9':
		k.code, k.vk = "Digit"+name, int(r)
	}
	if mods&(1|2|4) != 0 {
		k.text = "" // a shortcut, not typing
	}
	return k, mods, nil
}

func (b *browser) key(ctx context.Context, t *tab, s string) error {
	k, mods, err := parseKey(s)
	if err != nil {
		return err
	}
	down := map[string]any{"type": "rawKeyDown", "key": k.key, "code": k.code, "modifiers": mods,
		"windowsVirtualKeyCode": k.vk, "nativeVirtualKeyCode": k.vk}
	if k.text != "" {
		down["type"], down["text"], down["unmodifiedText"] = "keyDown", k.text, k.text
	}
	if err := b.c.call(ctx, t.session, "Input.dispatchKeyEvent", down, nil); err != nil {
		return err
	}
	return b.c.call(ctx, t.session, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": k.key, "code": k.code,
		"modifiers": mods, "windowsVirtualKeyCode": k.vk, "nativeVirtualKeyCode": k.vk}, nil)
}

func (b *browser) press(ctx context.Context, t *tab, key string, ref int, acceptDialogs bool) (string, error) {
	if _, _, err := parseKey(key); err != nil {
		return "", err
	}
	if ref != 0 {
		info, err := b.node(t, ref)
		if err != nil {
			return "", err
		}
		if err := b.c.call(ctx, t.session, "DOM.focus", map[string]any{"backendNodeId": info.node}, nil); err != nil {
			return "", staleErr(err)
		}
	}
	b.mu.Lock()
	t.acceptDialogs = acceptDialogs
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		t.acceptDialogs = false
		b.mu.Unlock()
	}()
	note, err := b.settle(ctx, t, func() error { return b.key(ctx, t, key) })
	if err != nil {
		return "", err
	}
	return joinLines("Pressed "+key+".", note, b.status(ctx, t)), nil
}

type layoutMetrics struct {
	CSSLayoutViewport struct {
		PageX        float64 `json:"pageX"`
		PageY        float64 `json:"pageY"`
		ClientWidth  float64 `json:"clientWidth"`
		ClientHeight float64 `json:"clientHeight"`
	} `json:"cssLayoutViewport"`
	CSSContentSize struct {
		Width  float64 `json:"width"`
		Height float64 `json:"height"`
	} `json:"cssContentSize"`
}

func (b *browser) metrics(ctx context.Context, t *tab) (layoutMetrics, error) {
	var m layoutMetrics
	err := b.c.call(ctx, t.session, "Page.getLayoutMetrics", nil, &m)
	return m, err
}

func (b *browser) scroll(ctx context.Context, t *tab, direction string, amount, ref int) (string, error) {
	if ref != 0 {
		info, err := b.node(t, ref)
		if err != nil {
			return "", err
		}
		if err := b.c.call(ctx, t.session, "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": info.node}, nil); err != nil {
			return "", staleErr(err)
		}
		return fmt.Sprintf("Scrolled %s%s [ref=%d] into view.", info.role, nameAttr(info.name), ref), nil
	}
	m, err := b.metrics(ctx, t)
	if err != nil {
		return "", err
	}
	vw, vh := m.CSSLayoutViewport.ClientWidth, m.CSSLayoutViewport.ClientHeight
	dx, dy := 0.0, 0.0
	switch direction {
	case "", "down":
		dy = 1
	case "up":
		dy = -1
	case "right":
		dx = 1
	case "left":
		dx = -1
	default:
		return "", fmt.Errorf("direction %q must be up, down, left or right", direction)
	}
	step := float64(amount)
	if step <= 0 {
		step = 0.8 * max(vh*abs(dy), vw*abs(dx))
	}
	if err := b.c.call(ctx, t.session, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseWheel", "x": vw / 2, "y": vh / 2, "deltaX": dx * step, "deltaY": dy * step,
	}, nil); err != nil {
		return "", err
	}
	time.Sleep(150 * time.Millisecond) // let the scroll apply before measuring
	var pos struct {
		X, Y, H, Total float64
	}
	raw, err := b.eval(ctx, t, `({X: scrollX, Y: scrollY, H: innerHeight, Total: document.documentElement.scrollHeight})`)
	if err == nil && json.Unmarshal(raw, &pos) == nil && pos.Total > 0 {
		pct := min(100, 100*(pos.Y+pos.H)/pos.Total)
		return fmt.Sprintf("Scrolled %s. The view now ends %.0f%% of the way down the page (y=%.0f of %.0f).", cmpOr(direction, "down"), pct, pos.Y, pos.Total), nil
	}
	return "Scrolled " + cmpOr(direction, "down") + ".", nil
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// eval runs an expression in the page and returns its value as JSON.
func (b *browser) eval(ctx context.Context, t *tab, expr string) (json.RawMessage, error) {
	var res evalResult
	if err := b.c.call(ctx, t.session, "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true, "awaitPromise": true, "userGesture": true,
	}, &res); err != nil {
		return nil, err
	}
	if res.ExceptionDetails != nil {
		return nil, errors.New(res.message())
	}
	switch {
	case res.Result.UnserializableValue != "":
		return json.RawMessage(fmt.Sprintf("%q", res.Result.UnserializableValue)), nil
	case res.Result.Type == "undefined":
		return json.RawMessage("undefined"), nil
	case len(res.Result.Value) == 0 && res.Result.Description != "":
		return json.Marshal(res.Result.Description)
	}
	return res.Result.Value, nil
}

func (b *browser) back(ctx context.Context, t *tab) (string, error) {
	var h struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			ID int `json:"id"`
		} `json:"entries"`
	}
	if err := b.c.call(ctx, t.session, "Page.getNavigationHistory", nil, &h); err != nil {
		return "", err
	}
	if h.CurrentIndex <= 0 || h.CurrentIndex >= len(h.Entries) {
		return "", errors.New("there is no earlier page in this tab")
	}
	w := b.c.wait(loaded(t))
	defer w.cancel()
	if err := b.c.call(ctx, t.session, "Page.navigateToHistoryEntry", map[string]any{"entryId": h.Entries[h.CurrentIndex-1].ID}, nil); err != nil {
		return "", err
	}
	wctx, cancel := shortly(ctx, 30*time.Second)
	_, werr := w.next(wctx, b.c)
	cancel()
	note := ""
	if werr != nil && !errors.Is(werr, errClosed) {
		note = "(the page is still loading)"
	}
	return joinLines("Went back.", note, b.status(ctx, t)), nil
}

// waitFor waits for text to appear on (or, with gone, leave) the page, or
// for a fixed time.
func (b *browser) waitFor(ctx context.Context, t *tab, text string, gone bool, ms int) (string, error) {
	if text == "" {
		if ms <= 0 {
			return "", errors.New("give text to wait for, or ms")
		}
		d := time.Duration(ms) * time.Millisecond
		wctx, cancel := shortly(ctx, d)
		defer cancel()
		<-wctx.Done()
		return fmt.Sprintf("Waited %s.", d), nil
	}
	q, _ := json.Marshal(text)
	expr := fmt.Sprintf(`!!document.body && document.body.innerText.includes(%s)`, q)
	limit := 30 * time.Second
	if ms > 0 {
		limit = time.Duration(ms) * time.Millisecond
	}
	wctx, cancel := shortly(ctx, limit)
	defer cancel()
	start := time.Now()
	for {
		raw, err := b.eval(wctx, t, expr)
		if err == nil && (string(raw) == "true") != gone {
			verb := "appeared"
			if gone {
				verb = "is gone"
			}
			return fmt.Sprintf("The text %s after %s.\n%s", verb, time.Since(start).Round(100*time.Millisecond), b.status(ctx, t)), nil
		}
		select {
		case <-wctx.Done():
			verb := "did not appear"
			if gone {
				verb = "is still there"
			}
			return "", fmt.Errorf("the text %s after %s", verb, time.Since(start).Round(100*time.Millisecond))
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Screenshots larger than this are sent to the model as JPEG, and full
// pages are cut at maxShotHeight (model APIs refuse taller images).
const (
	maxModelImage = 5 << 20
	maxShotHeight = 8000
)

// screenshot captures the viewport, the full page, or one element, as PNG
// (format "png") or JPEG (format "jpeg").
func (b *browser) screenshot(ctx context.Context, t *tab, fullPage bool, ref int, format string) ([]byte, string, error) {
	if b.connect {
		// A background tab of a visible browser does not paint.
		_ = b.c.call(ctx, "", "Target.activateTarget", map[string]any{"targetId": t.target}, nil)
	}
	params := map[string]any{"format": format}
	if format == "jpeg" {
		params["quality"] = 80
	}
	note := ""
	switch {
	case ref != 0:
		info, err := b.node(t, ref)
		if err != nil {
			return nil, "", err
		}
		_ = b.c.call(ctx, t.session, "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": info.node}, nil)
		var q struct {
			Quads [][]float64 `json:"quads"`
		}
		if err := b.c.call(ctx, t.session, "DOM.getContentQuads", map[string]any{"backendNodeId": info.node}, &q); err != nil || len(q.Quads) == 0 || len(q.Quads[0]) != 8 {
			return nil, "", errors.New("the element is not visible; take a new browser_snapshot")
		}
		m, err := b.metrics(ctx, t)
		if err != nil {
			return nil, "", err
		}
		x0, y0, x1, y1 := bounds(q.Quads[0])
		params["clip"] = map[string]any{"x": x0 + m.CSSLayoutViewport.PageX, "y": y0 + m.CSSLayoutViewport.PageY,
			"width": max(1, x1-x0), "height": max(1, min(y1-y0, maxShotHeight)), "scale": 1}
		params["captureBeyondViewport"] = true
	case fullPage:
		m, err := b.metrics(ctx, t)
		if err != nil {
			return nil, "", err
		}
		h := m.CSSContentSize.Height
		if h > maxShotHeight {
			note = fmt.Sprintf("(the page is %.0f pixels tall; the screenshot shows the top %d)", h, maxShotHeight)
			h = maxShotHeight
		}
		params["clip"] = map[string]any{"x": 0, "y": 0, "width": m.CSSContentSize.Width, "height": h, "scale": 1}
		params["captureBeyondViewport"] = true
	}
	var res struct {
		Data string `json:"data"`
	}
	if err := b.c.call(ctx, t.session, "Page.captureScreenshot", params, &res); err != nil {
		return nil, "", err
	}
	data, err := base64.StdEncoding.DecodeString(res.Data)
	return data, note, err
}

func bounds(q []float64) (x0, y0, x1, y1 float64) {
	x0, y0, x1, y1 = q[0], q[1], q[0], q[1]
	for i := 2; i < 8; i += 2 {
		x0, x1 = min(x0, q[i]), max(x1, q[i])
		y0, y1 = min(y0, q[i+1]), max(y1, q[i+1])
	}
	return
}

func (b *browser) pdf(ctx context.Context, t *tab) ([]byte, error) {
	var res struct {
		Data string `json:"data"`
	}
	if err := b.c.call(ctx, t.session, "Page.printToPDF", map[string]any{"printBackground": true, "preferCSSPageSize": true}, &res); err != nil {
		if b.connect || !*b.m.cfg.Headless {
			return nil, fmt.Errorf("%w (printing to PDF needs a headless browser)", err)
		}
		return nil, err
	}
	return base64.StdEncoding.DecodeString(res.Data)
}

var unsafeName = regexp.MustCompile(`[^\w.\- ()\[\]+,=@]+`)

// download saves a file into dir: the one url points at (loaded in a
// temporary tab) or the one clicking ref starts.
func (b *browser) download(ctx context.Context, t *tab, url string, ref int, dir string) (string, int64, error) {
	if url != "" {
		if err := b.m.guard.Check(ctx, url, true); err != nil {
			return "", 0, err
		}
	}
	var info refInfo
	if url == "" {
		var err error
		if info, err = b.node(t, ref); err != nil {
			return "", 0, err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, err
	}
	tmp, err := os.MkdirTemp(dir, ".download-")
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	events := b.c.wait(func(m message) bool {
		return m.SessionID == "" && (m.Method == "Browser.downloadWillBegin" || m.Method == "Browser.downloadProgress")
	})
	defer events.cancel()
	if err := b.c.call(ctx, "", "Browser.setDownloadBehavior", map[string]any{
		"behavior": "allowAndName", "downloadPath": tmp, "eventsEnabled": true,
	}, nil); err != nil {
		return "", 0, err
	}
	defer func() {
		_ = b.c.call(context.WithoutCancel(ctx), "", "Browser.setDownloadBehavior", map[string]any{"behavior": "default"}, nil)
	}()

	if url != "" {
		var res struct {
			TargetID string `json:"targetId"`
		}
		if err := b.c.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank", "background": true}, &res); err != nil {
			return "", 0, err
		}
		defer func() {
			_ = b.c.call(context.WithoutCancel(ctx), "", "Target.closeTarget", map[string]any{"targetId": res.TargetID}, nil)
		}()
		dt, err := b.attach(ctx, res.TargetID, true)
		if err != nil {
			return "", 0, err
		}
		if err := b.c.call(ctx, dt.session, "Page.navigate", map[string]any{"url": url}, nil); err != nil {
			return "", 0, err
		}
	} else if err := b.clickNode(ctx, t, info.node); err != nil {
		return "", 0, err
	}

	var guid, name string
	begin, cancel := shortly(ctx, 15*time.Second)
	defer cancel()
	for {
		wctx := ctx
		if guid == "" {
			wctx = begin
		}
		m, err := events.next(wctx, b.c)
		if err != nil {
			if guid == "" {
				return "", 0, errors.New("no download started; the link may open a page instead (use browser_navigate)")
			}
			return "", 0, fmt.Errorf("the download did not finish: %w", err)
		}
		var p struct {
			GUID              string  `json:"guid"`
			SuggestedFilename string  `json:"suggestedFilename"`
			State             string  `json:"state"`
			ReceivedBytes     float64 `json:"receivedBytes"`
		}
		if json.Unmarshal(m.Params, &p) != nil {
			continue
		}
		if m.Method == "Browser.downloadWillBegin" {
			if guid == "" {
				guid, name = p.GUID, p.SuggestedFilename
			}
			continue
		}
		if p.GUID != guid {
			continue
		}
		switch p.State {
		case "canceled":
			return "", 0, errors.New("the download was canceled (blocked by the URL guard, or by the site)")
		case "completed":
			dest := uniquePath(filepath.Join(dir, safeName(name)))
			if err := os.Rename(filepath.Join(tmp, guid), dest); err != nil {
				return "", 0, err
			}
			st, err := os.Stat(dest)
			if err != nil {
				return "", 0, err
			}
			return dest, st.Size(), nil
		}
	}
}

func safeName(s string) string {
	// Only "/" separates folders here, on every platform, so a name
	// means the same thing wherever it came from.
	s = path.Base(strings.TrimSpace(s))
	s = unsafeName.ReplaceAllString(s, "_")
	switch {
	case s == "" || s == "." || s == "..":
		s = "download"
	case strings.HasPrefix(s, "."):
		s = "download" + s
	}
	if len(s) > 120 {
		ext := filepath.Ext(s)
		if len(ext) > 20 {
			ext = ""
		}
		s = s[:120-len(ext)] + ext
	}
	return s
}

// uniquePath adds -2, -3, ... before the extension while p exists.
func uniquePath(p string) string {
	if _, err := os.Lstat(p); err != nil {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 2; ; i++ {
		q := fmt.Sprintf("%s-%d%s", base, i, ext)
		if _, err := os.Lstat(q); err != nil {
			return q
		}
	}
}

func joinLines(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n")
}
