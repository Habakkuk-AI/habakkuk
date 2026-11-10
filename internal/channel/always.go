package channel

import (
	"slices"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/ops/i18n"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// "Always allow": the words that say it in a chat, and how a chat is
// shown what the rule it saves will cover.

func (g *Gateway) isAlways(to Address, word string) bool {
	return slices.Contains(i18n.Words(g.lang(to), "approval.always_words"), word) ||
		slices.Contains(i18n.Words("en", "approval.always_words"), word)
}

// ruleText says in lang what req's rule covers: "`git status` run in your
// workspace".
func (g *Gateway) ruleText(lang string, req tools.ApprovalRequest) string {
	r := req.Rule
	if r == nil {
		return ""
	}
	code := func(s string) string {
		return "`" + strings.ReplaceAll(shorten(g.redact(s), 120), "`", "'") + "`"
	}
	where := func(dir string) string {
		if dir == "" {
			dir = req.Workspace
		}
		if p := g.place(lang, dir, req.Workspace); p != "" && dir == req.Workspace {
			return p
		}
		return code(tildePath(dir))
	}
	switch {
	case r.Exact != "":
		return i18n.T(lang, "approval.rule.exact", code(r.Exact), where(r.In))
	case r.Command != "":
		cmd, more := r.Pattern()
		if more {
			return i18n.T(lang, "approval.rule.prefix", code(cmd), where(r.In))
		}
		return i18n.T(lang, "approval.rule.command", code(cmd), where(r.In))
	}
	return i18n.T(lang, "approval.rule.write", where(r.Path))
}
