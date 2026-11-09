package agent

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// Context compaction keeps weeks-long chat sessions inside the model's
// window. The transcript is never rewritten: a compaction is a separate
// append-only record holding a summary of everything before a cut point, and
// the model is sent that summary followed by the messages after the cut. The
// system prompt is untouched, so its cache entry survives.

const (
	// manualKeepTokens is the tail kept verbatim by /compact when the agent
	// has automatic compaction turned off.
	manualKeepTokens = 20000
	// summaryMaxTokens caps the summarizing call's output.
	summaryMaxTokens = 16000
	// Per-block limits for the summary input: raw tool output is mostly
	// noise to a summary, and the whole input must fit the model's window.
	maxRenderedText = 8000
	maxRenderedTool = 2000
)

const summarySystem = `You compact the history of a long conversation between a user and their AI assistant, so the assistant can carry on without the full transcript. You get the previous summary, if there is one, and the transcript that follows it. Write one new summary that replaces both.

Write it for the assistant, addressing it as "you". Keep:
- the user's goals and what is being worked on now
- decisions made, and the reasons given
- facts about the user: preferences, people, places, dates, accounts, habits
- open tasks, questions waiting for an answer, and everything the assistant promised to do or follow up on, with any deadlines
- exact file paths, URLs, ids, names, numbers and commands that may be needed again
- results of tool calls that matter later: what was created, changed, sent or found

Drop small talk, superseded plans and raw tool output. Never invent anything; if something is uncertain, say so. Put the most recent state last. Use short headed sections with bullet points, at most about 1500 words. Reply with the summary only.`

const summaryPreamble = "[Summary of the earlier conversation. It stands in for the messages before this point, which are no longer shown.]\n\n"

// loadCompaction picks up the latest compaction and the most recent usage
// figures from a freshly loaded transcript.
func (t *turn) loadCompaction(recs []session.Record) error {
	t.usageAt = -1
	for _, rec := range recs {
		if rec.Usage != nil {
			t.usage, t.usageAt = *rec.Usage, rec.Seq
		}
	}
	c, err := t.r.Sessions.LatestCompaction(t.ctx, t.sess.ID)
	if err != nil || c == nil {
		return err
	}
	t.base, t.summary, t.compactedAt = c.KeepFrom, c.Summary, c.Through
	return nil
}

// context is what the model is sent: the whole transcript, or the latest
// summary followed by the messages kept after it.
func (t *turn) context() []llm.Message {
	if t.summary == "" {
		return t.msgs
	}
	tail := t.msgs[t.base:]
	note := llm.Block{Type: llm.Text, Text: summaryPreamble + t.summary}
	out := make([]llm.Message, 0, len(tail)+1)
	if tail[0].Role == llm.User {
		// One user message, not two in a row.
		first := llm.Message{Role: llm.User, Blocks: append([]llm.Block{note}, tail[0].Blocks...)}
		return append(append(out, first), tail[1:]...)
	}
	return append(append(out, llm.Message{Role: llm.User, Blocks: []llm.Block{note}}), tail...)
}

// shouldCompact reports whether the last response, sent with the current
// context, filled more of the window than the agent's threshold.
func (t *turn) shouldCompact() bool {
	return t.spec.CompactAt > 0 && !t.compactTried && t.usageAt >= t.compactedAt &&
		t.usage.Prompt() >= int64(t.spec.CompactAt)
}

// autoCompact compacts once per turn run. A failed summary is logged and the
// turn carries on with the full context; only a lost lease or a canceled
// turn is returned.
func (t *turn) autoCompact() (bool, error) {
	t.compactTried = true
	folded, _, err := t.compact()
	switch {
	case errors.Is(err, jobs.ErrLeaseLost) || t.ctx.Err() != nil:
		return false, err
	case err != nil:
		t.r.Log.Warn("compaction failed; continuing with the full context", "session", t.sess.ID, "err", err)
		return false, nil
	}
	return folded > 0, nil
}

// compact summarizes the messages between the current base and a new cut
// point and records the summary. It returns how many messages it folded into
// the summary and how many it kept verbatim; folded is 0 when there was
// nothing to fold.
func (t *turn) compact() (folded, kept int, err error) {
	budget := t.spec.CompactAt / 5
	if budget <= 0 {
		budget = manualKeepTokens
	}
	keep := cutPoint(t.msgs, t.base, budget)
	if keep <= t.base {
		return 0, 0, nil
	}
	limit := t.spec.CompactAt
	if limit <= 0 {
		limit = 100000
	}
	input := summaryInput(t.summary, t.msgs[t.base:keep], limit*3)
	resp, err := t.r.Models.Send(t.ctx, t.chain, llm.Request{
		System: summarySystem, Messages: []llm.Message{llm.UserText(input)},
		MaxTokens: min(t.spec.MaxTokens, summaryMaxTokens),
	}, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("summarize: %w", err)
	}
	summary := strings.TrimSpace(resp.Message.Text())
	if summary == "" {
		return 0, 0, errors.New("summarize: the model returned no text")
	}
	if err := t.r.Queue.Owns(t.ctx, t.job.ID, t.owner); err != nil {
		return 0, 0, err
	}
	if err := t.r.Sessions.AddCompaction(t.ctx, t.sess.ID, session.Compaction{
		KeepFrom: keep, Through: len(t.msgs), Summary: summary, Model: resp.Model, Usage: &resp.Usage, JobID: t.job.ID,
	}); err != nil {
		return 0, 0, err
	}
	folded, kept = keep-t.base, len(t.msgs)-keep
	t.r.Log.Info("compacted session", "session", t.sess.ID, "folded", folded, "kept", kept,
		"prompt_tokens", t.usage.Prompt(), "summary_chars", len(summary))
	t.base, t.summary, t.compactedAt = keep, summary, len(t.msgs)
	return folded, kept, nil
}

// runCompact is a turn that only compacts (the /compact command).
func (t *turn) runCompact() error {
	recs, err := t.r.Sessions.Messages(t.ctx, t.sess.ID)
	if err != nil {
		return err
	}
	for _, rec := range recs {
		t.msgs = append(t.msgs, rec.Message)
	}
	if err := t.loadCompaction(recs); err != nil {
		return err
	}
	folded, kept, err := t.compact()
	if err != nil {
		return err
	}
	if folded == 0 {
		t.final = "Nothing to compact yet."
		return nil
	}
	t.final = fmt.Sprintf("Compacted: %d earlier messages are now a summary; the last %d are kept as they were.", folded, kept)
	return nil
}

// cutPoint picks the first message to keep verbatim when compacting
// msgs[base:]. It cuts only before a user message that starts a turn (one
// without tool results) or before an assistant message, so a tool call is
// never separated from its result. It prefers turn starts, keeping as many
// recent turns as fit in budget tokens and always the last one; only when
// the last turn alone is over budget does it cut inside that turn. It
// returns base when there is nothing to fold or msgs[base:] already fits.
func cutPoint(msgs []llm.Message, base, budget int) int {
	n := len(msgs)
	suffix := make([]int, n+1)
	for i := n - 1; i >= 0; i-- {
		suffix[i] = suffix[i+1] + estimate(msgs[i])
	}
	if suffix[base] <= budget {
		return base
	}
	pick := func(ok func(llm.Message) bool) int {
		best := base
		for i := n - 1; i > base; i-- {
			if !ok(msgs[i]) {
				continue
			}
			if best != base && suffix[i] > budget {
				break
			}
			best = i
		}
		return best
	}
	keep := pick(startsTurn)
	if suffix[keep] > budget {
		if k := pick(func(m llm.Message) bool { return m.Role == llm.Assistant || startsTurn(m) }); k > keep {
			keep = k
		}
	}
	return keep
}

func startsTurn(m llm.Message) bool {
	if m.Role != llm.User {
		return false
	}
	for _, b := range m.Blocks {
		if b.Type == llm.ToolResult {
			return false
		}
	}
	return true
}

// estimate is a rough token count: about four bytes a token, and a flat
// figure for media.
func estimate(m llm.Message) int {
	n := 4
	for _, b := range m.Blocks {
		switch b.Type {
		case llm.Image, llm.Document:
			n += 1600
		default:
			n += (len(b.Text) + len(b.Input) + len(b.Raw) + len(b.Data)) / 4
		}
	}
	return n
}

// summaryInput renders the previous summary and the messages being folded as
// plain text. Media and opaque provider blocks (hosted web search) become
// placeholders, reasoning is left out, long text is cut, and if the whole is
// still over maxChars the oldest messages are dropped.
func summaryInput(prev string, msgs []llm.Message, maxChars int) string {
	names := map[string]string{}
	var lines []string
	for _, m := range msgs {
		for _, b := range m.Blocks {
			var line string
			switch b.Type {
			case llm.Text:
				if strings.TrimSpace(b.Text) != "" {
					line = fmt.Sprintf("[%s] %s", m.Role, cut(b.Text, maxRenderedText))
				}
			case llm.ToolUse:
				names[b.ToolUseID] = b.ToolName
				line = fmt.Sprintf("[assistant called %s] %s", b.ToolName, cut(string(b.Input), maxRenderedTool))
			case llm.ToolResult:
				status := ""
				if b.IsError {
					status = ", error"
				}
				line = fmt.Sprintf("[result of %s%s] %s", names[b.ToolUseID], status, cut(b.Text, maxRenderedTool))
			case llm.Image, llm.Document:
				line = fmt.Sprintf("[%s %s %q omitted]", m.Role, b.Type, b.Name)
			case llm.Opaque:
				line = "[hosted web search or fetch activity omitted]"
			}
			if line != "" {
				lines = append(lines, line)
			}
		}
	}
	var b strings.Builder
	if prev != "" {
		b.WriteString("<previous_summary>\n" + prev + "\n</previous_summary>\n\n")
	}
	size := b.Len()
	from := len(lines)
	for from > 0 && size+len(lines[from-1])+1 <= maxChars {
		from--
		size += len(lines[from]) + 1
	}
	b.WriteString("<transcript>\n")
	if from > 0 {
		fmt.Fprintf(&b, "[%d earlier lines omitted for length]\n", from)
	}
	for _, l := range lines[from:] {
		b.WriteString(l + "\n")
	}
	b.WriteString("</transcript>\n\nWrite the new summary now.")
	return b.String()
}

// cut shortens s to at most n bytes, on a character boundary, and says how
// much it left out.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + fmt.Sprintf(" ...[%d more characters]", len(s)-n)
}
