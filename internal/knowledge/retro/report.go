package retro

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// Markdown is a retro's readable report.
func Markdown(ret *Retro, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("# Retro: %s, %s to %s", ret.Agent, ret.From.In(loc).Format("2006-01-02 15:04"), ret.To.In(loc).Format("2006-01-02 15:04"))
	line("")
	line("Retro `%s`.", ret.ID)
	line("")
	line("## Summary")
	line("")
	if ret.Plan.Quiet() {
		line("Nothing notable happened.")
	} else {
		line("%s", ret.Plan.Summary)
	}
	if len(ret.Plan.WentWell) > 0 {
		line("")
		line("## What went well")
		line("")
		for _, p := range ret.Plan.WentWell {
			line("- %s (%s)", p.What, refList(p.Evidence))
		}
	}
	if len(ret.Plan.WentBadly) > 0 {
		line("")
		line("## What went badly")
		line("")
		for _, p := range ret.Plan.WentBadly {
			s := "- " + p.What
			if p.Cause != "" {
				s += " Likely cause: " + p.Cause
			}
			line("%s (%s)", s, refList(p.Evidence))
		}
	}
	if len(ret.Actions) > 0 {
		line("")
		line("## Proposals")
		line("")
		line("Approve with `habakkuk retro apply %s N`, or `/retro apply N` in a chat; reject with `reject`. Skill and memory changes can also be applied on their own with `retro: {apply: safe}`. SOUL.md, config, schedule changes and questions always wait for you.", ret.ID)
		for _, a := range ret.Actions {
			line("")
			line("### %d. %s `%s` [%s]", a.N, a.Kind, a.Target, a.Status)
			line("")
			if a.Description != "" {
				line("Description: %s", a.Description)
				line("")
			}
			if a.Find != "" {
				line("Replace:")
				line("")
				line("%s", quote(a.Find))
				line("")
				line("With:")
			} else {
				line("Change:")
			}
			line("")
			line("%s", quote(a.Change))
			line("")
			line("- Why: %s", a.Why)
			if a.Expected != "" {
				line("- Expected: %s", a.Expected)
			}
			line("- Risk: %s", a.Risk)
			if len(a.Evidence) > 0 {
				line("- Evidence: %s", refList(a.Evidence))
			}
			if a.Result != "" {
				line("- Result: %s", oneLine(a.Result))
			}
		}
	}
	if len(ret.Plan.FollowUp) > 0 {
		line("")
		line("## Did the last retro's changes help?")
		line("")
		for _, f := range ret.Plan.FollowUp {
			s := fmt.Sprintf("- Action %d of `%s`: %s.", f.Action, f.Retro, strings.ReplaceAll(f.Verdict, "_", " "))
			if f.Note != "" {
				s += " " + f.Note
			}
			if len(f.Evidence) > 0 {
				s += " (" + refList(f.Evidence) + ")"
			}
			line("%s", s)
		}
	}
	if len(ret.Plan.Dropped) > 0 {
		line("")
		line("## Left out")
		line("")
		line("Parts of the model's answer that did not hold up:")
		line("")
		for _, d := range ret.Plan.Dropped {
			line("- %s", d)
		}
	}
	if ret.Evidence != nil {
		line("")
		line("## Evidence")
		line("")
		line("````text")
		b.WriteString(strings.TrimRight(ret.Evidence.Text(), "\n") + "\n")
		line("````")
	}
	return b.String()
}

// ChatSummary is the short message a retro sends: what went wrong, what
// changed, and the numbered proposals that need a yes.
func ChatSummary(ret *Retro) string {
	if ret.Plan.Quiet() {
		return Silent
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**Retro for %s** (%s to %s)\n\n%s", ret.Agent, ret.From.Format("Jan 2"), ret.To.Format("Jan 2"), cutText(ret.Plan.Summary, 600))
	if len(ret.Plan.WentBadly) > 0 {
		b.WriteString("\n\n**What went wrong**")
		for i, p := range ret.Plan.WentBadly {
			if i == 5 {
				fmt.Fprintf(&b, "\n- and %d more in the report", len(ret.Plan.WentBadly)-i)
				break
			}
			fmt.Fprintf(&b, "\n- %s", cutText(p.What, 200))
		}
	}
	var changed, waiting []ActionState
	for _, a := range ret.Actions {
		switch a.Status {
		case Applied:
			changed = append(changed, a)
		case Proposed, Failed:
			waiting = append(waiting, a)
		}
	}
	if len(changed) > 0 {
		b.WriteString("\n\n**Changed**")
		for _, a := range changed {
			fmt.Fprintf(&b, "\n%d. %s %s: %s", a.N, a.Kind, a.Target, cutText(oneLine(a.Result), 160))
		}
	}
	if len(waiting) > 0 {
		b.WriteString("\n\n**Needs a yes**")
		for _, a := range waiting {
			fmt.Fprintf(&b, "\n%d. %s %s: %s", a.N, a.Kind, a.Target, cutText(oneLine(a.Why), 200))
		}
		b.WriteString("\n\nReply /retro apply N or /retro reject N.")
	}
	if ret.Report != "" {
		fmt.Fprintf(&b, "\n\nFull report: %s", ret.Report)
	}
	return b.String()
}

// writeReport writes the report to agents/<agent>/retros/YYYY-MM-DD.md,
// named by the period's last day. The name is chosen once and stored: a
// second retro the same day gets -2, -3, ...
func (r *Runner) writeReport(ctx context.Context, ret *Retro) error {
	loc := r.Location
	if loc == nil {
		loc = time.Local
	}
	path := ""
	if ret.Report != "" {
		path = filepath.Join(r.Home, filepath.FromSlash(ret.Report))
	} else {
		dir := filepath.Join(r.Home, "agents", ret.Agent, "retros")
		day := ret.To.In(loc).Format("2006-01-02")
		marker := "Retro `" + ret.ID + "`."
		for i := 1; i < 100; i++ {
			name := day + ".md"
			if i > 1 {
				name = fmt.Sprintf("%s-%d.md", day, i)
			}
			p := filepath.Join(dir, name)
			data, err := os.ReadFile(p)
			if errors.Is(err, os.ErrNotExist) || (err == nil && strings.Contains(string(data), marker)) {
				path = p
				break
			}
		}
		if path == "" {
			return fmt.Errorf("too many retros for %s on %s", ret.Agent, day)
		}
		ret.Report = r.relPath(path)
		if _, err := r.db().ExecContext(ctx, `UPDATE retros SET report = ? WHERE id = ?`, ret.Report, ret.ID); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return store.WriteFileAtomic(path, []byte(Markdown(ret, loc)), 0o600)
}

func refList(refs []string) string {
	if len(refs) == 0 {
		return "no evidence"
	}
	return strings.Join(refs, ", ")
}

// quote indents text as a Markdown block quote.
func quote(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight("> "+l, " ")
	}
	return strings.Join(lines, "\n")
}
