package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/budget"
	"github.com/habakkuk-ai/habakkuk/internal/llm/usage"
)

func TestDashBudget(t *testing.T) {
	r := newDashRig(t)
	if code, _ := r.do(t, "GET", "/api/budget", token, "", ""); code != http.StatusNotImplemented {
		t.Fatalf("without limits wired: %d", code)
	}
	cfg := &config.Config{Timezone: "America/Toronto", Budget: &config.Budget{Daily: config.Dollar(10)},
		Agents:  map[string]*config.Agent{"habakkuk": {Budget: &config.Budget{Monthly: config.Dollar(40), PerTurn: config.Dollar(1)}}, "ops": {}},
		Pricing: map[string]*config.Price{"m": {Input: 1}}}
	tr := budget.New(cfg, usage.NewPricer(cfg), quiet)
	tr.Record(llm.WithCaller(context.Background(), llm.Caller{Agent: "habakkuk"}), llm.Target{Endpoint: "fake", Model: "m"},
		llm.Usage{InputTokens: 8_500_000})
	r.s.Dash.Budget = func() budget.Report { return tr.Report([]string{"habakkuk", "ops"}) }

	out := r.get(t, "/api/budget")
	if out["timezone"] != "America/Toronto" || out["unknown_price"] != "allow" {
		t.Fatalf("report: %v", out)
	}
	install := out["install"].(map[string]any)
	today := install["today"].(map[string]any)
	if today["spent"] != 8.5 || today["limit"] != float64(10) || today["percent"] != float64(85) || !strings.Contains(today["resets"].(string), "T00:00:00-0") {
		t.Fatalf("install today: %v", today)
	}
	agents := list(out, "agents")
	if len(agents) != 2 {
		t.Fatalf("agents: %v", agents)
	}
	own := agents[0].(map[string]any)
	if own["agent"] != "habakkuk" || own["per_turn"] != float64(1) || own["month"].(map[string]any)["limit"] != float64(40) {
		t.Fatalf("habakkuk: %v", own)
	}
	if one := list(r.get(t, "/api/budget?agent=ops"), "agents"); len(one) != 1 || one[0].(map[string]any)["agent"] != "ops" {
		t.Fatalf("filtered: %v", one)
	}
}
