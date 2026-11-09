// Package telemetry holds Habakkuk's metrics registry, served in the
// Prometheus text format, and a small OTLP/HTTP trace exporter. Both are
// standard library only and hold no global state: the app owns one
// Registry and one Tracer and hands them to whatever records into them.
package telemetry

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
)

type kind int

const (
	counterKind kind = iota
	gaugeKind
	histogramKind
)

func (k kind) String() string {
	return [...]string{"counter", "gauge", "histogram"}[k]
}

// Registry holds metric families and writes them in the Prometheus text
// exposition format (version 0.0.4).
type Registry struct {
	mu       sync.Mutex
	families []*family
	byName   map[string]*family
	scrapes  []func(context.Context)
}

func NewRegistry() *Registry { return &Registry{byName: map[string]*family{}} }

type family struct {
	name, help string
	kind       kind
	labels     []string
	buckets    []float64 // histograms

	mu     sync.Mutex
	series map[string]*series // key: label values joined by \xff
}

type series struct {
	values []string
	value  float64  // counter, gauge
	counts []uint64 // histogram: per bucket (not cumulative)
	sum    float64  // histogram
	count  uint64   // histogram
}

func (r *Registry) add(name, help string, k kind, buckets []float64, labels []string) *family {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.byName[name]; ok {
		if f.kind != k || !slices.Equal(f.labels, labels) {
			panic("telemetry: metric " + name + " registered twice with different shapes")
		}
		return f
	}
	f := &family{name: name, help: help, kind: k, labels: labels, buckets: buckets, series: map[string]*series{}}
	r.families = append(r.families, f)
	r.byName[name] = f
	return f
}

// OnScrape registers a function run before every exposition, to set gauges
// that are read from storage (queue depth, channel state).
func (r *Registry) OnScrape(fn func(context.Context)) {
	r.mu.Lock()
	r.scrapes = append(r.scrapes, fn)
	r.mu.Unlock()
}

func (f *family) get(values []string) *series {
	if len(values) != len(f.labels) {
		panic(fmt.Sprintf("telemetry: %s wants %d label values, got %d", f.name, len(f.labels), len(values)))
	}
	key := strings.Join(values, "\xff")
	s := f.series[key]
	if s == nil {
		s = &series{values: slices.Clone(values)}
		if f.kind == histogramKind {
			s.counts = make([]uint64, len(f.buckets))
		}
		f.series[key] = s
	}
	return s
}

// Counter is a monotonically increasing value per label set.
type Counter struct{ f *family }

func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	return &Counter{r.add(name, help, counterKind, nil, labels)}
}

// Add adds v (which must not be negative) to the series for the labels.
func (c *Counter) Add(v float64, labels ...string) {
	if v < 0 || math.IsNaN(v) {
		return
	}
	c.f.mu.Lock()
	c.f.get(labels).value += v
	c.f.mu.Unlock()
}

func (c *Counter) Inc(labels ...string) { c.Add(1, labels...) }

// Set replaces a counter's value. It is for counters kept elsewhere (a
// channel's restart count) and copied in at scrape time.
func (c *Counter) Set(v float64, labels ...string) {
	c.f.mu.Lock()
	c.f.get(labels).value = v
	c.f.mu.Unlock()
}

// Value returns the current value (for tests and status output).
func (c *Counter) Value(labels ...string) float64 {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	if s := c.f.series[strings.Join(labels, "\xff")]; s != nil {
		return s.value
	}
	return 0
}

// Gauge is a value that goes up and down.
type Gauge struct{ f *family }

func (r *Registry) Gauge(name, help string, labels ...string) *Gauge {
	return &Gauge{r.add(name, help, gaugeKind, nil, labels)}
}

func (g *Gauge) Set(v float64, labels ...string) {
	g.f.mu.Lock()
	g.f.get(labels).value = v
	g.f.mu.Unlock()
}

// Reset drops every series, so a scrape that sets gauges afresh does not
// keep label sets that went away.
func (g *Gauge) Reset() {
	g.f.mu.Lock()
	clear(g.f.series)
	g.f.mu.Unlock()
}

func (g *Gauge) Value(labels ...string) float64 {
	g.f.mu.Lock()
	defer g.f.mu.Unlock()
	if s := g.f.series[strings.Join(labels, "\xff")]; s != nil {
		return s.value
	}
	return 0
}

// Histogram counts observations into buckets.
type Histogram struct{ f *family }

// DurationBuckets suit model calls, tool calls and turns, in seconds.
var DurationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 900}

func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *Histogram {
	b := slices.Clone(buckets)
	slices.Sort(b)
	return &Histogram{r.add(name, help, histogramKind, b, labels)}
}

func (h *Histogram) Observe(v float64, labels ...string) {
	if math.IsNaN(v) {
		return
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	s := h.f.get(labels)
	s.sum += v
	s.count++
	for i, ub := range h.f.buckets {
		if v <= ub {
			s.counts[i]++
			return
		}
	}
}

// Count returns the number of observations (for tests).
func (h *Histogram) Count(labels ...string) uint64 {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	if s := h.f.series[strings.Join(labels, "\xff")]; s != nil {
		return s.count
	}
	return 0
}

// Write runs the scrape functions, then writes every family.
func (r *Registry) Write(ctx context.Context, w io.Writer) error {
	r.mu.Lock()
	scrapes := slices.Clone(r.scrapes)
	fams := slices.Clone(r.families)
	r.mu.Unlock()
	for _, fn := range scrapes {
		fn(ctx)
	}
	slices.SortFunc(fams, func(a, b *family) int { return strings.Compare(a.name, b.name) })
	bw := bufio.NewWriter(w)
	for _, f := range fams {
		f.write(bw)
	}
	return bw.Flush()
}

func (f *family) write(w *bufio.Writer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.series) == 0 && len(f.labels) > 0 {
		return
	}
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, escapeHelp(f.help), f.name, f.kind)
	if len(f.series) == 0 { // an unlabeled metric reads 0 before anything happens
		fmt.Fprintf(w, "%s 0\n", f.name)
		return
	}
	keys := make([]string, 0, len(f.series))
	for k := range f.series {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		s := f.series[k]
		if f.kind != histogramKind {
			fmt.Fprintf(w, "%s%s %s\n", f.name, labelText(f.labels, s.values, "", ""), num(s.value))
			continue
		}
		var cum uint64
		for i, ub := range f.buckets {
			cum += s.counts[i]
			fmt.Fprintf(w, "%s_bucket%s %d\n", f.name, labelText(f.labels, s.values, "le", num(ub)), cum)
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", f.name, labelText(f.labels, s.values, "le", "+Inf"), s.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", f.name, labelText(f.labels, s.values, "", ""), num(s.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", f.name, labelText(f.labels, s.values, "", ""), s.count)
	}
}

func labelText(names, values []string, extraName, extraValue string) string {
	if len(names) == 0 && extraName == "" {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n + `="` + escapeLabel(values[i]) + `"`)
	}
	if extraName != "" {
		if len(names) > 0 {
			b.WriteByte(',')
		}
		b.WriteString(extraName + `="` + extraValue + `"`)
	}
	b.WriteByte('}')
	return b.String()
}

var (
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }

func num(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatFloat(v, 'f', -1, 64) // whole numbers (timestamps, counts) without an exponent
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Handler serves the registry at GET.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = r.Write(req.Context(), w)
	})
}
