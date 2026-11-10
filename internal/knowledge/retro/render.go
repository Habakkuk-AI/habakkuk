package retro

import (
	"fmt"
	"strings"
	"time"
)

// maxEvidenceText caps the evidence as the model reads it.
const maxEvidenceText = 40000

// Text renders the evidence for the model and for people: one section per
// source, each line ending with the ids it rests on.
func (e *Evidence) Text() string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("Agent %s, %s to %s.", e.Agent, e.From.Format("2006-01-02 15:04"), e.To.Format("2006-01-02 15:04"))

	t := e.Turns
	line("\n## Turns")
	line("%d sessions, %d turns: %d done, %d failed. Time to finish a turn: median %s, 95th percentile %s.",
		e.Sessions, t.Count, t.Done, t.Failed, ms(t.P50MS), ms(t.P95MS))
	for _, s := range t.Slow {
		refs := s.Ref
		if s.Session != "" {
			refs += ", session:" + s.Session
		}
		line("- slow: %s, first message %q [%s]", ms(s.MS), s.First, refs)
	}

	if len(e.Jobs) > 0 {
		line("\n## Jobs by kind")
		for _, j := range e.Jobs {
			line("- %s: %d, failed %d, retried %d, interrupted %d, canceled %d", j.Kind, j.Total, j.Failed, j.Retried, j.Interrupted, j.Canceled)
		}
	}
	if len(e.Problems) > 0 {
		line("\n## Jobs that went wrong")
		for _, p := range e.Problems {
			s := fmt.Sprintf("- %s %s after %d attempts", p.Kind, p.What, p.Attempts)
			if p.Error != "" {
				s += ": " + p.Error
			}
			refs := p.Ref
			if p.Session != "" {
				refs += ", session:" + p.Session
			}
			line("%s [%s]", s, refs)
		}
	}
	if len(e.ToolErrors) > 0 {
		line("\n## Tool errors")
		for _, te := range e.ToolErrors {
			line("- %s: %d errors", te.Tool, te.Count)
			for _, s := range te.Samples {
				line("  - %s [%s]", s.Text, s.Ref)
			}
		}
	}
	a := e.Approvals
	if a.ReviewerApproved+a.ReviewerDenied+a.ReviewerUnsure+a.UserDeclined+a.NoApprover+a.GuestDenied > 0 {
		line("\n## Approvals")
		line("Reviewer approved %d, reviewer denied %d, reviewer unsure with no human to ask %d, user declined or did not answer %d, nobody attending the run %d, refused to a guest %d.",
			a.ReviewerApproved, a.ReviewerDenied, a.ReviewerUnsure, a.UserDeclined, a.NoApprover, a.GuestDenied)
		for _, s := range a.Samples {
			line("- %s [%s]", s.Text, s.Ref)
		}
	}
	if len(e.Fallbacks) > 0 {
		line("\n## Model fallbacks")
		for _, f := range e.Fallbacks {
			s := fmt.Sprintf("- %dx ", f.Count)
			if f.From != "" {
				s += f.From + " -> "
			}
			s += f.To
			if f.Reason != "" {
				s += " (" + f.Reason + ")"
			}
			line("%s [%s]", s, strings.Join(f.Refs, ", "))
		}
	}
	if len(e.Cost) > 0 {
		line("\n## Cost by model")
		for _, c := range e.Cost {
			s := fmt.Sprintf("- %s: %d calls, %d input and %d output tokens, $%.2f", c.Model, c.Calls, c.Input, c.Output, c.USD)
			if c.Unpriced > 0 {
				s += fmt.Sprintf(" (%d calls unpriced)", c.Unpriced)
			}
			line("%s", s)
		}
	}
	if len(e.Schedules) > 0 || e.SchedulesOK > 0 {
		line("\n## Schedules")
		if e.SchedulesOK > 0 {
			line("%d schedules ran without trouble.", e.SchedulesOK)
		}
		for _, s := range e.Schedules {
			state := "on"
			if !s.Enabled {
				state = "paused"
			}
			line("- %q (%s, %s): %d runs, %d errors, %d skipped by a spending limit, %d failed turns, %d approvals nobody could give, fail streak %d, last status %q [%s]",
				s.Name, s.Spec, state, s.Runs, s.Errors, s.BudgetSkips, s.TurnsFailed, s.Denied, s.FailStreak, s.LastStatus,
				strings.Join(append([]string{s.Ref}, s.Refs...), ", "))
		}
	}
	if e.CardsDone > 0 || len(e.Stalled) > 0 {
		line("\n## Board and delegated tasks")
		line("%d cards finished in the period.", e.CardsDone)
		for _, s := range e.Stalled {
			line("- stalled %s %q: %s, no progress for %d days [%s]", s.What, s.Title, s.Status, s.Days, s.Ref)
		}
	}
	if len(e.Deliveries) > 0 {
		line("\n## Deliveries that failed or kept retrying")
		for _, d := range e.Deliveries {
			s := fmt.Sprintf("- to %s: %s after %d attempts", d.Target, d.Status, d.Attempts)
			if d.Error != "" {
				s += ": " + d.Error
			}
			line("%s [%s]", s, d.Ref)
		}
	}
	if len(e.Corrections) > 0 {
		line("\n## Signs the user was not happy")
		for _, c := range e.Corrections {
			line("- %s: %q [%s]", c.Kind, c.Text, c.Ref)
		}
	}
	if len(e.Changes) > 0 {
		line("\n## Memory and skill changes")
		for _, c := range e.Changes {
			line("- %s %s %s by %s [%s]", c.Kind, c.Name, c.Op, c.By, c.Ref)
		}
	}
	return cutText(b.String(), maxEvidenceText)
}

// ms formats milliseconds for people.
func ms(n int64) string {
	if n <= 0 {
		return "-"
	}
	d := time.Duration(n) * time.Millisecond
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", n)
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}
