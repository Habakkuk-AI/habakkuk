package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
)

// openMemoryHistory opens history.db, has the memory store, the skills
// library and set_identity record their changes in it, and takes in hand
// edits made while Habakkuk was down (the first scan of an agent is a
// baseline).
func (a *App) openMemoryHistory(ctx context.Context, home string) error {
	h, err := history.Open(ctx, home)
	if err != nil {
		return err
	}
	h.SetLogger(a.Log)
	a.closers = append(a.closers, h.Close)
	a.Memory.UseHistory(h)
	a.Skills.History = h
	a.scanMemoryHistory(ctx)
	return nil
}

// scanMemoryHistory records hand edits to every agent's notes, USER.md,
// IDENTITY.md and skills.
func (a *App) scanMemoryHistory(ctx context.Context) {
	for _, name := range a.Cfg.AgentNames() {
		if err := a.Memory.ScanHistory(ctx, name); err != nil && ctx.Err() == nil {
			a.Log.Warn("memory history scan failed", "agent", name, "err", err)
		}
	}
}

// pruneMemoryHistory drops changes older than memory.history_days, keeping
// the newest version of every file that exists.
func (a *App) pruneMemoryHistory(ctx context.Context) {
	h := a.Memory.History()
	keep := a.Cfg.Memory.HistoryRetention()
	if h == nil || keep <= 0 {
		return
	}
	n, err := h.Prune(ctx, time.Now().Add(-keep))
	if err != nil {
		a.Log.Warn("memory history prune failed", "err", err)
		return
	}
	if n > 0 {
		a.Log.Info("memory history pruned", "changes", n)
	}
}

// memoryChangesShown is how many changes /memory lists.
const memoryChangesShown = 10

// MemoryHistory lists the agent's latest memory changes for /memory.
func (m memoryCommands) MemoryHistory(ctx context.Context, agentName string) (string, error) {
	h := m.mem.History()
	if h == nil {
		return "Memory history isn't available here.", nil
	}
	cs, err := h.List(ctx, history.Filter{Agent: agentName, Limit: memoryChangesShown})
	if err != nil {
		return "", err
	}
	if len(cs) == 0 {
		return "No memory changes recorded yet.", nil
	}
	var b strings.Builder
	b.WriteString("Latest changes to my memory, newest first. /undo-memory <id> puts back what was there before one:")
	for _, c := range cs {
		fmt.Fprintf(&b, "\n- `%s` %s", c.ID, strings.TrimSpace(strings.TrimPrefix(c.Describe(), c.ID)))
	}
	return b.String(), nil
}

// UndoMemory restores what a change replaced, for /undo-memory.
func (m memoryCommands) UndoMemory(ctx context.Context, agentName, sessionID, id string) (string, error) {
	id = strings.Trim(strings.TrimSpace(id), "`")
	ctx = history.WithActor(ctx, history.Actor{By: history.ByUser, Tool: "/undo-memory", Session: sessionID})
	c, r, err := m.mem.Restore(ctx, agentName, id, history.Before)
	if err != nil {
		return "", err
	}
	what := c.Name
	switch c.Kind {
	case history.KindNote:
		what = "note " + c.Name
	case history.KindSkill:
		what = "skill " + c.Name
	}
	if r == nil {
		return fmt.Sprintf("%s is already as it was before change `%s`.", what, c.ID), nil
	}
	verb := "Restored"
	if r.Op() == "deleted" {
		verb = "Removed" // the change created it
	}
	return fmt.Sprintf("%s %s as it was before change `%s`. That is change `%s`; /undo-memory %s reverts it.", verb, what, c.ID, r.ID, r.ID), nil
}
