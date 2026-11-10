package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Retro configures retrospectives: an agent looks back over a period of
// its own work, finds what went well and what went wrong from Habakkuk's
// own records, and proposes changes. A nil section never runs one on a
// timer; `hk retro` and /retro still work, with the defaults.
type Retro struct {
	// Every is off (default), daily, weekly, or a cron expression.
	Every string `yaml:"every"`
	// At is the local time for daily ("17:00") and weekly ("fri 17:00")
	// retros. Default 17:00, and "fri 17:00" for weekly.
	At string `yaml:"at"`
	// Agents are the agents that get retros; empty means every agent that
	// had turns in the period.
	Agents []string `yaml:"agents"`
	// Model is "<provider>/<model>" tried first; default the agent's own
	// model chain. A strong model is right here.
	Model string `yaml:"model"`
	// Apply is propose (default: nothing changes until approved) or safe
	// (skill and memory changes are applied at once, each undoable).
	Apply string `yaml:"apply"`
	// Deliver is where the summary goes: a `hk send` target
	// (CHANNEL:CHAT[:THREAD]), none for the report only, or empty for the
	// agent's home chat.
	Deliver string `yaml:"deliver"`
}

// Retro apply modes.
const (
	RetroPropose = "propose"
	RetroSafe    = "safe"
)

// RetroDeliverNone keeps a retro to its report.
const RetroDeliverNone = "none"

// weekdays are the three-letter day names cron uses, by number.
var weekdays = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// weekday reads a day name: its first three letters, alone or followed by
// the rest of the name ("mon", "monday", "Tuesday").
func weekday(s string) (int, bool) {
	if len(s) < 3 {
		return 0, false
	}
	n, ok := weekdays[s[:3]]
	if !ok || (len(s) > 3 && !strings.HasSuffix(s, "day")) {
		return 0, false
	}
	return n, true
}

// retroCron parses cron expressions the way schedules do.
var retroCron = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

func (c *Config) applyRetroDefaults() {
	r := c.Retro
	if r == nil {
		return
	}
	r.Every = strings.ToLower(strings.TrimSpace(r.Every))
	if r.Every == "" {
		r.Every = "off"
	}
	if r.Apply == "" {
		r.Apply = RetroPropose
	}
}

// RetroSpec is the cron expression retros run on, in the config's
// timezone; ok is false when they are off.
func (c *Config) RetroSpec() (spec string, ok bool) {
	r := c.Retro
	if r == nil {
		return "", false
	}
	spec, err := r.spec()
	return spec, err == nil && spec != ""
}

// RetroApply is the apply mode, propose unless the config says safe.
func (c *Config) RetroApply() string {
	if c.Retro != nil && c.Retro.Apply == RetroSafe {
		return RetroSafe
	}
	return RetroPropose
}

// spec turns every and at into a cron expression; "" is off.
func (r *Retro) spec() (string, error) {
	switch r.Every {
	case "", "off":
		if r.At != "" {
			return "", fmt.Errorf("at %q needs every: daily or weekly", r.At)
		}
		return "", nil
	case "daily", "weekly":
		day, hour, minute, err := parseRetroAt(r.At)
		if err != nil {
			return "", err
		}
		if r.Every == "daily" {
			if day >= 0 {
				return "", fmt.Errorf("at %q names a weekday; a daily retro takes HH:MM", r.At)
			}
			return fmt.Sprintf("%d %d * * *", minute, hour), nil
		}
		if day < 0 {
			day = weekdays["fri"]
		}
		return fmt.Sprintf("%d %d * * %d", minute, hour, day), nil
	}
	if r.At != "" {
		return "", fmt.Errorf("at %q is for every: daily or weekly; a cron expression carries its own time", r.At)
	}
	sched, err := retroCron.Parse(r.Every)
	if err != nil {
		return "", fmt.Errorf("every %q must be off, daily, weekly or a cron expression: %w", r.Every, err)
	}
	// Retros look back over the time since the last one: more than one an
	// hour leaves nothing to look at.
	t := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	for range 200 {
		next := sched.Next(t)
		if next.IsZero() {
			break
		}
		if after := sched.Next(next); !after.IsZero() && after.Sub(next) < time.Hour {
			return "", fmt.Errorf("every %q runs more than once an hour; a retro needs at least an hour to look back on", r.Every)
		}
		t = next
	}
	return r.Every, nil
}

// parseRetroAt reads "HH:MM" or "DAY HH:MM"; day is -1 when not given.
// Empty is 17:00.
func parseRetroAt(at string) (day, hour, minute int, err error) {
	at = strings.ToLower(strings.TrimSpace(at))
	day = -1
	if at == "" {
		return day, 17, 0, nil
	}
	if d, rest, ok := strings.Cut(at, " "); ok {
		n, known := weekday(d)
		if !known {
			return 0, 0, 0, fmt.Errorf("at %q: %q is not a weekday (mon, tue, ...)", at, d)
		}
		day, at = n, strings.TrimSpace(rest)
	}
	t, perr := time.Parse("15:04", at)
	if perr != nil {
		return 0, 0, 0, fmt.Errorf("at %q: want HH:MM, or a weekday and HH:MM such as \"fri 17:00\"", at)
	}
	return day, t.Hour(), t.Minute(), nil
}

func (c *Config) validateRetro() []error {
	r := c.Retro
	if r == nil {
		return nil
	}
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("retro: "+format, args...)) }
	if _, err := r.spec(); err != nil {
		bad("%v", err)
	}
	for _, name := range r.Agents {
		if _, ok := c.Agents[name]; !ok {
			bad("agent %q is not configured", name)
		}
	}
	if r.Model != "" {
		if ref, err := ParseModelRef(r.Model); err != nil {
			bad("model: %v", err)
		} else if _, ok := c.Providers[ref.Provider]; !ok {
			bad("model: unknown provider %q", ref.Provider)
		}
	}
	switch r.Apply {
	case RetroPropose, RetroSafe:
	default:
		bad("apply %q must be propose or safe", r.Apply)
	}
	switch d := r.Deliver; d {
	case "", RetroDeliverNone:
	default:
		account, ok := HomeChannel(d)
		switch {
		case !ok:
			bad("deliver %q: want CHANNEL:CHAT or CHANNEL:CHAT:THREAD, or none", d)
		case c.Channels[account] == nil:
			bad("deliver %q names channel %q, which is not configured", d, account)
		}
	}
	return errs
}

// RetroDeliver is where an agent's retro summaries go: a send target, or
// "" for none.
func (c *Config) RetroDeliver(agent string) string {
	d := ""
	if c.Retro != nil {
		d = c.Retro.Deliver
	}
	switch d {
	case RetroDeliverNone:
		return ""
	case "":
		if a := c.Agents[agent]; a != nil {
			return a.Home
		}
		return ""
	}
	return d
}
