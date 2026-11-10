package retro

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
)

// apply makes one action's change, as the context's history actor. It
// returns the action's new status and what to tell the owner. Skill,
// memory and SOUL.md changes go through the memory history; a pause of a
// schedule is undone with `habakkuk schedules resume`; config changes and
// other schedule changes are never made here, only spelled out. Each is
// safe to apply twice: the second time finds the change already made.
//
// background is true for a retro applying its own safe changes: pinned
// files are left alone (the action stays proposed) and memory notes are
// marked as coming from outside content, since the evidence holds tool
// output and messages.
func (r *Runner) apply(ctx context.Context, ret *Retro, a *ActionState, background bool) (status, result string, err error) {
	agent := ret.Agent
	switch a.Kind {
	case KindSkill:
		if r.Skills == nil {
			return "", "", errors.New("skills are not available here")
		}
		if background && r.Skills.Pinned(ctx, agent, a.Target) {
			return Proposed, "skill " + a.Target + " is pinned, so it was left for you to approve", nil
		}
		if _, _, err := r.Skills.SaveContext(ctx, agent, a.Target, a.Description, a.Change); err != nil {
			return "", "", err
		}
		return Applied, r.changed(ctx, ret, a, history.KindSkill, a.Target), nil
	case KindMemory:
		slug := memory.Slugify(a.Target)
		if background && r.Mem.Pinned(ctx, agent, slug) {
			return Proposed, "note " + slug + " is pinned, so it was left for you to approve", nil
		}
		if m, err := r.Mem.Read(ctx, agent, slug); err == nil && strings.TrimSpace(m.Body) == a.Change {
			return Applied, r.changed(ctx, ret, a, history.KindNote, slug), nil // made before a crash
		}
		n := memory.Note{Title: a.Target, Content: a.Change, Source: memory.SourceUser}
		if background {
			n.Source, n.Untrusted = memory.SourceReview, true
		}
		if _, _, err := r.Mem.Put(ctx, agent, n); err != nil {
			return "", "", err
		}
		return Applied, r.changed(ctx, ret, a, history.KindNote, slug), nil
	case KindSoul:
		if background {
			return Proposed, "", nil // never applied without the owner
		}
		if err := r.editSoul(ctx, agent, a); err != nil {
			return "", "", err
		}
		return Applied, r.changed(ctx, ret, a, history.KindSoul, history.SoulFile), nil
	case KindSchedule:
		if background {
			return Proposed, "", nil
		}
		if strings.EqualFold(strings.Trim(strings.TrimSpace(a.Change), "."), "pause") {
			if r.Schedules == nil {
				return "", "", errors.New("schedules are not available here")
			}
			sc, err := r.Schedules.Get(ctx, a.Target)
			if err != nil {
				return "", "", err
			}
			if sc.Agent != agent {
				return "", "", fmt.Errorf("schedule %s belongs to agent %s", a.Target, sc.Agent)
			}
			if sc.Enabled {
				if err := r.Schedules.SetEnabled(ctx, a.Target, false); err != nil {
					return "", "", err
				}
			}
			return Applied, fmt.Sprintf("paused schedule %s; `habakkuk schedules resume %s` turns it back on", a.Target, a.Target), nil
		}
		return Approved, fmt.Sprintf("Change schedule %s yourself, as proposed: %s. See `habakkuk schedules` (or remove it and add it again with the new spec or prompt).",
			a.Target, oneLine(a.Change)), nil
	case KindConfig:
		if background {
			return Proposed, "", nil
		}
		return Approved, fmt.Sprintf("Make this edit yourself: in %s set `%s` to\n\n%s\n\nthen run `habakkuk doctor` and restart `habakkuk serve`.",
			filepath.Join(r.Home, "config.yaml"), a.Target, fence(a.Change)), nil
	case KindAsk:
		if background {
			return Proposed, "", nil
		}
		return Approved, "answered yes", nil
	}
	return "", "", fmt.Errorf("unknown kind %q", a.Kind)
}

// changed names the history change an action made, for undoing it.
func (r *Runner) changed(ctx context.Context, ret *Retro, a *ActionState, kind, name string) string {
	h := r.Mem.History()
	if h == nil {
		return "applied"
	}
	cs, err := h.List(ctx, history.Filter{Agent: ret.Agent, Kind: kind, File: name, Limit: 5})
	if err != nil {
		return "applied"
	}
	note := actionNote(ret.ID, a.N)
	for _, c := range cs {
		if c.Note == note {
			return fmt.Sprintf("applied as change %s; `habakkuk memory restore %s` (or /undo-memory %s) undoes it", c.ID, c.ID, c.ID)
		}
	}
	return "applied (it was already in place)"
}

// editSoul appends the action's text to SOUL.md, or replaces the exact
// passage it names, through the memory history.
func (r *Runner) editSoul(ctx context.Context, agent string, a *ActionState) error {
	h := r.Mem.History()
	if h == nil {
		return errors.New("SOUL.md is only changed when the change can be undone, and memory history is not available here")
	}
	t := history.SoulTarget(r.Home, agent)
	data, err := os.ReadFile(t.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cur := string(data)
	var next string
	switch {
	case a.Find != "" && strings.Count(cur, a.Find) == 1:
		next = strings.Replace(cur, a.Find, a.Change, 1)
	case a.Find != "" && strings.Contains(cur, a.Change):
		return nil // replaced before
	case a.Find != "":
		return errors.New("the passage to replace is no longer in SOUL.md exactly once; it changed since the retro")
	case strings.Contains(cur, a.Change):
		return nil // added before
	case strings.TrimSpace(cur) == "":
		next = a.Change + "\n"
	default:
		next = strings.TrimRight(cur, "\n") + "\n\n" + a.Change + "\n"
	}
	out := []byte(next)
	_, err = h.Write(ctx, t, out, func() error {
		if err := os.MkdirAll(filepath.Dir(t.Path), 0o700); err != nil {
			return err
		}
		return store.WriteFileAtomic(t.Path, out, 0o600)
	})
	return err
}

// Outcome is what became of one action the owner approved or rejected.
type Outcome struct {
	N      int    `json:"n"`
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Status string `json:"status"`
	Result string `json:"result,omitempty"`
}

// ApplyActions applies the numbered actions of a finished retro, as the
// owner approved them; actor says who (the CLI, a chat command). An
// action already applied is left as it is. A failure is recorded on the
// action and reported, and the others still go ahead.
func (r *Runner) ApplyActions(ctx context.Context, id string, ns []int, actor history.Actor) ([]Outcome, error) {
	ret, err := r.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ret.Done {
		return nil, fmt.Errorf("retro %s is still running; try again when it is done", id)
	}
	if len(ns) == 0 {
		return nil, errors.New("say which actions, by number")
	}
	for _, n := range ns {
		if n < 1 || n > len(ret.Actions) {
			return nil, fmt.Errorf("retro %s has no action %d (it has %d)", id, n, len(ret.Actions))
		}
	}
	var out []Outcome
	for _, n := range ns {
		a := &ret.Actions[n-1]
		o := Outcome{N: n, Kind: a.Kind, Target: a.Target}
		if a.Status == Applied {
			o.Status, o.Result = Applied, "already applied: "+a.Result
			out = append(out, o)
			continue
		}
		act := actor
		act.Background, act.Note = false, actionNote(ret.ID, n)
		status, result, err := r.apply(history.WithActor(ctx, act), ret, a, false)
		if err != nil {
			if ctx.Err() != nil {
				return out, err
			}
			status, result = Failed, err.Error()
		}
		if err := r.setAction(ctx, ret.ID, n, status, result); err != nil {
			return out, err
		}
		a.Status, a.Result = status, result
		o.Status, o.Result = status, result
		out = append(out, o)
	}
	if err := r.writeReport(ctx, ret); err != nil {
		r.log().Warn("retro report not updated", "retro", ret.ID, "err", err)
	}
	return out, nil
}

// Reject marks an action rejected. One already applied must be undone
// through the memory history instead.
func (r *Runner) Reject(ctx context.Context, id string, n int) (Outcome, error) {
	ret, err := r.Get(ctx, id)
	if err != nil {
		return Outcome{}, err
	}
	if n < 1 || n > len(ret.Actions) {
		return Outcome{}, fmt.Errorf("retro %s has no action %d (it has %d)", id, n, len(ret.Actions))
	}
	a := &ret.Actions[n-1]
	if a.Status == Applied {
		return Outcome{}, fmt.Errorf("action %d is already applied (%s)", n, a.Result)
	}
	if err := r.setAction(ctx, ret.ID, n, Rejected, ""); err != nil {
		return Outcome{}, err
	}
	a.Status, a.Result = Rejected, ""
	if ret.Done {
		if err := r.writeReport(ctx, ret); err != nil {
			r.log().Warn("retro report not updated", "retro", ret.ID, "err", err)
		}
	}
	return Outcome{N: n, Kind: a.Kind, Target: a.Target, Status: Rejected}, nil
}

func fence(s string) string {
	return "```yaml\n" + strings.TrimRight(s, "\n") + "\n```"
}
