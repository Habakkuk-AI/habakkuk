package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// withBudget parses the template with a top-level budget section and one
// for the agent.
func withBudget(top, agent string) (*Config, error) {
	text := Template
	if agent != "" {
		text = strings.Replace(text, "    effort: high\n", "    effort: high\n    budget:\n"+indent(agent, "      "), 1)
	}
	if top != "" {
		text += "budget:\n" + indent(top, "  ")
	}
	return Parse([]byte(text))
}

func indent(s, pre string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		b.WriteString(pre + strings.TrimSpace(l) + "\n")
	}
	return b.String()
}

func TestBudgetAmounts(t *testing.T) {
	cfg, err := withBudget("daily: 5\nmonthly: \"100.00\"\nper_turn: 1.5\nunknown_price: block", "daily: \"$2\"\nper_turn: 0.25")
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Budget
	if b.Daily.Dollars != 5 || b.Monthly.Dollars != 100 || b.PerTurn.Dollars != 1.5 || !b.Daily.Set() {
		t.Fatalf("top budget %+v", b)
	}
	if !cfg.BlockUnpriced() {
		t.Fatal("unknown_price: block not read")
	}
	a := cfg.BudgetFor("habakkuk")
	if a.Daily.Dollars != 2 || a.PerTurn.Dollars != 0.25 || a.Monthly.Set() {
		t.Fatalf("agent budget %+v", a)
	}
	if cfg.BudgetFor("nobody") != nil {
		t.Fatal("budget for an unknown agent")
	}
	// No budget section: no limits, unpriced models allowed.
	plain, err := Parse([]byte(Template))
	if err != nil || plain.Budget != nil || plain.BlockUnpriced() {
		t.Fatalf("template: %v %+v", err, plain.Budget)
	}
}

func TestBudgetValidation(t *testing.T) {
	for _, tc := range []struct {
		top, agent, want string
	}{
		{top: "daily: five", want: `budget.daily: "five" is not an amount in dollars`},
		{top: "monthly: 0", want: "budget.monthly: must be more than 0"},
		{top: "per_turn: -1", want: "budget.per_turn: must be more than 0"},
		{top: "daily: [1, 2]", want: "budget.daily: (not a single value)"},
		{top: "daily: .nan", want: "is not an amount in dollars"},
		{top: "unknown_price: maybe", want: `budget.unknown_price: "maybe" must be allow or block`},
		{agent: "weekly: 5", want: "field weekly not found"},
		{agent: "daily: \"5,00\"", want: `agent habakkuk: budget.daily: "5,00" is not an amount`},
		{agent: "unknown_price: block", want: "agent habakkuk: budget.unknown_price: set it in the top-level budget only"},
	} {
		_, err := withBudget(tc.top, tc.agent)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("top %q agent %q: err = %v, want %q", tc.top, tc.agent, err, tc.want)
		}
	}
	// A per-agent limit above the install's is allowed.
	if _, err := withBudget("daily: 1", "daily: 50"); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetMarshalsAsNumbers(t *testing.T) {
	cfg, err := withBudget("daily: \"5.50\"", "")
	if err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(cfg.Budget)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "daily: 5.5" {
		t.Fatalf("marshaled %q", out)
	}
}
