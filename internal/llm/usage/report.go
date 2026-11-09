package usage

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// Totals sums a group of calls.
type Totals struct {
	Calls int
	llm.Usage
	Cost     float64 // of the priced calls
	Unpriced int     // calls whose model has no known price
}

func (t *Totals) add(c Call, cost float64, priced bool) {
	t.Calls++
	t.Add(c.Usage)
	t.Cost += cost
	if !priced {
		t.Unpriced++
	}
}

// Summary groups calls by model and by agent.
type Summary struct {
	Total   Totals
	ByModel map[string]*Totals // "<provider>/<model>"
	ByAgent map[string]*Totals
}

// Summarize totals calls, pricing each with pr.
func Summarize(calls []Call, pr *Pricer) Summary {
	s := Summary{ByModel: map[string]*Totals{}, ByAgent: map[string]*Totals{}}
	for _, c := range calls {
		cost, ok := pr.Cost(c)
		s.Total.add(c, cost, ok)
		group(s.ByModel, c.Provider+"/"+c.Model).add(c, cost, ok)
		agent := c.Agent
		if agent == "" {
			agent = "(none)"
		}
		group(s.ByAgent, agent).add(c, cost, ok)
	}
	return s
}

func group(m map[string]*Totals, key string) *Totals {
	t, ok := m[key]
	if !ok {
		t = &Totals{}
		m[key] = t
	}
	return t
}

// Write prints the summary as two tables, most expensive first.
func (s Summary) Write(w io.Writer) {
	if s.Total.Calls == 0 {
		fmt.Fprintln(w, "no model calls recorded")
		return
	}
	// Numbers align right; labels are padded to one width so they read
	// left-aligned.
	width := len("MODEL")
	for _, g := range []map[string]*Totals{s.ByModel, s.ByAgent} {
		for k := range g {
			width = max(width, len(k))
		}
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	writeGroup(tw, width, "MODEL", s.ByModel)
	fmt.Fprintln(tw, "\t\t\t\t\t\t\t")
	writeGroup(tw, width, "AGENT", s.ByAgent)
	fmt.Fprintln(tw, "\t\t\t\t\t\t\t")
	writeRow(tw, width, "TOTAL", &s.Total)
	_ = tw.Flush()
	if s.Total.Unpriced > 0 {
		fmt.Fprintf(w, "\n%d call(s) used models with no known price (cost shown as ?); add them under pricing: in config.yaml.\n", s.Total.Unpriced)
	}
}

func writeGroup(tw *tabwriter.Writer, width int, title string, groups map[string]*Totals) {
	fmt.Fprintf(tw, "%-*s\tCALLS\tINPUT\tOUTPUT\tCACHE READ\tCACHE WRITE\tCOST\t\n", width, title)
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := groups[keys[i]], groups[keys[j]]
		if a.Cost != b.Cost {
			return a.Cost > b.Cost
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		writeRow(tw, width, k, groups[k])
	}
}

func writeRow(tw *tabwriter.Writer, width int, label string, t *Totals) {
	fmt.Fprintf(tw, "%-*s\t%d\t%s\t%s\t%s\t%s\t%s\t\n", width, label, t.Calls, Tokens(t.InputTokens), Tokens(t.OutputTokens),
		Tokens(t.CacheReadTokens), Tokens(t.CacheWriteTokens), t.CostString())
}

// CostString formats the cost, marking it when some calls had no price.
func (t *Totals) CostString() string {
	s := fmt.Sprintf("$%.2f", t.Cost)
	if t.Cost > 0 && t.Cost < 0.01 {
		s = fmt.Sprintf("$%.4f", t.Cost)
	}
	switch {
	case t.Unpriced == t.Calls && t.Calls > 0:
		return "?"
	case t.Unpriced > 0:
		return s + "+?"
	}
	return s
}

// Tokens formats a token count briefly: 950, 12.3k, 4.5M.
func Tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1e6)) + "M"
	case n >= 1000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1e3)) + "k"
	}
	return fmt.Sprint(n)
}

func trimZero(s string) string { return strings.TrimSuffix(s, ".0") }

// Brief is a few lines for a chat: totals and the cost per model.
func (s Summary) Brief() string {
	if s.Total.Calls == 0 {
		return "No model calls in this session yet."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d model call(s): %s in, %s out, %s cache read, %s cache write. Cost %s.",
		s.Total.Calls, Tokens(s.Total.InputTokens), Tokens(s.Total.OutputTokens),
		Tokens(s.Total.CacheReadTokens), Tokens(s.Total.CacheWriteTokens), s.Total.CostString())
	keys := make([]string, 0, len(s.ByModel))
	for k := range s.ByModel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t := s.ByModel[k]
		fmt.Fprintf(&b, "\n%s: %d call(s), %s", k, t.Calls, t.CostString())
	}
	return b.String()
}
