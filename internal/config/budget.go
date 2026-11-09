package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Budget caps what model calls may cost, in US dollars, priced like
// `habakkuk usage`. The top-level budget covers the whole install; an
// agent's covers the calls that agent makes. A limit left out is no limit.
// Days and months start at 00:00 in the configured timezone.
type Budget struct {
	Daily   Money `yaml:"daily,omitempty"`
	Monthly Money `yaml:"monthly,omitempty"`
	// PerTurn caps one turn: every model call it makes, including its tool
	// loop, compaction, its title and the sub-agents it delegates to.
	PerTurn Money `yaml:"per_turn,omitempty"`
	// UnknownPrice (top level only) is what calls to a model with no known
	// price do: allow (default) counts them as $0, block refuses them while
	// any limit applies.
	UnknownPrice string `yaml:"unknown_price,omitempty"`
}

// Values of Budget.UnknownPrice.
const (
	UnknownPriceAllow = "allow"
	UnknownPriceBlock = "block"
)

// Money is an amount in US dollars, written as a number (5, 2.5) or a
// string ("5.00", "$5").
type Money struct {
	Dollars float64
	set     bool   // the config gave a value
	bad     string // the value as written, when it is not an amount
}

// Dollar returns an amount that was set, for code and tests.
func Dollar(v float64) Money { return Money{Dollars: v, set: true} }

// Set reports whether the config gave an amount.
func (m Money) Set() bool { return m.set && m.bad == "" }

// IsZero lets yaml's omitempty leave out an unset amount.
func (m Money) IsZero() bool { return !m.set }

func (m *Money) UnmarshalYAML(n *yaml.Node) error {
	*m = Money{set: true}
	if n.Kind != yaml.ScalarNode {
		m.bad = "(not a single value)"
		return nil
	}
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(n.Value), "$"))
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		m.bad = strconv.Quote(n.Value)
		return nil
	}
	m.Dollars = v
	return nil
}

func (m Money) MarshalYAML() (any, error) {
	if m.bad != "" {
		return m.bad, nil
	}
	return m.Dollars, nil
}

func (m Money) String() string { return fmt.Sprintf("$%.2f", m.Dollars) }

// BudgetFor is an agent's own budget, or nil.
func (c *Config) BudgetFor(agent string) *Budget {
	if a := c.Agents[agent]; a != nil {
		return a.Budget
	}
	return nil
}

// BlockUnpriced reports whether budget.unknown_price is block.
func (c *Config) BlockUnpriced() bool {
	return c.Budget != nil && c.Budget.UnknownPrice == UnknownPriceBlock
}

func (c *Config) validateBudget() []error {
	var errs []error
	check := func(where string, b *Budget, top bool) {
		if b == nil {
			return
		}
		for _, f := range []struct {
			name string
			m    Money
		}{{"daily", b.Daily}, {"monthly", b.Monthly}, {"per_turn", b.PerTurn}} {
			switch {
			case !f.m.set:
			case f.m.bad != "":
				errs = append(errs, fmt.Errorf("%s.%s: %s is not an amount in dollars (write 5 or \"5.00\")", where, f.name, f.m.bad))
			case f.m.Dollars <= 0:
				errs = append(errs, fmt.Errorf("%s.%s: must be more than 0 (leave it out for no limit)", where, f.name))
			}
		}
		switch {
		case !top && b.UnknownPrice != "":
			errs = append(errs, fmt.Errorf("%s.unknown_price: set it in the top-level budget only", where))
		case b.UnknownPrice != "" && b.UnknownPrice != UnknownPriceAllow && b.UnknownPrice != UnknownPriceBlock:
			errs = append(errs, fmt.Errorf("%s.unknown_price: %q must be allow or block", where, b.UnknownPrice))
		}
	}
	check("budget", c.Budget, true)
	for name, a := range c.Agents {
		if a != nil {
			check("agent "+name+": budget", a.Budget, false)
		}
	}
	return errs
}
