package telemetry

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

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

func TestExposition(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("x_total", "Things.\nCounted.", "name")
	g := r.Gauge("depth", "Depth.", "kind")
	h := r.Histogram("dur_seconds", "Durations.", []float64{1, 0.1}, "op")
	plain := r.Counter("plain_total", "No labels.")
	c.Add(2, `a"b\c`+"\n")
	c.Inc("z")
	c.Add(-1, "z") // ignored
	g.Set(3, "turn")
	h.Observe(0.05, "op")
	h.Observe(0.5, "op")
	h.Observe(7, "op")
	r.OnScrape(func(context.Context) { g.Set(9, "deliver") })

	var b strings.Builder
	if err := r.Write(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	want := `# HELP depth Depth.
# TYPE depth gauge
depth{kind="deliver"} 9
depth{kind="turn"} 3
# HELP dur_seconds Durations.
# TYPE dur_seconds histogram
dur_seconds_bucket{op="op",le="0.1"} 1
dur_seconds_bucket{op="op",le="1"} 2
dur_seconds_bucket{op="op",le="+Inf"} 3
dur_seconds_sum{op="op"} 7.55
dur_seconds_count{op="op"} 3
# HELP plain_total No labels.
# TYPE plain_total counter
plain_total 0
# HELP x_total Things.\nCounted.
# TYPE x_total counter
x_total{name="a\"b\\c\n"} 2
x_total{name="z"} 1
`
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}
	_ = plain
}

func TestRegistryHandler(t *testing.T) {
	m := NewMetrics()
	m.TurnsStarted.Inc("habakkuk")
	rec := httptest.NewRecorder()
	m.Registry.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `habakkuk_turns_started_total{agent="habakkuk"} 1`) {
		t.Fatal(rec.Body.String())
	}
}

func TestParseHeaders(t *testing.T) {
	got := ParseHeaders("api-key=abc%3D, x-team = ops ,broken,=x")
	if len(got) != 2 || got["api-key"] != "abc=" || got["x-team"] != "ops" {
		t.Fatalf("%v", got)
	}
}

type spanSink struct {
	mu    sync.Mutex
	spans []map[string]any
	fail  bool
}

func (s *spanSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.fail {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []map[string]any `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	data, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(data, &body)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rs := range body.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			s.spans = append(s.spans, ss.Spans...)
		}
	}
}

func TestObserverSpansAndContent(t *testing.T) {
	sink := &spanSink{}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	exp, err := NewExporter(srv.URL+"/", nil, "habakkuk", "test", nil)
	if err != nil || !strings.HasSuffix(exp.URL, "/v1/traces") {
		t.Fatalf("%v %v", exp, err)
	}
	o := &Observer{M: NewMetrics(), Tracer: NewTracer(exp), Content: true}
	o.TurnStart("habakkuk", "s1", "j1", "hello there")
	o.ToolStart("s1", "exec", "c1", `{"command":"ls"}`)
	o.ToolEnd("s1", "exec", "c1", "denied by permission policy: no", ToolOutcome("denied by permission policy: no", true))
	o.TurnEnd("habakkuk", "s1", "j1", "", "model failed")
	exp.Close(context.Background())

	if o.M.TurnsFailed.Value("habakkuk") != 1 || o.M.ToolCalls.Value("exec", "denied") != 1 {
		t.Fatal("metrics not recorded")
	}
	if len(sink.spans) != 2 {
		t.Fatalf("spans %v", sink.spans)
	}
	raw, _ := json.Marshal(sink.spans)
	for _, want := range []string{`"hello there"`, `"habakkuk.tool.input"`, `"code":2`, `"model failed"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("spans lack %s: %s", want, raw)
		}
	}
}

// Without a tracer nothing is exported and nothing breaks.
func TestObserverWithoutTracer(t *testing.T) {
	o := &Observer{M: NewMetrics()}
	o.TurnEnd("habakkuk", "s", "j", "x", "") // an end with no start
	o.ToolEnd("s", "t", "c", "", "ok")
	var sp *Span
	sp.Set("k", 1)
	sp.End(nil)
	if o.M.TurnsFinished.Value("habakkuk") != 1 {
		t.Fatal("finish not counted")
	}
}

type failing struct{}

func (failing) Stream(context.Context, string, llm.Request, func(llm.Event)) (*llm.Response, error) {
	return nil, &llm.APIError{Status: 429, Message: "slow down"}
}

type lister struct{ failing }

func (lister) ListModels(context.Context, string) ([]string, error) { return []string{"m"}, nil }

func TestProviderWrapper(t *testing.T) {
	o := &Observer{M: NewMetrics()}
	p := o.Provider("ep", failing{})
	if _, ok := p.(llm.ModelLister); ok {
		t.Fatal("wrapper claims to list models")
	}
	if _, err := p.Stream(context.Background(), "", llm.Request{Model: "m1"}, nil); err == nil {
		t.Fatal("no error")
	}
	if o.M.ModelCalls.Value("ep", "m1", "rate_limit") != 1 || o.M.ModelDuration.Count("ep", "m1") != 1 {
		t.Fatal("failed call not counted")
	}
	if _, ok := o.Provider("ep", lister{}).(llm.ModelLister); !ok {
		t.Fatal("wrapper lost ListModels")
	}
}

func TestExporterDropsWhenFull(t *testing.T) {
	sink := &spanSink{fail: true}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	exp, _ := NewExporter(srv.URL, nil, "habakkuk", "test", nil)
	exp.MaxQueue, exp.Interval = 2, time.Hour
	tr := NewTracer(exp)
	for range 5 {
		tr.Start("x", nil, KindInternal).End(errors.New("e"))
	}
	exp.mu.Lock()
	queued, dropped := len(exp.queue), exp.dropped
	exp.mu.Unlock()
	if queued != 2 || dropped != 3 {
		t.Fatalf("queued %d dropped %d", queued, dropped)
	}
	exp.Close(context.Background()) // a failing collector does not hang Close
}
