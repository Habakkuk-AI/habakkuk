package meeting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

// Page is the browser tab an attendance drives. *browser.Dedicated
// implements it; tests use a fake.
type Page interface {
	Navigate(ctx context.Context, url string) error
	Eval(ctx context.Context, expr string) (json.RawMessage, error)
	InsertText(ctx context.Context, text string) error
	Click(ctx context.Context, x, y float64) error
	Press(ctx context.Context, key string) error
	// Bind adds a page function that hands its string argument to fn.
	// fn must return quickly.
	Bind(ctx context.Context, name string, fn func(payload string)) error
	Done() <-chan struct{}
	Close()
}

// Launcher starts a browser for an attendance, keeping its files in dir.
type Launcher func(ctx context.Context, rec *Record, dir string) (Page, error)

// errBrowserGone means the browser died mid-meeting; the attendance joins
// again.
var errBrowserGone = errors.New("the meeting browser stopped")

// Limits of the join flow.
const (
	joinTimeout  = 3 * time.Minute // stuck on one page (other than the lobby) this long
	evalTimeout  = 15 * time.Second
	captionRetry = 2 * time.Minute // pause after failing to turn captions on
	maxPending   = 10000           // caption batches waiting to be read
)

type attendance struct {
	m    *Manager
	rec  *Record
	page Page
	sp   *spec
	asm  *Assembler

	mu      sync.Mutex
	pending []string
	dirty   bool // the record changed since it was saved
}

// attend runs one browser through the meeting: join, stay, leave. It
// returns the final status and why, or an error: errBrowserGone (join
// again) or the context's (shutting down; the job resumes later).
func (m *Manager) attend(ctx context.Context, rec *Record) (Status, string, error) {
	dir := m.Store.BrowserDir(rec.ID)
	// The profile holds the session's cookies and caches; it goes when the
	// browser does.
	defer func() { _ = os.RemoveAll(dir) }()
	page, err := m.Launch(ctx, rec, dir)
	if err != nil {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		return StatusFailed, "could not start a browser: " + err.Error(), nil
	}
	defer page.Close()
	known, err := m.Store.Lines(rec.ID)
	if err != nil {
		return StatusFailed, "reading the transcript: " + err.Error(), nil
	}
	a := &attendance{m: m, rec: rec, page: page, sp: specFor(rec.Platform), asm: NewAssembler(m.Settle, known)}
	if err := page.Bind(ctx, bindingName, a.onBatch); err != nil {
		return a.broken(ctx, err)
	}
	m.setStatus(rec, StatusJoining, "opening the "+rec.Platform.Title()+" page")
	if err := page.Navigate(ctx, rec.JoinURL); err != nil {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		return StatusFailed, "could not open the meeting page: " + err.Error(), nil
	}
	st, reason, err := a.join(ctx)
	if err != nil || st != StatusInCall {
		return st, reason, err
	}
	return a.stay(ctx)
}

// broken turns an error of the page into the attendance's outcome.
func (a *attendance) broken(ctx context.Context, err error) (Status, string, error) {
	if ctx.Err() != nil {
		return "", "", ctx.Err()
	}
	if a.gone() {
		return "", "", errBrowserGone
	}
	return StatusFailed, err.Error(), nil
}

func (a *attendance) gone() bool {
	select {
	case <-a.page.Done():
		return true
	default:
		return false
	}
}

// onBatch runs on the browser connection's read loop: it only queues.
func (a *attendance) onBatch(payload string) {
	a.mu.Lock()
	if len(a.pending) < maxPending {
		a.pending = append(a.pending, payload)
	}
	a.mu.Unlock()
}

func (a *attendance) eval(ctx context.Context, op string, arg any) (json.RawMessage, error) {
	ectx, cancel := context.WithTimeout(ctx, evalTimeout)
	defer cancel()
	return a.page.Eval(ectx, pageCall(op, arg))
}

func (a *attendance) probe(ctx context.Context) (Probe, error) {
	raw, err := a.eval(ctx, "probe", map[string]any{"skip": a.sp.skipSelectors(), "captions": a.sp.captions})
	if err != nil {
		return Probe{}, err
	}
	var p Probe
	if err := json.Unmarshal(raw, &p); err != nil {
		return Probe{}, fmt.Errorf("reading the page: %w", err)
	}
	return p, nil
}

// click presses the first visible button whose label matches pattern.
func (a *attendance) click(ctx context.Context, pattern string) (string, bool, error) {
	raw, err := a.eval(ctx, "find", map[string]string{"pattern": pattern})
	if err != nil || string(raw) == "null" || len(raw) == 0 {
		return "", false, err
	}
	var r struct {
		X, Y  float64
		Label string
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", false, err
	}
	ectx, cancel := context.WithTimeout(ctx, evalTimeout)
	defer cancel()
	if err := a.page.Click(ectx, r.X, r.Y); err != nil {
		return "", false, err
	}
	return r.Label, true, nil
}

// fill types text into the first visible field whose label matches
// pattern, replacing what is there.
func (a *attendance) fill(ctx context.Context, pattern, text string) (bool, error) {
	raw, err := a.eval(ctx, "focus", map[string]string{"pattern": pattern})
	if err != nil || string(raw) == "null" || len(raw) == 0 {
		return false, err
	}
	var r struct{ Value string }
	if err := json.Unmarshal(raw, &r); err != nil {
		return false, err
	}
	if r.Value == text {
		return true, nil
	}
	ectx, cancel := context.WithTimeout(ctx, evalTimeout)
	defer cancel()
	return true, a.page.InsertText(ectx, text)
}

// wait sleeps one tick, or less when the context ends or the browser dies.
func (a *attendance) wait(ctx context.Context) error {
	t := time.NewTimer(a.m.tick())
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.page.Done():
		return errBrowserGone
	case <-t.C:
		return nil
	}
}

// join gets from the meeting page into the call.
func (a *attendance) join(ctx context.Context) (Status, string, error) {
	m, rec := a.m, a.rec
	var lobbySince time.Time
	filled, muted := false, map[string]bool{}
	fails := 0
	lastState, since := StateUnknown, m.now()
	for {
		if m.Store.LeaveRequested(rec.ID) {
			if !rec.JoinedAt.IsZero() {
				return StatusEnded, "asked to leave", nil
			}
			return StatusEnded, "asked to leave before being let in", nil
		}
		pr, err := a.probe(ctx)
		now := m.now()
		switch {
		case err != nil:
			if ctx.Err() != nil || a.gone() {
				return a.broken(ctx, err)
			}
			if fails++; fails > 20 {
				return StatusFailed, "the meeting page stopped answering: " + err.Error(), nil
			}
		default:
			fails = 0
			st := Classify(rec.Platform, pr)
			if st != lastState {
				lastState, since = st, now
			}
			switch st {
			case StateInCall:
				if rec.JoinedAt.IsZero() {
					rec.JoinedAt = now
				}
				m.setStatus(rec, StatusInCall, "in the call")
				return StatusInCall, "", nil
			case StateWaiting:
				if lobbySince.IsZero() {
					lobbySince = now
					m.setStatus(rec, StatusWaiting, "waiting to be let in")
				}
				if now.Sub(lobbySince) > m.Cfg.LobbyTimeout {
					return StatusFailed, fmt.Sprintf("nobody let Habakkuk in within %s", m.Cfg.LobbyTimeout), nil
				}
			case StateDenied, StateInvalid:
				return StatusFailed, Reason(rec.Platform, st, pr), nil
			case StateEnded:
				if !rec.JoinedAt.IsZero() {
					return StatusEnded, Reason(rec.Platform, st, pr), nil
				}
				return StatusFailed, Reason(rec.Platform, st, pr), nil
			default:
				a.joinStep(ctx, pr, &filled, muted)
			}
			if st != StateWaiting && now.Sub(since) > joinTimeout {
				return StatusFailed, fmt.Sprintf("could not get into the meeting: stuck on a page Habakkuk does not recognize (%q, %s)",
					clip(pr.Title, 80), pr.URL), nil
			}
		}
		if err := a.wait(ctx); err != nil {
			return a.broken(ctx, err)
		}
	}
}

func (a *attendance) joinStep(ctx context.Context, pr Probe, filled *bool, muted map[string]bool) {
	step := NextJoinStep(a.rec.Platform, pr, *filled, muted)
	switch step.Kind {
	case "fill":
		ok, err := a.fill(ctx, step.Pattern, a.rec.Name)
		*filled = ok && err == nil
	case "click":
		label, ok, _ := a.click(ctx, step.Pattern)
		if ok && re(a.sp.mute).MatchString(clean(label)) {
			muted[clean(label)] = true
		}
	}
}

// stay keeps Habakkuk in the call, reading captions, until the call ends,
// the time limit passes, everyone else leaves, or someone asks it to go.
func (a *attendance) stay(ctx context.Context) (Status, string, error) {
	m, rec := a.m, a.rec
	deadline := rec.JoinedAt.Add(rec.Limit())
	a.afterJoin(ctx)
	var aloneSince, captionsNext time.Time
	away, fails, captionTries, n := 0, 0, 0, 0
	lastSave := m.now()
	for {
		if err := a.wait(ctx); err != nil {
			a.finish()
			return a.broken(ctx, err)
		}
		now := m.now()
		a.drain(now)
		if now.Sub(lastSave) >= 15*time.Second {
			a.save()
			lastSave = now
		}
		if m.Store.LeaveRequested(rec.ID) {
			return a.leave(ctx, "asked to leave")
		}
		if !now.Before(deadline) {
			return a.leave(ctx, "reached the time limit of "+rec.Limit().String())
		}
		if n++; n%3 != 0 {
			continue
		}
		pr, err := a.probe(ctx)
		if err != nil {
			if fails++; fails >= 10 {
				a.finish()
				return "", "", errBrowserGone
			}
			continue
		}
		fails = 0
		st := Classify(rec.Platform, pr)
		switch st {
		case StateInCall, StateWaiting:
			away = 0
		default:
			limit := 2
			if st == StateUnknown || st == StatePreJoin || st == StateInterstitial {
				limit = 4
			}
			if away++; away >= limit {
				a.finish()
				reason := Reason(rec.Platform, st, pr)
				if st == StateUnknown || st == StatePreJoin || st == StateInterstitial {
					reason = "the call page went away"
				}
				return StatusEnded, reason, nil
			}
			continue
		}
		if Alone(rec.Platform, pr) {
			if aloneSince.IsZero() {
				aloneSince = now
			} else if now.Sub(aloneSince) >= m.Cfg.AloneTimeout {
				return a.leave(ctx, "everyone else left")
			}
		} else {
			aloneSince = time.Time{}
		}
		if st != StateInCall {
			continue
		}
		if CaptionsOn(rec.Platform, pr) {
			if !rec.Captions {
				rec.Captions, a.dirty = true, true
				m.setDetail(rec, "in the call, reading captions")
			}
			_, _ = a.eval(ctx, "captions", map[string]any{"captions": a.sp.captions, "binding": bindingName})
		} else if !now.Before(captionsNext) {
			a.captionsStep(ctx, pr, captionTries == 0)
			if captionTries++; captionTries >= 10 {
				captionTries, captionsNext = 0, now.Add(captionRetry)
				m.setDetail(rec, "in the call; captions could not be turned on yet")
			}
		}
		if hasButton(pr, dismissInCall) {
			_, _, _ = a.click(ctx, dismissInCall)
		}
	}
}

// afterJoin turns off a microphone or camera still on, and says Habakkuk is
// there in the meeting chat.
func (a *attendance) afterJoin(ctx context.Context) {
	if pr, err := a.probe(ctx); err == nil {
		r := re(a.sp.mute)
		for _, b := range pr.Buttons {
			if r.MatchString(clean(b)) {
				_, _, _ = a.click(ctx, "^"+regexpQuote(clean(b))+"$")
			}
		}
	}
	if *a.m.Cfg.Announce && !a.rec.Announced {
		a.announce(ctx)
	}
}

// announce posts "<name> has joined" in the meeting chat, opening the chat
// panel when its field is not showing. Platforms that give guests no chat
// are skipped after a few tries.
func (a *attendance) announce(ctx context.Context) {
	msg := a.rec.Name + " has joined to take notes."
	for range 3 {
		ok, err := a.fill(ctx, a.sp.chatField, msg)
		if ok && err == nil {
			if a.page.Press(ctx, "Enter") == nil {
				a.rec.Announced, a.dirty = true, true
				a.save()
			}
			return
		}
		if _, _, err := a.click(ctx, a.sp.chatOpen); err != nil && (ctx.Err() != nil || a.gone()) {
			return
		}
		if a.wait(ctx) != nil {
			return
		}
	}
}

// captionsStep makes one move toward turning captions on: the deepest
// step showing (a menu item before the menu), else the keyboard shortcut
// once per round, which on some platforms toggles.
func (a *attendance) captionsStep(ctx context.Context, pr Probe, first bool) {
	for _, step := range a.sp.captionSteps {
		if hasButton(pr, step) {
			_, _, _ = a.click(ctx, step)
			return
		}
	}
	if first && a.sp.captionKey != "" {
		_ = a.page.Press(ctx, a.sp.captionKey)
	}
}

// leave presses the leave button (and confirms), best effort: the browser
// closes right after anyway.
func (a *attendance) leave(ctx context.Context, reason string) (Status, string, error) {
	a.finish()
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, ok, _ := a.click(lctx, a.sp.leave); ok && a.sp.leaveConfirm != "" {
		t := time.NewTimer(min(a.m.tick(), time.Second))
		select {
		case <-t.C:
		case <-lctx.Done():
		}
		t.Stop()
		_, _, _ = a.click(lctx, a.sp.leaveConfirm)
	}
	return StatusEnded, reason, nil
}

// finish writes every caption received so far, open blocks included.
func (a *attendance) finish() {
	a.drain(a.m.now())
	a.write(a.asm.Close())
	a.save()
}

// drain feeds the caption batches the page sent into the assembler and
// writes the lines that finished.
func (a *attendance) drain(now time.Time) {
	a.mu.Lock()
	batches := a.pending
	a.pending = nil
	a.mu.Unlock()
	var out []Line
	for _, raw := range batches {
		var b captionBatch
		if json.Unmarshal([]byte(raw), &b) != nil {
			continue
		}
		for _, c := range b.Changes {
			key := b.Epoch + ":" + strconv.Itoa(c.ID)
			if c.Gone {
				out = append(out, a.asm.Gone(key)...)
			} else {
				out = append(out, a.asm.Update(key, c.Speaker, c.Text, now)...)
			}
		}
	}
	a.write(append(out, a.asm.Flush(now)...))
}

func (a *attendance) write(lines []Line) {
	if len(lines) == 0 {
		return
	}
	if err := a.m.Store.AppendLines(a.rec.ID, lines); err != nil {
		a.m.log().Error("writing the meeting transcript failed", "meeting", a.rec.ID, "err", err)
		return
	}
	a.rec.Lines += len(lines)
	a.rec.LastLine = lines[len(lines)-1].Time
	a.dirty = true
}

func (a *attendance) save() {
	if !a.dirty {
		return
	}
	if err := a.m.Store.Save(a.rec); err != nil {
		a.m.log().Warn("saving the meeting record failed", "meeting", a.rec.ID, "err", err)
		return
	}
	a.dirty = false
}

func clip(s string, n int) string {
	s = clean(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
