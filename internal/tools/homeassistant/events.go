package homeassistant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/coder/websocket"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// Event is a Home Assistant event as the WebSocket API sends it.
type Event struct {
	EventType string                     `json:"event_type"`
	Data      map[string]json.RawMessage `json:"data"`
	TimeFired time.Time                  `json:"time_fired"`
	Context   struct {
		ID string `json:"id"`
	} `json:"context"`
}

// Fire starts a trigger's turn. turnID is derived from the event, so an
// event seen twice (a reconnect replaying it) runs once.
type Fire func(ctx context.Context, t *config.HATrigger, turnID, text string) error

// Listener follows Home Assistant's event bus over the WebSocket API and
// fires triggers whose filters match. It reconnects with backoff.
type Listener struct {
	URL      string // the REST base URL; the socket is at /api/websocket
	Token    string
	Triggers []*config.HATrigger
	Fire     Fire
	Location *time.Location
	Log      *slog.Logger
	HTTP     *http.Client

	mu      sync.Mutex
	last    map[string]time.Time // trigger -> last fire, for cooldowns
	prompts map[string]*template.Template
}

func wsURL(base string) string {
	base = strings.TrimRight(base, "/")
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	}
	return base + "/api/websocket"
}

// Run listens until ctx ends.
func (l *Listener) Run(ctx context.Context) {
	if l.Log == nil {
		l.Log = slog.New(slog.DiscardHandler)
	}
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := l.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		l.Log.Warn("home assistant events disconnected", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

type wsMessage struct {
	ID      int             `json:"id"`
	Type    string          `json:"type"`
	Success *bool           `json:"success"`
	Message string          `json:"message"`
	Error   json.RawMessage `json:"error"`
	Event   *Event          `json:"event"`
}

// session is one connection: authenticate, subscribe to each event type
// the triggers need, then read events.
func (l *Listener) session(ctx context.Context) error {
	if l.Token == "" {
		return errors.New("the Home Assistant token is not set")
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, wsURL(l.URL), &websocket.DialOptions{HTTPClient: l.HTTP})
	if err != nil {
		return err
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(16 << 20)
	read := func(ctx context.Context) (wsMessage, error) {
		var m wsMessage
		_, data, err := conn.Read(ctx)
		if err != nil {
			return m, err
		}
		return m, json.Unmarshal(data, &m)
	}
	write := func(v any) error {
		data, _ := json.Marshal(v)
		return conn.Write(ctx, websocket.MessageText, data)
	}
	m, err := read(dctx)
	if err != nil {
		return err
	}
	if m.Type != "auth_required" {
		return fmt.Errorf("unexpected first message %q", m.Type)
	}
	if err := write(map[string]string{"type": "auth", "access_token": l.Token}); err != nil {
		return err
	}
	if m, err = read(dctx); err != nil {
		return err
	}
	if m.Type != "auth_ok" {
		return fmt.Errorf("home assistant refused the token: %s %s", m.Type, m.Message)
	}
	var types []string
	for _, t := range l.Triggers {
		if !slices.Contains(types, t.EventType) {
			types = append(types, t.EventType)
		}
	}
	slices.Sort(types)
	for i, et := range types {
		if err := write(map[string]any{"id": i + 1, "type": "subscribe_events", "event_type": et}); err != nil {
			return err
		}
	}
	l.Log.Info("home assistant events connected", "event_types", types)
	for {
		m, err := read(ctx)
		if err != nil {
			return err
		}
		switch {
		case m.Type == "result" && m.Success != nil && !*m.Success:
			return fmt.Errorf("subscribe failed: %s", m.Error)
		case m.Type == "event" && m.Event != nil:
			l.dispatch(ctx, m.Event)
		}
	}
}

// dispatch fires every trigger the event matches.
func (l *Listener) dispatch(ctx context.Context, e *Event) {
	for _, t := range l.Triggers {
		if !Match(t, e) || !l.cool(t) {
			continue
		}
		text, err := l.render(t, e)
		if err != nil {
			l.Log.Warn("home assistant trigger prompt failed", "trigger", t.Name, "err", err)
			continue
		}
		key := e.Context.ID
		if key == "" {
			key = e.TimeFired.String()
		}
		sum := sha256.Sum256([]byte("ha\x00" + t.Name + "\x00" + key))
		id := "turn_" + hex.EncodeToString(sum[:12])
		if err := l.Fire(ctx, t, id, text); err != nil {
			l.Log.Error("home assistant trigger failed", "trigger", t.Name, "err", err)
			continue
		}
		l.Log.Info("home assistant trigger fired", "trigger", t.Name, "event", e.EventType, "turn", id)
	}
}

// cool reports whether the trigger is out of its cooldown, and starts a
// new one when it is.
func (l *Listener) cool(t *config.HATrigger) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	now := time.Now()
	if last, ok := l.last[t.Name]; ok && now.Sub(last) < t.Cooldown {
		return false
	}
	l.last[t.Name] = now
	return true
}

// stateOf reads new_state or old_state of a state_changed event.
func stateOf(e *Event, key string) *State {
	raw, ok := e.Data[key]
	if !ok || string(raw) == "null" {
		return nil
	}
	var s State
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return &s
}

func dataString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// Match reports whether an event fires a trigger. A state_changed trigger
// fires only when the state itself changed, not just an attribute.
func Match(t *config.HATrigger, e *Event) bool {
	if e.EventType != t.EventType {
		return false
	}
	if t.EntityID != "" {
		id := dataString(e.Data["entity_id"])
		if ok, _ := path.Match(t.EntityID, id); !ok {
			return false
		}
	}
	for k, want := range t.Data {
		raw, ok := e.Data[k]
		if !ok || dataString(raw) != want {
			return false
		}
	}
	if e.EventType == "state_changed" {
		cur, old := stateOf(e, "new_state"), stateOf(e, "old_state")
		if cur == nil || (old != nil && old.State == cur.State) {
			return false
		}
		if t.To != "" && cur.State != t.To {
			return false
		}
		if t.From != "" && (old == nil || old.State != t.From) {
			return false
		}
	}
	return true
}

const defaultPrompt = `Home Assistant event {{.EventType}}{{if .EntityID}} for {{.Name}} ({{.EntityID}}){{end}}` +
	`{{if .To}}: {{if .From}}{{.From}} -> {{end}}{{.To}}{{end}} at {{.Time}}.{{if not .To}} Data: {{.DataJSON}}{{end}}`

func (l *Listener) render(t *config.HATrigger, e *Event) (string, error) {
	l.mu.Lock()
	if l.prompts == nil {
		l.prompts = map[string]*template.Template{}
	}
	tmpl := l.prompts[t.Name]
	if tmpl == nil {
		src := t.Prompt
		if src == "" {
			src = defaultPrompt
		}
		var err error
		if tmpl, err = template.New(t.Name).Option("missingkey=zero").Parse(src); err != nil {
			l.mu.Unlock()
			return "", err
		}
		l.prompts[t.Name] = tmpl
	}
	l.mu.Unlock()
	loc := l.Location
	if loc == nil {
		loc = time.Local
	}
	data := map[string]any{}
	for k, v := range e.Data {
		var x any
		_ = json.Unmarshal(v, &x)
		data[k] = x
	}
	all, _ := json.Marshal(e.Data)
	fired := e.TimeFired
	if fired.IsZero() {
		fired = time.Now()
	}
	view := map[string]any{"EventType": e.EventType, "EntityID": dataString(e.Data["entity_id"]), "Data": data,
		"DataJSON": string(all), "Time": fired.In(loc).Format("2006-01-02 15:04:05 MST"), "From": "", "To": "", "Name": ""}
	if cur := stateOf(e, "new_state"); cur != nil {
		view["To"], view["Name"] = cur.State, cur.Name()
	}
	if old := stateOf(e, "old_state"); old != nil {
		view["From"] = old.State
	}
	if view["Name"] == "" {
		view["Name"] = view["EntityID"]
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, view); err != nil {
		return "", err
	}
	return b.String(), nil
}
