package homeassistant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

const token = "ha-token"

type fakeHA struct {
	t     *testing.T
	mu    sync.Mutex
	calls []string // "domain.service body"
	// events are sent to each WebSocket client after it subscribes.
	events []map[string]any
	subs   []string
}

func (f *fakeHA) handler() http.Handler {
	mux := http.NewServeMux()
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "401: Unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /api/states", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []map[string]any{
			{"entity_id": "light.kitchen", "state": "on", "attributes": map[string]any{"friendly_name": "Kitchen", "brightness": 200}},
			{"entity_id": "light.porch", "state": "off", "attributes": map[string]any{"friendly_name": "Porch light"}},
			{"entity_id": "lock.front_door", "state": "locked", "attributes": map[string]any{"friendly_name": "Front door"}},
			{"entity_id": "sensor.outside_temperature", "state": "4.5",
				"attributes": map[string]any{"friendly_name": "Outside", "unit_of_measurement": "°C"}},
		})
	}))
	mux.HandleFunc("POST /api/services/{domain}/{service}", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		b, _ := json.Marshal(body)
		f.mu.Lock()
		f.calls = append(f.calls, r.PathValue("domain")+"."+r.PathValue("service")+" "+string(b))
		f.mu.Unlock()
		ids, _ := body["entity_id"].([]any)
		var out []map[string]any
		for _, id := range ids {
			out = append(out, map[string]any{"entity_id": id, "state": "on", "attributes": map[string]any{}})
		}
		writeJSON(w, out)
	}))
	mux.HandleFunc("GET /api/history/period/{start}", auth(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if _, err := time.Parse(time.RFC3339, r.PathValue("start")); err != nil || q.Get("filter_entity_id") != "sensor.outside_temperature" ||
			!q.Has("minimal_response") || q.Get("end_time") == "" {
			f.t.Errorf("history query %s %v", r.PathValue("start"), q)
		}
		writeJSON(w, [][]map[string]any{{
			{"entity_id": "sensor.outside_temperature", "state": "3.0", "last_changed": "2026-11-07T08:00:00+00:00"},
			{"state": "4.5", "last_changed": "2026-11-07T09:30:00+00:00"},
		}})
	}))
	mux.HandleFunc("GET /api/websocket", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		send := func(v any) { b, _ := json.Marshal(v); _ = c.Write(ctx, websocket.MessageText, b) }
		recv := func() map[string]any {
			_, b, err := c.Read(ctx)
			if err != nil {
				return nil
			}
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			return m
		}
		send(map[string]any{"type": "auth_required", "ha_version": "2026.11.0"})
		if m := recv(); m["type"] != "auth" || m["access_token"] != token {
			send(map[string]any{"type": "auth_invalid", "message": "Invalid access token"})
			return
		}
		send(map[string]any{"type": "auth_ok", "ha_version": "2026.11.0"})
		m := recv()
		if m["type"] != "subscribe_events" {
			f.t.Errorf("subscribe: %v", m)
			return
		}
		f.mu.Lock()
		f.subs = append(f.subs, m["event_type"].(string))
		evs := f.events
		f.mu.Unlock()
		send(map[string]any{"id": m["id"], "type": "result", "success": true, "result": nil})
		for _, e := range evs {
			send(map[string]any{"id": m["id"], "type": "event", "event": e})
		}
		<-ctx.Done()
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type approver struct {
	ok   bool
	asks []string
}

func (a *approver) Approve(_ context.Context, req tools.ApprovalRequest) (bool, error) {
	a.asks = append(a.asks, req.Summary)
	return a.ok, nil
}

func TestTools(t *testing.T) {
	f := &fakeHA{t: t}
	ts := httptest.NewServer(f.handler())
	defer ts.Close()
	reg := tools.NewRegistry()
	RegisterTools(reg, &Toolset{Client: &Client{URL: ts.URL, Token: token}, Allow: []string{"light", "switch"},
		Confirm: []string{"lock"}, Location: time.UTC})
	run := func(env *tools.Env, name string, in any) llm.Block {
		b, _ := json.Marshal(in)
		return reg.Execute(context.Background(), env, llm.Block{Type: llm.ToolUse, ToolUseID: "1", ToolName: name, Input: b})
	}
	env := &tools.Env{Mode: tools.ModeWorkspace}

	res := run(env, "ha_states", map[string]any{"filter": "porch"})
	if res.IsError || !strings.Contains(res.Text, "light.porch: off (Porch light)") || strings.Contains(res.Text, "kitchen") {
		t.Fatalf("states: %+v", res)
	}
	res = run(env, "ha_states", map[string]any{"filter": "sensor.*_temperature"})
	if !strings.Contains(res.Text, "sensor.outside_temperature: 4.5 (Outside) unit_of_measurement=°C") {
		t.Fatalf("glob: %+v", res)
	}
	res = run(env, "ha_states", map[string]any{"domain": "light", "details": true})
	if !strings.Contains(res.Text, "2 of 4") || !strings.Contains(res.Text, `"brightness":200`) {
		t.Fatalf("domain: %+v", res)
	}

	res = run(env, "ha_call_service", map[string]any{"domain": "light", "service": "turn_on", "entity_id": "light.kitchen",
		"data": map[string]any{"brightness_pct": 40}})
	if res.IsError || !strings.Contains(res.Text, "light.kitchen: on") {
		t.Fatalf("call: %+v", res)
	}
	if f.calls[0] != `light.turn_on {"brightness_pct":40,"entity_id":["light.kitchen"]}` {
		t.Fatalf("sent %v", f.calls)
	}
	for _, in := range []map[string]any{
		{"domain": "climate", "service": "set_temperature", "entity_id": "climate.hall"},
		{"domain": "light", "service": "turn_on", "entity_id": "climate.hall"},
		{"domain": "homeassistant", "service": "turn_off", "data": map[string]any{"area_id": "house"}},
	} {
		if res := run(env, "ha_call_service", in); !res.IsError {
			t.Errorf("%v allowed: %+v", in, res)
		}
	}
	// A confirm domain asks even in full mode, and is denied unattended.
	unlock := map[string]any{"domain": "lock", "service": "unlock", "entity_id": "lock.front_door"}
	if res := run(&tools.Env{Mode: tools.ModeFull}, "ha_call_service", unlock); !res.IsError || !strings.Contains(res.Text, "approval") {
		t.Fatalf("unattended unlock: %+v", res)
	}
	no := &approver{}
	if res := run(&tools.Env{Mode: tools.ModeFull, Approver: no}, "ha_call_service", unlock); !res.IsError || len(no.asks) != 1 ||
		no.asks[0] != "lock.unlock on lock.front_door" {
		t.Fatalf("declined unlock: %+v %v", res, no.asks)
	}
	yes := &approver{ok: true}
	if res := run(&tools.Env{Mode: tools.ModeWorkspace, Approver: yes}, "ha_call_service", unlock); res.IsError {
		t.Fatalf("approved unlock: %+v", res)
	}
	if len(f.calls) != 2 || !strings.HasPrefix(f.calls[1], "lock.unlock") {
		t.Fatalf("calls %v", f.calls)
	}

	res = run(env, "ha_history", map[string]any{"entity": "sensor.outside_temperature", "hours": 6})
	if res.IsError || !strings.Contains(res.Text, "2026-11-07 08:00:00 3.0") || !strings.Contains(res.Text, "2026-11-07 09:30:00 4.5") {
		t.Fatalf("history: %+v", res)
	}
	bad := &Client{URL: ts.URL, Token: "wrong"}
	if _, err := bad.States(context.Background()); err == nil || !strings.Contains(err.Error(), "refused the token") {
		t.Fatalf("bad token: %v", err)
	}
}

func stateChanged(entity, from, to, ctxID string, attrs map[string]any) map[string]any {
	return map[string]any{
		"event_type": "state_changed", "time_fired": "2026-11-07T18:00:00+00:00", "origin": "LOCAL",
		"context": map[string]any{"id": ctxID},
		"data": map[string]any{"entity_id": entity,
			"old_state": map[string]any{"entity_id": entity, "state": from, "attributes": attrs},
			"new_state": map[string]any{"entity_id": entity, "state": to, "attributes": attrs}},
	}
}

func TestEventTriggers(t *testing.T) {
	f := &fakeHA{t: t, events: []map[string]any{
		stateChanged("binary_sensor.front_doorbell", "off", "on", "c1", map[string]any{"friendly_name": "Front doorbell"}),
		stateChanged("binary_sensor.front_doorbell", "on", "on", "c2", map[string]any{"friendly_name": "Front doorbell", "x": 1}),
		stateChanged("binary_sensor.back_doorbell", "off", "on", "c3", nil),
		stateChanged("binary_sensor.front_doorbell", "on", "off", "c4", nil),
		stateChanged("binary_sensor.side_doorbell", "off", "on", "c5", nil), // within the cooldown
		{"event_type": "zha_event", "time_fired": "2026-11-07T18:01:00+00:00", "context": map[string]any{"id": "c6"},
			"data": map[string]any{"device_id": "abc", "command": "single"}},
	}}
	ts := httptest.NewServer(f.handler())
	defer ts.Close()
	type fired struct{ trigger, id, text string }
	got := make(chan fired, 10)
	l := &Listener{URL: ts.URL, Token: token, Location: time.UTC, Triggers: []*config.HATrigger{
		{Name: "doorbell", EventType: "state_changed", EntityID: "binary_sensor.*_doorbell", To: "on", Cooldown: time.Hour},
		{Name: "button", EventType: "zha_event", Data: map[string]string{"device_id": "abc", "command": "single"},
			Prompt: "Button {{.Data.command}} pressed"},
	}, Fire: func(_ context.Context, tr *config.HATrigger, id, text string) error {
		got <- fired{tr.Name, id, text}
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	var all []fired
	timeout := time.After(10 * time.Second)
	for len(all) < 2 {
		select {
		case fe := <-got:
			all = append(all, fe)
		case <-timeout:
			t.Fatalf("fired %v", all)
		}
	}
	select {
	case fe := <-got:
		t.Fatalf("extra fire %v", fe)
	case <-time.After(200 * time.Millisecond):
	}
	if all[0].trigger != "doorbell" || all[0].text != "Home Assistant event state_changed for Front doorbell (binary_sensor.front_doorbell): off -> on at 2026-11-07 18:00:00 UTC." {
		t.Fatalf("doorbell: %+v", all[0])
	}
	if all[1].trigger != "button" || all[1].text != "Button single pressed" || !strings.HasPrefix(all[1].id, "turn_") {
		t.Fatalf("button: %+v", all[1])
	}
	f.mu.Lock()
	subs := strings.Join(f.subs, ",")
	f.mu.Unlock()
	if !strings.HasPrefix(subs, "state_changed") {
		t.Fatalf("subscriptions %s", subs)
	}
}

func TestListenerRefusedToken(t *testing.T) {
	f := &fakeHA{t: t}
	ts := httptest.NewServer(f.handler())
	defer ts.Close()
	l := &Listener{URL: ts.URL, Token: "wrong", Triggers: []*config.HATrigger{{Name: "x", EventType: "state_changed"}}}
	if err := l.session(context.Background()); err == nil || !strings.Contains(err.Error(), "refused the token") {
		t.Fatalf("got %v", err)
	}
}
