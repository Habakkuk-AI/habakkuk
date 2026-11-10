package budget

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// Spend is one period's spend against its limit.
type Spend struct {
	Spent   float64 `json:"spent"`
	Limit   float64 `json:"limit,omitempty"`   // 0: no limit
	Percent float64 `json:"percent,omitempty"` // of the limit
	Resets  string  `json:"resets"`            // RFC 3339, when the period ends
}

// Scope is the install's or one agent's spend and limits.
type Scope struct {
	Agent   string  `json:"agent,omitempty"` // "" is the whole install
	Today   Spend   `json:"today"`
	Month   Spend   `json:"month"`
	PerTurn float64 `json:"per_turn,omitempty"`
	Paused  string  `json:"paused,omitempty"` // why calls are refused now
}

// Report is the spend so far against every limit.
type Report struct {
	Timezone     string  `json:"timezone"`
	Day          string  `json:"day"`   // today, YYYY-MM-DD
	Month        string  `json:"month"` // YYYY-MM
	UnknownPrice string  `json:"unknown_price"`
	Install      Scope   `json:"install"`
	Agents       []Scope `json:"agents"`
}

// Limited reports whether any limit is set.
func (r Report) Limited() bool {
	if r.Install.limited() {
		return true
	}
	for _, a := range r.Agents {
		if a.limited() {
			return true
		}
	}
	return false
}

func (s Scope) limited() bool { return s.Today.Limit > 0 || s.Month.Limit > 0 || s.PerTurn > 0 }

// Report reads the spend and limits now, for the install and for agents
// (the configured ones; every agent that spent this month is added).
func (t *Tracker) Report(agents []string) Report {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.roll(t.now())
	r := Report{Timezone: t.loc.String(), Day: t.day.Format(time.DateOnly), Month: t.month.Format("2006-01"),
		UnknownPrice: "allow"}
	if t.blockUnpriced {
		r.UnknownPrice = "block"
	}
	r.Install = t.scope("", t.global)
	seen := map[string]bool{}
	for _, a := range agents {
		seen[a] = true
	}
	for a := range t.monthly {
		if a != "" && !seen[a] {
			seen[a] = true
			agents = append(agents, a)
		}
	}
	sort.Strings(agents)
	for _, a := range agents {
		r.Agents = append(r.Agents, t.scope(a, t.agents[a]))
	}
	return r
}

// scope fills one Scope. The caller holds mu.
func (t *Tracker) scope(agent string, l Limits) Scope {
	spend := func(spent, limit float64, end time.Time) Spend {
		s := Spend{Spent: spent, Limit: limit, Resets: end.Format(time.RFC3339)}
		if limit > 0 {
			s.Percent = 100 * spent / limit
		}
		return s
	}
	s := Scope{Agent: agent, PerTurn: l.PerTurn,
		Today: spend(t.daily[agent], l.Daily, t.day.AddDate(0, 0, 1)),
		Month: spend(t.monthly[agent], l.Monthly, t.month.AddDate(0, 1, 0))}
	if agent == "" {
		// The install's pause is any of its own limits.
		for _, x := range t.applicable("") {
			if x.spent >= x.amount {
				s.Paused = t.pausedMessage(x)
				break
			}
		}
	} else if be := t.blocked(agent); be != nil {
		s.Paused = be.Message
	}
	return s
}

func (s Spend) String() string {
	if s.Limit == 0 {
		return Dollars(s.Spent)
	}
	return fmt.Sprintf("%s / %s (%.0f%%)", Dollars(s.Spent), Dollars(s.Limit), s.Percent)
}

// Write prints the report as a table, for hk usage.
func (r Report) Write(w io.Writer) {
	fmt.Fprintf(w, "Spending limits (days and months in %s)\n\n", r.Timezone)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SCOPE\tTODAY\tTHIS MONTH\tPER TURN\t")
	row := func(name string, s Scope) {
		per := "-"
		if s.PerTurn > 0 {
			per = Dollars(s.PerTurn)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t\n", name, s.Today, s.Month, per)
	}
	row("install", r.Install)
	for _, a := range r.Agents {
		row("agent "+a.Agent, a)
	}
	_ = tw.Flush()
	var paused []string
	if r.Install.Paused != "" {
		paused = append(paused, r.Install.Paused)
	}
	for _, a := range r.Agents {
		if a.Paused != "" && a.Paused != r.Install.Paused {
			paused = append(paused, a.Paused)
		}
	}
	for _, p := range paused {
		fmt.Fprintln(w, "\n"+p)
	}
	if !r.Limited() {
		fmt.Fprintln(w, "\nNo spending limits are set; add them under budget: in config.yaml.")
	}
}

// Brief is a few lines for a chat (/budget): agent's spend and limits, and
// the install's.
func (r Report) Brief(agent string) string {
	var b strings.Builder
	line := func(title string, s Scope) {
		fmt.Fprintf(&b, "%s: today %s, this month %s", title, s.Today, s.Month)
		if s.PerTurn > 0 {
			fmt.Fprintf(&b, ", %s per turn", Dollars(s.PerTurn))
		}
		b.WriteString(".\n")
	}
	var own *Scope
	for i := range r.Agents {
		if r.Agents[i].Agent == agent {
			own = &r.Agents[i]
		}
	}
	if own == nil {
		own = &Scope{Agent: agent}
	}
	line("Agent "+agent, *own)
	line("Whole install", r.Install)
	fmt.Fprintf(&b, "Days and months reset at 00:00 %s.", r.Timezone)
	switch {
	case r.Install.Paused != "":
		b.WriteString("\n" + r.Install.Paused)
	case own.Paused != "":
		b.WriteString("\n" + own.Paused)
	case !r.Limited():
		b.WriteString("\nNo spending limits are set; add them under budget: in config.yaml.")
	}
	return b.String()
}
