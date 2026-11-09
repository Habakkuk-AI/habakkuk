package channel

import (
	"slices"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/ops/i18n"
)

// lang is the language of the text Habakkuk writes itself in a conversation
// (command replies, approval prompts, notices): the channel's language,
// else the gateway's, else English. The model's own replies follow the
// user's language regardless.
func (g *Gateway) lang(to Address) string {
	chLang := ""
	if ch := g.Channels[to.Account]; ch != nil {
		chLang = ch.Language
	}
	return i18n.Pick(chLang, g.Language)
}

// t formats a catalog message in the conversation's language.
func (g *Gateway) t(to Address, key string, args ...any) string {
	return i18n.T(g.lang(to), key, args...)
}

// T formats a catalog message in the language of the command's chat.
func (c *Call) T(key string, args ...any) string { return c.G.t(c.To, key, args...) }

// builtinHelp lists the gateway's own commands, in the chat's language.
func builtinHelp(lang string) string {
	var b strings.Builder
	b.WriteString(i18n.T(lang, "help.header"))
	for _, name := range []string{"new", "stop", "status", "compact", "undo", "help"} {
		b.WriteString("\n/" + name + ": " + i18n.T(lang, "help."+name))
	}
	return b.String()
}

// isYes and isNo read a reply to an approval prompt in the chat's language
// (English words always count).
func (g *Gateway) isYes(to Address, word string) bool {
	return slices.Contains(i18n.Words(g.lang(to), "approval.yes_words"), word)
}

func (g *Gateway) isNo(to Address, word string) bool {
	return slices.Contains(i18n.Words(g.lang(to), "approval.no_words"), word)
}
