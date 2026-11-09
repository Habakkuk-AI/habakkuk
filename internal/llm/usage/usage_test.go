package usage

import (
	"bytes"
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestPriceMatching(t *testing.T) {
	pr := &Pricer{}
	for _, tc := range []struct {
		model string
		want  string // key expected to match, "" for none
	}{
		{"claude-opus-5-5", "claude-opus-5-5"},
		{"claude-opus-5-5-20261001", "claude-opus-5-5"},
		{"claude-opus-5-20260301", "claude-opus-5"},
		{"anthropic/claude-sonnet-5-5", "claude-sonnet-5-5"},
		{"us.anthropic.claude-haiku-4-5-20251001-v1:0", "claude-haiku-4-5"},
		{"claude-sonnet-4.5", "claude-sonnet-4-5"},
		{"gpt-5.4-mini", "gpt-5.4-mini"},
		{"gpt-5.4-mini-2026-03-01", "gpt-5.4-mini"},
		{"gpt-5-mini", "gpt-5-mini"},
		{"gpt-5.3-codex", "gpt-5.3-codex"},
		{"models/gemini-3.1-pro-preview", "gemini-3.1-pro"},
		{"gemini-2.5-flash-lite", "gemini-2.5-flash-lite"},
		{"x-ai/grok-4.20-0309-reasoning", "grok-4.20"},
		{"deepseek-flash", "deepseek-flash"},
		{"deepseek/deepseek-v4-flash", "deepseek-v4-flash"}, // the older name the API still accepts
		{"llama-4-scout", ""},
		{"gpt-50", ""},
	} {
		got, ok := pr.Lookup("p", tc.model)
		want, wantOK := Prices[tc.want]
		if tc.want != "" && !wantOK {
			t.Fatalf("%s: no price %q in the table", tc.model, tc.want)
		}
		if ok != wantOK || ok && got.Input != want.Input || ok && got.Output != want.Output {
			t.Errorf("%s: got %+v %v, want %s", tc.model, got, ok, tc.want)
		}
	}
}

func TestCost(t *testing.T) {
	pr := &Pricer{
		Overrides:    map[string]*config.Price{"local/llama-4": {Input: 0, Output: 0}, "claude-opus-5-5": {Input: 1, Output: 2, CacheRead: 0.5, CacheWrite: 3}},
		Subscription: map[string]bool{"chatgpt": true},
	}
	u := llm.Usage{InputTokens: 1_000_000, OutputTokens: 500_000, CacheReadTokens: 2_000_000, CacheWriteTokens: 100_000}
	if c, ok := pr.Cost(Call{Provider: "anthropic", Model: "claude-opus-5-5", Usage: u}); !ok || !near(c, 1+1+1+0.3) {
		t.Errorf("override: %v %v", c, ok)
	}
	if c, ok := pr.Cost(Call{Provider: "anthropic", Model: "claude-sonnet-5-5", Usage: u}); !ok || !near(c, 2+5+0.2+0.25) {
		t.Errorf("built-in: %v %v", c, ok)
	}
	if c, ok := pr.Cost(Call{Provider: "chatgpt", Model: "gpt-5.5", Usage: u}); !ok || c != 0 {
		t.Errorf("subscription: %v %v", c, ok)
	}
	if _, ok := pr.Cost(Call{Provider: "local", Model: "llama-4", Usage: u}); !ok {
		t.Error("provider/model override not found")
	}
	if _, ok := pr.Cost(Call{Provider: "x", Model: "mystery", Usage: u}); ok {
		t.Error("unknown model priced")
	}
	// Long prompts: Haiku 5.5 bills every token at the higher rate past 100k.
	short := llm.Usage{InputTokens: 50_000, OutputTokens: 1000}
	long := llm.Usage{InputTokens: 10_000, CacheReadTokens: 100_000, OutputTokens: 1000}
	cs, _ := pr.Cost(Call{Model: "claude-haiku-5-5", Usage: short})
	cl, _ := pr.Cost(Call{Model: "claude-haiku-5-5", Usage: long})
	if !near(cs, (50_000*0.10+1000*0.50)/1e6) || !near(cl, (10_000*0.50+100_000*0.05+1000*2.50)/1e6) {
		t.Errorf("tiers: short %v long %v", cs, cl)
	}
}

func TestStoreAndSummary(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	old := time.Now().AddDate(0, 0, -40)
	for _, c := range []Call{
		{At: old, Provider: "anthropic", Model: "claude-opus-5-5", Agent: "habakkuk", Session: "s0", Usage: llm.Usage{InputTokens: 1_000_000}},
		{Provider: "anthropic", Model: "claude-opus-5-5", Agent: "habakkuk", Session: "s1", Usage: llm.Usage{InputTokens: 1_000_000, OutputTokens: 100_000}},
		{Provider: "anthropic", Model: "claude-opus-5-5", Agent: "ops", Session: "s2", Usage: llm.Usage{InputTokens: 500_000}},
		{Provider: "groq", Model: "llama-4", Agent: "habakkuk", Session: "s1", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}},
	} {
		if err := st.Record(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	since := time.Now().AddDate(0, 0, -30)
	calls, err := st.Calls(ctx, Filter{Since: since})
	if err != nil || len(calls) != 3 {
		t.Fatalf("calls %d, %v", len(calls), err)
	}
	s := Summarize(calls, &Pricer{})
	if s.Total.Calls != 3 || s.Total.InputTokens != 1_500_010 || s.Total.Unpriced != 1 {
		t.Errorf("total %+v", s.Total)
	}
	opus := s.ByModel["anthropic/claude-opus-5-5"]
	if opus.Calls != 2 || !near(opus.Cost, 4+2+2) {
		t.Errorf("opus %+v", opus)
	}
	if f := s.ByAgent["habakkuk"]; f.Calls != 2 || f.Unpriced != 1 || f.CostString() != "$6.00+?" {
		t.Errorf("habakkuk %+v %s", f, f.CostString())
	}
	if s.ByAgent["ops"].Calls != 1 {
		t.Errorf("ops %+v", s.ByAgent["ops"])
	}
	var out bytes.Buffer
	s.Write(&out)
	for _, want := range []string{"anthropic/claude-opus-5-5", "groq/llama-4", "habakkuk", "ops", "TOTAL", "1.5M", "?"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}

	agentCalls, _ := st.Calls(ctx, Filter{Since: since, Agent: "ops"})
	sessCalls, _ := st.Calls(ctx, Filter{Session: "s1"})
	if len(agentCalls) != 1 || len(sessCalls) != 2 {
		t.Errorf("filters: agent %d session %d", len(agentCalls), len(sessCalls))
	}
	if b := Summarize(sessCalls, &Pricer{}).Brief(); !strings.Contains(b, "2 model call(s)") || !strings.Contains(b, "groq/llama-4: 1 call(s), ?") {
		t.Errorf("brief: %s", b)
	}

	n, err := st.Prune(ctx, since)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
}

func TestSessionModels(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if m, _ := st.SessionModel(ctx, "s1"); m != "" {
		t.Fatalf("unset session has model %q", m)
	}
	st.SetSessionModel(ctx, "s1", "groq/llama-4")
	st.SetSessionModel(ctx, "s1", "openai/gpt-5.5")
	if m, _ := st.SessionModel(ctx, "s1"); m != "openai/gpt-5.5" {
		t.Fatalf("model %q", m)
	}
	st.SetSessionModel(ctx, "s1", "")
	if m, _ := st.SessionModel(ctx, "s1"); m != "" {
		t.Fatalf("reset left %q", m)
	}
}

func TestTokens(t *testing.T) {
	for n, want := range map[int64]string{950: "950", 12_300: "12.3k", 4_000: "4k", 4_500_000: "4.5M"} {
		if got := Tokens(n); got != want {
			t.Errorf("%d: %s", n, got)
		}
	}
}
