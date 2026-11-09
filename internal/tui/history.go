package tui

import (
	"slices"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/core/state"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// retryPrefix starts a message that reruns the user's last one, as /retry
// does in a chat.
const retryPrefix = "[The user asked you to try this message again; answer it afresh.]\n"

// synthetic marks user messages Habakkuk wrote itself.
var synthetic = []string{"[The user asked you to try", "[Standing goal", "[Guidance", "[Side question", "[Scheduled task", "[Task from another agent", "Background process"}

// cleanUserText strips the time stamp and task list a turn adds to the
// user's text. typed is false for messages Habakkuk wrote itself.
func cleanUserText(text string) (clean string, typed bool) {
	if strings.HasPrefix(text, "[") { // the "[Mon 2006-01-02 15:04 MST]" stamp line
		if _, rest, ok := strings.Cut(text, "\n"); ok {
			text = rest
		}
	}
	text, _, _ = strings.Cut(text, state.TaskListMarker)
	text = strings.TrimSpace(text)
	if rest, ok := strings.CutPrefix(text, strings.TrimSpace(retryPrefix)); ok {
		return strings.TrimSpace(rest), true
	}
	return text, !slices.ContainsFunc(synthetic, func(p string) bool { return strings.HasPrefix(text, p) })
}

// historyEntries turns a stored transcript into transcript entries, and
// returns the messages the user typed, oldest first, for input history.
func historyEntries(recs []session.Record) ([]*entry, []string) {
	var out []*entry
	var typed []string
	tools := map[string]*entry{}
	for _, r := range recs {
		m := r.Message
		for i, b := range m.Blocks {
			switch {
			case b.Type == llm.Text && m.Role == llm.User && i == 0:
				text, ok := cleanUserText(b.Text)
				if text == "" {
					continue
				}
				if ok {
					out = append(out, &entry{kind: kindUser, text: text})
					typed = append(typed, text)
				} else {
					out = append(out, &entry{kind: kindNotice, text: firstLine(text)})
				}
			case b.Type == llm.Text && m.Role == llm.Assistant:
				if strings.TrimSpace(b.Text) != "" {
					out = append(out, &entry{kind: kindAssistant, text: b.Text})
				}
			case b.Type == llm.Thinking && strings.TrimSpace(b.Text) != "":
				out = append(out, &entry{kind: kindThinking, text: b.Text})
			case b.Type == llm.ToolUse:
				e := &entry{kind: kindTool, toolID: b.ToolUseID, tool: b.ToolName, input: b.Input, status: toolNoResult}
				tools[b.ToolUseID] = e
				out = append(out, e)
			case b.Type == llm.ToolResult:
				if e := tools[b.ToolUseID]; e != nil {
					e.output, e.status = b.Text, toolOK
					if b.IsError {
						e.status = toolFailed
					}
				}
			case b.Type == llm.Image || b.Type == llm.Document:
				if m.Role == llm.User {
					out = append(out, &entry{kind: kindNotice, text: "(" + string(b.Type) + " " + b.Name + ")"})
				}
			}
		}
	}
	return out, typed
}

// toolOutputs maps tool call ids to their recorded results.
func toolOutputs(recs []session.Record) map[string]llm.Block {
	out := map[string]llm.Block{}
	for _, r := range recs {
		for _, b := range r.Message.Blocks {
			if b.Type == llm.ToolResult {
				out[b.ToolUseID] = b
			}
		}
	}
	return out
}
