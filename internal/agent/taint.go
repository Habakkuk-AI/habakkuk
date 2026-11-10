package agent

import (
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// markHostedContent taints the turn when the model used the provider's
// hosted web search or fetch: their results reach the model inside opaque
// blocks, not through a Habakkuk tool, so the registry never sees them.
func markHostedContent(env *tools.Env, m llm.Message) {
	for _, b := range m.Blocks {
		if b.Type == llm.Opaque {
			env.Taint.Mark()
			return
		}
	}
}

// restoreTaint sets the taint of a turn that is running again (after a
// restart, a crash or a lost lease) from what its earlier run recorded. The
// taint itself lives in memory for one run; without this a turn that had
// read a web page before the restart would resume clean, and its shell
// calls would no longer need a human. The transcript is what survives, so
// it is the record: any hosted search block, and any call to a tool that
// can bring outside content in, written by this turn. A call counts even
// when its result was never recorded; the tool may have run.
func (t *turn) restoreTaint(env *tools.Env, recs []session.Record) {
	if env.Taint == nil || env.Taint.Marked() {
		return
	}
	for _, rec := range recs {
		if rec.JobID != t.job.ID || rec.Message.Role != llm.Assistant {
			continue
		}
		for _, b := range rec.Message.Blocks {
			if b.Type == llm.Opaque || (b.Type == llm.ToolUse && t.r.Tools != nil && t.r.Tools.MayTaint(b.ToolName)) {
				env.Taint.Mark()
				return
			}
		}
	}
}

// limitRules decides whether approval rules may answer for the turn. They
// answer only for a turn the owner typed just now, in a direct chat or on
// this machine (TurnInfo.Typed), that is not background work, delegated,
// or another system's request, and is not tainted. Every other turn
// (schedules, /goal follow-ups and other turns the gateway queues, notices,
// board cards, sub-agents, A2A, the API, MCP clients, editors) asks a
// person (tools.Env.RulesOff). The policy's Place then says where the turn
// stands, and "" means no rules.
//
// Rules are also paused (tools.Env.RulesPaused) when the turn's message
// carries chat notes (text that reached the chat from elsewhere), when its
// parent had them paused, and when anything tainted entered the session
// since the owner's previous message (sessionPaused).
func (t *turn) limitRules(env *tools.Env, recs []session.Record) {
	i := t.info
	if !i.Typed || !i.OwnerMessage() || i.Tainted {
		env.RulesOff = true
		return
	}
	switch {
	case t.sub != nil && t.sub.RulesPaused:
		env.RulesPaused = true
	case t.r.Notes != nil && t.r.Notes.NotesTaken(t.ctx, t.job.ID):
		env.RulesPaused = true
	case t.sessionPaused(recs):
		env.RulesPaused = true
	}
}

// sessionPaused reports whether anything tainted entered the session since
// the owner's previous message: in that message's turn or any turn after
// it, a tool that can bring outside content in or a hosted search result;
// in a turn after it, a payload that started tainted (a tainted result, an
// A2A task) or a chat note. A turn whose job can no longer be read counts
// as tainted.
func (t *turn) sessionPaused(recs []session.Record) bool {
	var jobsSeen []string
	seen := map[string]bool{t.job.ID: true}
	for i := len(recs) - 1; i >= 0; i-- {
		if id := recs[i].JobID; id != "" && !seen[id] {
			seen[id] = true
			jobsSeen = append(jobsSeen, id)
		}
	}
	for _, id := range jobsSeen {
		typed, tainted := t.jobTrust(id)
		if tainted || t.recordsTaint(recs, id) {
			return true // in the previous message's own turn too: it came after the message
		}
		if typed {
			return false // the owner's previous message: its notes came with it, and nothing before counts
		}
		if t.r.Notes != nil && t.r.Notes.NotesTaken(t.ctx, id) {
			return true
		}
	}
	return false
}

// jobTrust reads how a turn job started: typed by the owner, and whether
// it started tainted. A job that cannot be read is taken as tainted.
func (t *turn) jobTrust(id string) (typed, tainted bool) {
	j, err := t.r.Queue.Get(t.ctx, id)
	if err != nil || j == nil {
		return false, true
	}
	var p TurnPayload
	if j.Decode(&p) != nil {
		return false, true
	}
	typed = (p.Typed || p.Local) && p.Source == "" && !p.Background && p.Sub == nil
	tainted = p.Tainted || p.Source == SourceA2A || (p.Sub != nil && p.Sub.Tainted)
	return typed, tainted
}

// recordsTaint reports whether the job's records show outside content.
func (t *turn) recordsTaint(recs []session.Record, id string) bool {
	for _, rec := range recs {
		if rec.JobID != id || rec.Message.Role != llm.Assistant {
			continue
		}
		for _, b := range rec.Message.Blocks {
			if b.Type == llm.Opaque || (b.Type == llm.ToolUse && t.r.Tools != nil && t.r.Tools.MayTaint(b.ToolName)) {
				return true
			}
		}
	}
	return false
}
