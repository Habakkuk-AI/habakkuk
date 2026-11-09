package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/ops/telemetry"
)

// twoStep calls list_dir once, then answers.
type twoStep struct {
	mu    sync.Mutex
	calls int
}

func (m *twoStep) Stream(_ context.Context, _ string, _ llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	u := llm.Usage{InputTokens: 1000, OutputTokens: 50}
	if m.calls == 1 {
		return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.ToolUse, ToolUseID: "call_1",
			ToolName: "list_dir", Input: json.RawMessage(`{"path":"."}`)}}}, StopReason: llm.StopToolUse, Usage: u}, nil
	}
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "all done"}}},
		StopReason: llm.StopEnd, Usage: u}, nil
}

type collector struct {
	mu    sync.Mutex
	spans []map[string]any
	auth  string
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []map[string]any `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	data, _ := io.ReadAll(r.Body)
	if r.URL.Path != "/v1/traces" || json.Unmarshal(data, &body) != nil {
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.auth = r.Header.Get("Authorization")
	for _, rs := range body.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			c.spans = append(c.spans, ss.Spans...)
		}
	}
}

func TestTelemetryRecordsATurn(t *testing.T) {
	col := &collector{}
	srv := httptest.NewServer(col)
	defer srv.Close()
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("TEST_HTTP_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("TEST_OTLP_HEADERS", "Authorization=Bearer%20collector-token")
	a, _, _ := openTestApp(t, "http:\n  listen: 127.0.0.1:0\n  token_env: TEST_HTTP_TOKEN\n"+
		"telemetry:\n  otlp_endpoint: "+srv.URL+"\n  headers_env: TEST_OTLP_HEADERS\n")
	ctx := context.Background()
	model := &twoStep{}
	ep := a.Router.Endpoints["anthropic"]
	ep.Provider = a.Telemetry.Observer.Provider("anthropic", model)
	a.Runner.Agents["habakkuk"].AutoTitle = false // count only the turn's own model calls

	sess, err := a.Runner.NewSession(ctx, "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Runner.Submit(ctx, sess.ID, "look around"); err != nil {
		t.Fatal(err)
	}
	pools := a.Pools(false)
	handle := pools[0].Handlers[agent.KindTurn]
	j, err := a.Queue.Claim(ctx, "w", time.Minute, agent.KindTurn)
	if err != nil || j == nil {
		t.Fatalf("claim: %v %v", j, err)
	}
	if res, err := handle(ctx, j, "w"); err != nil || res != "all done" {
		t.Fatalf("turn: %q %v", res, err)
	}

	var b strings.Builder
	if err := a.Telemetry.Metrics.Registry.Write(ctx, &b); err != nil {
		t.Fatal(err)
	}
	text := b.String()
	for _, want := range []string{
		`habakkuk_turns_started_total{agent="habakkuk"} 1`,
		`habakkuk_turns_finished_total{agent="habakkuk"} 1`,
		`habakkuk_turn_duration_seconds_count{agent="habakkuk"} 1`,
		`habakkuk_model_calls_total{provider="anthropic",model="claude-opus-5-5",outcome="ok"} 2`,
		`habakkuk_model_tokens_total{provider="anthropic",model="claude-opus-5-5",kind="input"} 2000`,
		`habakkuk_model_cost_usd_total{provider="anthropic",model="claude-opus-5-5"}`,
		`habakkuk_tool_calls_total{tool="list_dir",outcome="ok"} 1`,
		`habakkuk_tool_call_duration_seconds_count{tool="list_dir"} 1`,
		`habakkuk_jobs_total{kind="turn",outcome="done"} 1`,
		`habakkuk_build_info{version="dev"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	if t.Failed() {
		t.Log(text)
	}

	// /metrics needs the API token unless metrics.public is set.
	h := a.Web.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/metrics", nil)
	req.Header.Set("Authorization", "Bearer 0123456789abcdef0123456789abcdef")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "habakkuk_turns_started_total") ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("with token: %d %s", rec.Code, rec.Body.String())
	}

	// Spans: one turn, two model calls and the tool call under it, with no
	// message content by default.
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	a.Telemetry.Exporter.Close(cctx)
	col.mu.Lock()
	defer col.mu.Unlock()
	if col.auth != "Bearer collector-token" {
		t.Errorf("collector auth header %q", col.auth)
	}
	byName := map[string][]map[string]any{}
	for _, s := range col.spans {
		byName[s["name"].(string)] = append(byName[s["name"].(string)], s)
	}
	turns, chats, tools := byName["turn habakkuk"], byName["chat claude-opus-5-5"], byName["tool list_dir"]
	if len(turns) != 1 || len(chats) != 2 || len(tools) != 1 {
		t.Fatalf("spans: %v", col.spans)
	}
	turnID, traceID := turns[0]["spanId"], turns[0]["traceId"]
	for _, s := range append(chats, tools...) {
		if s["parentSpanId"] != turnID || s["traceId"] != traceID {
			t.Errorf("span %s is not the turn's child: %v", s["name"], s)
		}
	}
	raw, _ := json.Marshal(col.spans)
	for _, leak := range []string{"look around", "all done", `"habakkuk.input"`, `"habakkuk.tool.result"`} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("spans carry content %q without include_content", leak)
		}
	}
}

func TestJobOutcomesAndDeliveries(t *testing.T) {
	m := telemetry.NewMetrics()
	payload := json.RawMessage(`{"to":{"account":"tg","conversation":"1"}}`)
	run := func(err error, attempts int) {
		h := observeJob(m, channel.KindDeliver, func(context.Context, *jobs.Job, string) (string, error) { return "", err })
		_, _ = h(context.Background(), &jobs.Job{Kind: channel.KindDeliver, Payload: payload, Attempts: attempts, MaxAttempts: 3}, "w")
	}
	run(nil, 1)
	run(errors.New("flaky"), 1)
	run(errors.New("flaky"), 3)
	run(jobs.Permanent{Err: errors.New("bot blocked")}, 1)
	run(jobs.ErrLeaseLost, 1)
	for outcome, want := range map[string]float64{"sent": 1, "retry": 1, "failed": 2, "lost": 0} {
		if got := m.Deliveries.Value("tg", outcome); got != want {
			t.Errorf("deliveries %s = %v, want %v", outcome, got, want)
		}
	}
	if m.JobRetries.Value(channel.KindDeliver) != 1 || m.Jobs.Value(channel.KindDeliver, "lost") != 1 {
		t.Errorf("retries %v, lost %v", m.JobRetries.Value(channel.KindDeliver), m.Jobs.Value(channel.KindDeliver, "lost"))
	}
}
