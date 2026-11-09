package channel

import (
	"context"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/ops/i18n"
)

// /fork, /rewind, /title and /subagents act on the conversation's session.

func (g *Gateway) cmdFork(ctx context.Context, c *Call) {
	conv, err := g.Store.Conversation(ctx, c.To)
	if err != nil || conv == nil {
		c.Reply(c.T("fork.nothing"))
		return
	}
	fork, err := g.Runner.Fork(ctx, conv.SessionID, 0)
	if err != nil {
		c.Reply(c.T("fork.failed", err.Error()))
		return
	}
	if !g.switchTo(ctx, c, conv, fork) {
		return
	}
	c.Reply(c.T("fork.done", fork.ID, conv.SessionID))
}

func (g *Gateway) cmdRewind(ctx context.Context, c *Call) {
	n := 1
	if c.Args != "" {
		v, err := strconv.Atoi(strings.TrimSpace(c.Args))
		if err != nil || v < 1 {
			c.Reply(c.T("rewind.usage"))
			return
		}
		n = v
	}
	conv, err := g.Store.Conversation(ctx, c.To)
	if err != nil || conv == nil {
		c.Reply(c.T("rewind.nothing"))
		return
	}
	fork, err := g.Runner.Fork(ctx, conv.SessionID, n)
	if err != nil {
		c.Reply(c.T("rewind.failed", err.Error()))
		return
	}
	if !g.switchTo(ctx, c, conv, fork) {
		return
	}
	c.Reply(i18n.N(g.lang(c.To), "rewind.done", n, fork.ID, conv.SessionID))
}

// switchTo binds the conversation to another session, declining any
// approval the old one was waiting on.
func (g *Gateway) switchTo(ctx context.Context, c *Call, conv *Conversation, sess *session.Session) bool {
	g.resolveApproval(c.To, false)
	if err := g.Store.Bind(ctx, c.To, conv.Agent, sess.ID); err != nil {
		g.Log.Error("bind conversation failed", "err", err)
		c.Reply(c.T("switch.failed"))
		return false
	}
	return true
}

const maxTitle = 120

func (g *Gateway) cmdTitle(ctx context.Context, c *Call) {
	conv, ok := c.session(ctx)
	if !ok {
		return
	}
	title := strings.Join(strings.Fields(c.Args), " ")
	if title == "" {
		sess, err := g.Runner.Sessions.Get(ctx, conv.SessionID)
		if err != nil {
			c.Reply(c.T("conv.read_failed"))
			return
		}
		if sess.Title == "" {
			c.Reply(c.T("title.none"))
		} else {
			c.Reply(c.T("title.current", sess.Title))
		}
		return
	}
	if utf8.RuneCountInString(title) > maxTitle {
		title = string([]rune(title)[:maxTitle])
	}
	if _, err := g.Runner.Sessions.SetTitleFrom(ctx, conv.SessionID, title, session.TitleUser); err != nil {
		c.Reply(c.T("title.failed"))
		return
	}
	c.Reply(c.T("title.set", title))
}

func (g *Gateway) cmdSubagents(ctx context.Context, c *Call) {
	conv, err := g.Store.Conversation(ctx, c.To)
	if err != nil || conv == nil {
		c.Reply(c.T("subagents.none"))
		return
	}
	verb, id, _ := strings.Cut(c.Args, " ")
	switch strings.ToLower(verb) {
	case "":
		subs, err := g.Runner.Subturns(ctx, conv.SessionID)
		if err != nil {
			c.Reply(c.T("subagents.list_failed", err.Error()))
			return
		}
		c.Reply(agent.FormatSubturns(subs, g.now()))
	case "stop", "cancel", "kill":
		if strings.TrimSpace(id) == "" {
			c.Reply(c.T("subagents.stop_usage"))
			return
		}
		s, err := g.Runner.StopSubturn(ctx, conv.SessionID, id)
		if err != nil {
			c.Reply(c.T("subagents.stop_failed", err.Error()))
			return
		}
		c.Reply(c.T("subagents.stopped", s.ID, s.Agent))
	default:
		c.Reply(c.T("subagents.usage"))
	}
}
