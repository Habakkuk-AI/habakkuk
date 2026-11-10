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

// pauseRules stops approval rules (tools.Env.RulesPaused) for a turn that
// nobody sent a fresh message, in a session where an earlier turn read
// outside content: that text is still in the context, and only the
// owner's next message re-arms the rules. A delegated task whose parent
// had them paused has them paused too.
func (t *turn) pauseRules(env *tools.Env, recs []session.Record) {
	if t.sub != nil && t.sub.RulesPaused {
		env.RulesPaused = true
		return
	}
	if !t.background && t.sub == nil {
		return
	}
	for _, rec := range recs {
		if rec.JobID == t.job.ID || rec.Message.Role != llm.Assistant {
			continue
		}
		for _, b := range rec.Message.Blocks {
			if b.Type == llm.Opaque || (b.Type == llm.ToolUse && t.r.Tools != nil && t.r.Tools.MayTaint(b.ToolName)) {
				env.RulesPaused = true
				return
			}
		}
	}
}
