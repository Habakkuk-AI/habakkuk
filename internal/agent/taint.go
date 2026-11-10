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

// limitRules decides whether approval rules may answer for the turn.
// They answer only for the owner's own message (in a chat, or typed on
// this machine) and for the run of a schedule a clean owner turn made
// (TurnPayload.Tainted is unset). Every other turn, a board card's run, a
// sub-agent's task, a background result or notice, an API, MCP or A2A
// request, asks a person (tools.Env.RulesOff). The policy's Place then
// says where the turn stands, and "" (a guest's, a webhook's) means none.
//
// Rules are also paused (tools.Env.RulesPaused) for a turn whose message
// carries a chat note from a turn outside content steered: it entered the
// session with this message, so only the owner's next message re-arms
// them.
func (t *turn) limitRules(env *tools.Env) {
	i := t.info
	scheduled := i.Source == SourceSchedule && !i.Tainted && !i.Delegated
	if !i.OwnerMessage() && !scheduled {
		env.RulesOff = true
	}
	if t.sub != nil && t.sub.RulesPaused {
		env.RulesPaused = true
	}
	if t.r.Notes != nil && t.r.Notes.NotesTainted(t.ctx, t.job.ID) {
		env.RulesPaused = true
	}
}
