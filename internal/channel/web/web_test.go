package web

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

const token = "s3cret-token"

// echo answers "echo: <text>" to the last user message.
type echo struct {
	mu    sync.Mutex
	calls int
}

func (e *echo) Send(_ context.Context, _ []llm.Target, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	_, msg, _ := strings.Cut(req.Messages[len(req.Messages)-1].Text(), "\n") // drop the time stamp line
	// Stream it in two pieces, as a provider would.
	if onEvent != nil {
		onEvent(llm.Event{Type: llm.EventText, Text: "echo: "})
		onEvent(llm.Event{Type: llm.EventText, Text: msg})
	}
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "echo: " + msg}}},
		StopReason: llm.StopEnd}, nil
}

func (e *echo) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

type rig struct {
	s     *Server
	ts    *httptest.Server
	q     *jobs.Queue
	model *echo
}

func newRig(t *testing.T, tok string) *rig {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	control, err := store.Open(ctx, filepath.Join(dir, "control.db"), slices.Concat(jobs.Migrations, channel.Migrations))
	if err != nil {
		t.Fatal(err)
	}
	transcripts, err := store.Open(ctx, filepath.Join(dir, "transcripts.db"), session.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	st, err := OpenStore(ctx, filepath.Join(dir, "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close(); transcripts.Close(); st.Close() })
	ws := filepath.Join(dir, "ws")
	os.MkdirAll(ws, 0o755)
	reg := tools.NewRegistry()
	tools.RegisterBuiltins(reg)
	model := &echo{}
	q := jobs.NewQueue(control)
	spec := func(name string) *agent.Spec {
		return &agent.Spec{Name: name, Chain: []llm.Target{{Endpoint: "fake", Model: "m"}}, MaxTokens: 1000,
			MaxIterations: 10, Toolsets: []string{"core"}, Mode: tools.ModeWorkspace, Workspace: ws, Dir: dir}
	}
	runner := &agent.Runner{
		Agents:   map[string]*agent.Spec{"habakkuk": spec("habakkuk"), "ops": spec("ops")},
		Sessions: session.NewStore(transcripts), Queue: q, Models: model, Tools: reg, Bus: events.NewBus(), Log: quiet,
		Env: func(s *agent.Spec, _ string) *tools.Env { return &tools.Env{Workspace: s.Workspace, Mode: s.Mode} },
	}
	s := &Server{Account: "web", Addr: "127.0.0.1:0", Token: tok, Store: st, Log: quiet, Hooks: map[string]*Hook{}}
	g := &channel.Gateway{
		Channels: map[string]*config.Channel{"web": {Type: "web", Agent: "habakkuk", AllowFrom: []string{"*"}, Groups: "mention"}},
		Adapters: map[string]channel.Adapter{"web": s}, Runner: runner, Queue: q, Store: channel.NewStore(control), Log: quiet,
	}
	runner.Approvers = g.ApproverFor
	s.Gateway = g
	hook, err := NewHook("alerts", &config.Webhook{Agent: "habakkuk", Prompt: "Alert {{.title}} ({{.id}})"}, "hook-secret")
	if err != nil {
		t.Fatal(err)
	}
	s.Hooks["alerts"] = hook
	tg, _ := NewHook("tg", &config.Webhook{Agent: "ops", Deliver: "web:ops-room"}, "bearer-secret")
	s.Hooks["tg"] = tg

	pool := &jobs.Pool{Queue: q, Handlers: map[string]jobs.Handler{agent.KindTurn: g.HandleTurn, channel.KindDeliver: g.HandleDeliver},
		Concurrency: 2, Lease: 5 * time.Second, Poll: 10 * time.Millisecond, Log: quiet}
	g.Wake = pool.Wake
	pctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pool.Run(pctx) }()
	go func() { defer wg.Done(); g.Run(pctx) }()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); cancel(); wg.Wait() })
	waitFor(t, "web channel up", func() bool { return s.currentInbox() != nil })
	return &rig{s: s, ts: ts, q: q, model: model}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (r *rig) do(t *testing.T, method, path, tok, contentType, body string, hdr ...string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, r.ts.URL+path, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			req.Host = hdr[i+1]
			continue
		}
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	data, _ := io.ReadAll(res.Body)
	_ = json.Unmarshal(data, &out)
	return res.StatusCode, out
}

func (r *rig) chat(t *testing.T, conv, text string) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"conversation": conv, "text": text})
	code, out := r.do(t, "POST", "/api/chat", token, "application/json", string(body))
	if code != http.StatusAccepted {
		t.Fatalf("chat: %d %v", code, out)
	}
	return out
}

type stream struct {
	events chan Event
	typing chan struct{}
	cancel func()
}

// open reads an event stream in the background.
func (r *rig) open(t *testing.T, path string, hdr ...string) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", r.ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		cancel()
		t.Fatalf("events: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	s := &stream{events: make(chan Event, 100), typing: make(chan struct{}, 100), cancel: cancel}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		event := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && event == "message":
				var e Event
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e) == nil {
					s.events <- e
				}
			case strings.HasPrefix(line, "data: ") && event == "typing":
				s.typing <- struct{}{}
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	return s
}

// next returns the next message event or fails after a timeout.
func (s *stream) next(t *testing.T) Event {
	t.Helper()
	select {
	case e := <-s.events:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
		return Event{}
	}
}

func (s *stream) none(t *testing.T) {
	t.Helper()
	select {
	case e := <-s.events:
		t.Fatalf("unexpected event %+v", e)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestAPIRequiresToken(t *testing.T) {
	r := newRig(t, token)
	body := `{"conversation":"c1","text":"hi"}`
	if code, _ := r.do(t, "POST", "/api/chat", "", "application/json", body); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := r.do(t, "POST", "/api/chat", "wrong", "application/json", body); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", code)
	}
	if code, _ := r.do(t, "GET", "/api/chat/c1/events", "", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("events without token: %d", code)
	}
	if code, _ := r.do(t, "GET", "/api/agents", "", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("agents without token: %d", code)
	}
	if code, _ := r.do(t, "POST", "/api/chat", token, "text/plain", body); code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain accepted: %d", code)
	}
	if r.model.count() != 0 {
		t.Fatal("an unauthenticated message reached the model")
	}
	res, err := http.Get(r.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(page), "<title>Habakkuk</title>") {
		t.Fatalf("chat page: %d", res.StatusCode)
	}
}

func TestHealthNeedsNoToken(t *testing.T) {
	r := newRig(t, token)
	waitFor(t, "status recorded", func() bool {
		code, out := r.do(t, "GET", "/healthz", "", "", "")
		chans, _ := out["channels"].([]any)
		return code == http.StatusOK && out["status"] == "ok" && len(chans) == 1
	})
	_, out := r.do(t, "GET", "/healthz", "", "", "")
	ch := out["channels"].([]any)[0].(map[string]any)
	q, ok := out["queue"].(map[string]any)
	if ch["account"] != "web" || ch["state"] != "connected" || !ok || q["pending"] == nil {
		t.Fatalf("health: %v", out)
	}
}

func TestChatTurnRepliesOverSSE(t *testing.T) {
	r := newRig(t, token)
	s := r.open(t, "/api/chat/c1/events")
	out := r.chat(t, "c1", "hello there")
	turn, _ := out["turn"].(string)
	if !strings.HasPrefix(turn, "turn_") {
		t.Fatalf("no turn id: %v", out)
	}
	if j, err := r.q.Get(context.Background(), turn); err != nil || j.Kind != agent.KindTurn {
		t.Fatalf("turn job: %v %v", j, err)
	}
	if e := s.next(t); e.Role != "user" || e.Text != "hello there" {
		t.Fatalf("first event %+v", e)
	}
	if e := s.next(t); e.Role != "assistant" || e.Text != "echo: hello there" {
		t.Fatalf("reply %+v", e)
	}
	// Another conversation's traffic does not leak into this stream.
	r.chat(t, "c2", "elsewhere")
	s.none(t)
}

func TestReconnectCatchesUp(t *testing.T) {
	r := newRig(t, token)
	r.chat(t, "c1", "one")
	waitFor(t, "reply stored", func() bool {
		evs, _ := r.s.Store.After(context.Background(), "c1", 0, 10)
		return len(evs) == 2
	})
	all := r.open(t, "/api/chat/c1/events?after=0")
	first, second := all.next(t), all.next(t)
	if first.Text != "one" || second.Text != "echo: one" || second.Seq <= first.Seq {
		t.Fatalf("catch-up: %+v %+v", first, second)
	}
	all.cancel()

	// A client that saw the first event resumes after it, by query or by
	// the Last-Event-ID header an EventSource sends.
	byQuery := r.open(t, "/api/chat/c1/events?after="+itoa(first.Seq))
	if e := byQuery.next(t); e.Seq != second.Seq {
		t.Fatalf("resumed at %+v", e)
	}
	byHeader := r.open(t, "/api/chat/c1/events", "Last-Event-ID", itoa(second.Seq))
	byHeader.none(t)
	r.chat(t, "c1", "two")
	if e := byHeader.next(t); e.Text != "two" {
		t.Fatalf("live after resume: %+v", e)
	}
}

func TestResentMessageRunsOnce(t *testing.T) {
	r := newRig(t, token)
	body := `{"conversation":"c1","text":"once","id":"m-1"}`
	_, a := r.do(t, "POST", "/api/chat", token, "application/json", body)
	_, b := r.do(t, "POST", "/api/chat", token, "application/json", body)
	if a["turn"] != b["turn"] || a["turn"] == "" {
		t.Fatalf("turns %v %v", a, b)
	}
	waitFor(t, "reply", func() bool {
		evs, _ := r.s.Store.After(context.Background(), "c1", 0, 10)
		return len(evs) == 2
	})
	time.Sleep(150 * time.Millisecond)
	evs, _ := r.s.Store.After(context.Background(), "c1", 0, 10)
	if len(evs) != 2 || r.model.count() != 1 {
		t.Fatalf("events %+v, model calls %d", evs, r.model.count())
	}
}

func TestCommandsAndAgentPick(t *testing.T) {
	r := newRig(t, token)
	ctx := context.Background()
	body := `{"conversation":"c1","text":"hi","agent":"ops"}`
	if code, out := r.do(t, "POST", "/api/chat", token, "application/json", body); code != http.StatusAccepted {
		t.Fatalf("%d %v", code, out)
	}
	to := channel.Address{Account: "web", Conversation: "c1"}
	waitFor(t, "bound", func() bool { c, _ := r.s.Gateway.Store.Conversation(ctx, to); return c != nil })
	before, _ := r.s.Gateway.Store.Conversation(ctx, to)
	if before.Agent != "ops" {
		t.Fatalf("agent %q, want ops", before.Agent)
	}
	out := r.chat(t, "c1", "/new") // no agent given: the conversation keeps ops
	if out["turn"] != "" {
		t.Fatalf("a command started a turn: %v", out)
	}
	// The reply to "hi" may land after the acknowledgement, so look at all.
	waitFor(t, "ack", func() bool {
		evs, _ := r.s.Store.After(ctx, "c1", 0, 10)
		for _, e := range evs {
			if strings.Contains(e.Text, "fresh conversation") {
				return true
			}
		}
		return false
	})
	after, _ := r.s.Gateway.Store.Conversation(ctx, to)
	if after.SessionID == before.SessionID || after.Agent != "ops" {
		t.Fatalf("after /new: %+v", after)
	}
	if code, _ := r.do(t, "POST", "/api/chat", token, "application/json", `{"conversation":"c1","text":"x","agent":"ghost"}`); code != http.StatusBadRequest {
		t.Fatalf("unknown agent: %d", code)
	}
	if code, _ := r.do(t, "POST", "/api/chat", token, "application/json", `{"conversation":"../x","text":"x"}`); code != http.StatusBadRequest {
		t.Fatalf("bad conversation id: %d", code)
	}
}

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookSignature(t *testing.T) {
	r := newRig(t, token)
	body := `{"title":"disk full","id":12345678}`
	replies := r.open(t, "/api/chat/hook-alerts/events")

	if code, _ := r.do(t, "POST", "/hooks/alerts", "", "application/json", body,
		"X-Habakkuk-Signature", sign("wrong", body)); code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", code)
	}
	if code, _ := r.do(t, "POST", "/hooks/alerts", "", "application/json", body); code != http.StatusUnauthorized {
		t.Fatalf("unsigned: %d", code)
	}
	if code, _ := r.do(t, "POST", "/hooks/alerts", token, "application/json", body); code != http.StatusUnauthorized {
		t.Fatalf("the API token opened a webhook: %d", code)
	}
	if code, _ := r.do(t, "POST", "/hooks/nope", "", "", body); code != http.StatusNotFound {
		t.Fatalf("unknown hook: %d", code)
	}
	if r.model.count() != 0 {
		t.Fatal("a rejected webhook reached the model")
	}

	code, out := r.do(t, "POST", "/hooks/alerts", "", "application/json", body,
		"X-Habakkuk-Signature", sign("hook-secret", body), "X-Habakkuk-Delivery", "d-1")
	if code != http.StatusAccepted || out["turn"] == "" {
		t.Fatalf("good signature: %d %v", code, out)
	}
	e := replies.next(t)
	if e.Role != "assistant" || !strings.Contains(e.Text, "Alert disk full (12345678)") {
		t.Fatalf("reply %+v", e)
	}

	// A replay of the same delivery is accepted but runs nothing.
	code, again := r.do(t, "POST", "/hooks/alerts", "", "application/json", body,
		"X-Habakkuk-Signature", sign("hook-secret", body), "X-Habakkuk-Delivery", "d-1")
	if code != http.StatusAccepted || again["turn"] != out["turn"] || again["duplicate"] != true {
		t.Fatalf("replay: %d %v", code, again)
	}
	// Without a delivery id the body hash identifies the delivery.
	other := `{"title":"cpu hot","id":1}`
	_, a := r.do(t, "POST", "/hooks/alerts", "", "", other, "X-Habakkuk-Signature", sign("hook-secret", other))
	_, b := r.do(t, "POST", "/hooks/alerts", "", "", other, "X-Habakkuk-Signature", sign("hook-secret", other))
	if a["turn"] != b["turn"] || b["duplicate"] != true {
		t.Fatalf("body-hash replay: %v %v", a, b)
	}
	replies.next(t)
	replies.none(t)
	if n := r.model.count(); n != 2 {
		t.Fatalf("model ran %d times, want 2", n)
	}
}

func TestWebhookBearerAndDeliver(t *testing.T) {
	r := newRig(t, token)
	room := r.open(t, "/api/chat/ops-room/events")
	if code, out := r.do(t, "POST", "/hooks/tg", "bearer-secret", "", "ping"); code != http.StatusAccepted {
		t.Fatalf("bearer: %d %v", code, out)
	}
	if e := room.next(t); !strings.Contains(e.Text, "echo: ") || !strings.Contains(e.Text, "ping") {
		t.Fatalf("delivered %+v", e)
	}
	big := strings.Repeat("x", maxBody+1)
	if code, _ := r.do(t, "POST", "/hooks/tg", "bearer-secret", "", big); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", code)
	}
}

func TestTokenlessServerStaysLocal(t *testing.T) {
	r := newRig(t, "")
	if code, out := r.do(t, "POST", "/api/chat", "", "application/json", `{"conversation":"c1","text":"hi"}`); code != http.StatusAccepted {
		t.Fatalf("loopback without token: %d %v", code, out)
	}
	// DNS rebinding: a page on another name resolving to 127.0.0.1.
	if code, _ := r.do(t, "GET", "/api/agents", "", "", "", "Host", "evil.example"); code != http.StatusForbidden {
		t.Fatalf("foreign Host answered: %d", code)
	}
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.0.2.1:8484"} {
		s := &Server{Addr: addr}
		if ln, err := s.Listen(); err == nil {
			ln.Close()
			t.Fatalf("listened on %s without a token", addr)
		}
	}
	s := &Server{Addr: "127.0.0.1:0", TokenEnv: "HABAKKUK_HTTP_TOKEN"}
	if _, err := s.Listen(); err == nil || !strings.Contains(err.Error(), "HABAKKUK_HTTP_TOKEN") {
		t.Fatalf("missing token variable: %v", err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
