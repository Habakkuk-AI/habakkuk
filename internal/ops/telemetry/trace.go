package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Span kinds, as OTLP numbers them.
const (
	KindInternal = 1
	KindClient   = 3
)

// Span is one timed operation. Spans are safe to end from any goroutine;
// a nil *Span ignores every call, so callers need no tracer checks.
type Span struct {
	t       *Tracer
	traceID [16]byte
	spanID  [8]byte
	parent  [8]byte
	name    string
	kind    int
	start   time.Time

	mu    sync.Mutex
	attrs []attr
	ended bool
}

type attr struct {
	key string
	val any // string, int64, float64 or bool
}

// Tracer makes spans and hands ended ones to an exporter in batches.
type Tracer struct {
	exp *Exporter
}

// NewTracer returns a tracer exporting through exp; a nil exp gives a nil
// tracer, whose spans are nil and cost nothing.
func NewTracer(exp *Exporter) *Tracer {
	if exp == nil {
		return nil
	}
	return &Tracer{exp: exp}
}

// Start begins a span, a child of parent when parent is not nil.
func (t *Tracer) Start(name string, parent *Span, kind int) *Span {
	if t == nil {
		return nil
	}
	s := &Span{t: t, name: name, kind: kind, start: time.Now()}
	if parent != nil {
		s.traceID, s.parent = parent.traceID, parent.spanID
	} else {
		rand.Read(s.traceID[:])
	}
	rand.Read(s.spanID[:])
	return s
}

// Set adds an attribute: a string, an integer, a float or a bool.
func (s *Span) Set(key string, v any) {
	if s == nil {
		return
	}
	switch x := v.(type) {
	case int:
		v = int64(x)
	case int64, float64, bool, string:
	default:
		v = fmt.Sprint(v)
	}
	s.mu.Lock()
	s.attrs = append(s.attrs, attr{key, v})
	s.mu.Unlock()
}

// End finishes the span, with an error status when err is set. Only the
// first End counts.
func (s *Span) End(err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	rec := spanRecord{
		TraceID: hex.EncodeToString(s.traceID[:]), SpanID: hex.EncodeToString(s.spanID[:]),
		Name: s.name, Kind: s.kind,
		Start: strconv.FormatInt(s.start.UnixNano(), 10), End: strconv.FormatInt(time.Now().UnixNano(), 10),
		Status: status{Code: 1},
	}
	if s.parent != ([8]byte{}) {
		rec.Parent = hex.EncodeToString(s.parent[:])
	}
	if err != nil {
		rec.Status = status{Code: 2, Message: truncate(err.Error(), 500)}
	}
	for _, a := range s.attrs {
		rec.Attrs = append(rec.Attrs, keyValue(a.key, a.val))
	}
	s.mu.Unlock()
	s.t.exp.add(rec)
}

// TraceID is the span's trace id in hex.
func (s *Span) TraceID() string {
	if s == nil {
		return ""
	}
	return hex.EncodeToString(s.traceID[:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// OTLP/HTTP JSON shapes (opentelemetry-proto, JSON mapping).
type spanRecord struct {
	TraceID string     `json:"traceId"`
	SpanID  string     `json:"spanId"`
	Parent  string     `json:"parentSpanId,omitempty"`
	Name    string     `json:"name"`
	Kind    int        `json:"kind"`
	Start   string     `json:"startTimeUnixNano"`
	End     string     `json:"endTimeUnixNano"`
	Attrs   []kvRecord `json:"attributes,omitempty"`
	Status  status     `json:"status"`
}

type status struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

type kvRecord struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func keyValue(k string, v any) kvRecord {
	switch x := v.(type) {
	case int64:
		return kvRecord{k, map[string]any{"intValue": strconv.FormatInt(x, 10)}}
	case float64:
		return kvRecord{k, map[string]any{"doubleValue": x}}
	case bool:
		return kvRecord{k, map[string]any{"boolValue": x}}
	default:
		return kvRecord{k, map[string]any{"stringValue": fmt.Sprint(x)}}
	}
}

// Exporter posts spans to an OTLP/HTTP collector as JSON, in batches, from
// one goroutine. When the collector is slow or down, spans beyond the
// queue's capacity are dropped rather than held in memory.
type Exporter struct {
	URL      string
	Headers  map[string]string
	Client   *http.Client
	Log      *slog.Logger
	Service  string
	Version  string
	Interval time.Duration // flush period; default 5s
	MaxQueue int           // default 2048
	MaxBatch int           // default 512

	mu      sync.Mutex
	queue   []spanRecord
	dropped int
	kick    chan struct{}
	done    chan struct{}
	stop    chan struct{}
	once    sync.Once
	stopped sync.Once
	lastErr time.Time
}

// NewExporter returns an exporter for an OTLP base endpoint: spans go to
// <endpoint>/v1/traces unless it already ends in /v1/traces.
func NewExporter(endpoint string, headers map[string]string, service, version string, log *slog.Logger) (*Exporter, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("otlp endpoint %q must be an http:// or https:// URL", endpoint)
	}
	if !strings.HasSuffix(u.Path, "/v1/traces") {
		u.Path = strings.TrimRight(u.Path, "/") + "/v1/traces"
	}
	if log == nil {
		log = slog.Default()
	}
	return &Exporter{URL: u.String(), Headers: headers, Service: service, Version: version, Log: log}, nil
}

// ParseHeaders reads "k=v,k2=v2" (values may be URL-encoded), the form of
// OTEL_EXPORTER_OTLP_HEADERS.
func ParseHeaders(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(part, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			continue
		}
		if dv, err := url.QueryUnescape(strings.TrimSpace(v)); err == nil {
			v = dv
		}
		out[k] = strings.TrimSpace(v)
	}
	return out
}

func (e *Exporter) init() {
	e.once.Do(func() {
		if e.Interval <= 0 {
			e.Interval = 5 * time.Second
		}
		if e.MaxQueue <= 0 {
			e.MaxQueue = 2048
		}
		if e.MaxBatch <= 0 {
			e.MaxBatch = 512
		}
		if e.Client == nil {
			e.Client = &http.Client{Timeout: 10 * time.Second}
		}
		e.kick = make(chan struct{}, 1)
		e.stop = make(chan struct{})
		e.done = make(chan struct{})
		go e.loop()
	})
}

func (e *Exporter) add(rec spanRecord) {
	e.init()
	e.mu.Lock()
	if len(e.queue) >= e.MaxQueue {
		e.dropped++
		e.mu.Unlock()
		return
	}
	e.queue = append(e.queue, rec)
	full := len(e.queue) >= e.MaxBatch
	e.mu.Unlock()
	if full {
		select {
		case e.kick <- struct{}{}:
		default:
		}
	}
}

func (e *Exporter) loop() {
	defer close(e.done)
	t := time.NewTicker(e.Interval)
	defer t.Stop()
	for {
		select {
		case <-e.stop:
			e.flush(context.Background())
			return
		case <-t.C:
		case <-e.kick:
		}
		e.flush(context.Background())
	}
}

// flush sends everything queued, a batch at a time.
func (e *Exporter) flush(ctx context.Context) {
	for {
		e.mu.Lock()
		n := min(len(e.queue), e.MaxBatch)
		batch := e.queue[:n:n]
		e.queue = e.queue[n:]
		dropped := e.dropped
		e.dropped = 0
		e.mu.Unlock()
		if dropped > 0 {
			e.Log.Warn("trace export queue full; spans dropped", "count", dropped)
		}
		if n == 0 {
			return
		}
		if err := e.send(ctx, batch); err != nil {
			if time.Since(e.lastErr) > time.Minute { // one warning a minute, not one per batch
				e.lastErr = time.Now()
				e.Log.Warn("trace export failed", "err", err, "spans", n)
			}
			return
		}
	}
}

func (e *Exporter) send(ctx context.Context, spans []spanRecord) error {
	e.mu.Lock()
	version := e.Version
	e.mu.Unlock()
	body := map[string]any{"resourceSpans": []any{map[string]any{
		"resource": map[string]any{"attributes": []kvRecord{
			keyValue("service.name", e.Service), keyValue("service.version", version),
		}},
		"scopeSpans": []any{map[string]any{
			"scope": map[string]any{"name": "habakkuk", "version": version},
			"spans": spans,
		}},
	}}}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.Headers {
		req.Header.Set(k, v)
	}
	resp, err := e.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("collector answered %s", resp.Status)
	}
	return nil
}

// SetVersion sets the service.version resource attribute.
func (e *Exporter) SetVersion(v string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.Version = v
	e.mu.Unlock()
}

// Close sends what is queued, waiting at most until ctx ends.
func (e *Exporter) Close(ctx context.Context) {
	if e == nil {
		return
	}
	e.init()
	e.stopped.Do(func() { close(e.stop) })
	select {
	case <-e.done:
	case <-ctx.Done():
	}
}
