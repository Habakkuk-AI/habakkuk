package channel

import (
	"path/filepath"
	"slices"

	"github.com/habakkuk-ai/habakkuk/internal/ops/i18n"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// "Always allow": the words that say it in a chat, and how a chat is
// shown what the rule it saves will cover.

func (g *Gateway) isAlways(to Address, word string) bool {
	return slices.Contains(i18n.Words(g.lang(to), "approval.always_words"), word) ||
		slices.Contains(i18n.Words("en", "approval.always_words"), word)
}

// scope names where a rule saved from to applies: this chat, or this
// computer for the local web chat.
func (g *Gateway) scope(lang string, to Address) string {
	if ch := g.Channels[to.Account]; ch != nil && !ch.MessagingChannel() {
		return i18n.T(lang, "approval.scope.local")
	}
	return i18n.T(lang, "approval.scope.chat")
}

// ruleText says in lang what req's rule covers, with the rule itself in
// full, unaltered, in a code block: the command exactly as it will be
// matched, or the folder a file tool may write in. tools.ProposeRule
// offers no rule for a command with a backquote or a line break, or longer
// than fits clearly, so the block always shows it whole.
func (g *Gateway) ruleText(lang string, to Address, req tools.ApprovalRequest) string {
	r := req.Rule
	if r == nil {
		return ""
	}
	where := func(dir string) string {
		ws := req.Workspace
		if real, err := filepath.EvalSymlinks(ws); err == nil {
			ws = real
		}
		if dir == "" || dir == ws {
			if p := g.place(lang, req.Workspace, req.Workspace); p != "" {
				return p
			}
			dir = req.Workspace
		}
		return "`" + tildePath(dir) + "`"
	}
	if r.Tool != "exec" {
		return i18n.T(lang, "approval.always_write", g.scope(lang, to)) + "\n```\n" + r.Path + "\n```"
	}
	return i18n.T(lang, "approval.always_command", g.scope(lang, to), where(r.In)) + "\n```\n" + r.Exact + "\n```"
}
