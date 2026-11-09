package channel

import "context"

// MemoryCommands backs /remember and /forget. Replies are Markdown for the
// chat; errors are reported to the user as they are.
type MemoryCommands interface {
	// Remember saves text, as the user wrote it, to the agent's memory.
	Remember(ctx context.Context, agent, sessionID, text string) (string, error)
	// Forget removes the memory that query names: a slug, or words that
	// match exactly one memory; several matches are listed instead.
	Forget(ctx context.Context, agent, query string) (string, error)
}

// /remember

func (g *Gateway) cmdRemember(ctx context.Context, c *Call) {
	if g.Memory == nil {
		c.Reply(c.T("memory.unavailable"))
		return
	}
	if c.Args == "" {
		c.Reply(c.T("remember.usage"))
		return
	}
	conv, ok := c.session(ctx)
	if !ok {
		return
	}
	out, err := g.Memory.Remember(ctx, conv.Agent, conv.SessionID, c.Args)
	if err != nil {
		c.Reply(c.T("remember.failed", err.Error()))
		return
	}
	c.Reply(out)
}

// /forget

func (g *Gateway) cmdForget(ctx context.Context, c *Call) {
	if g.Memory == nil {
		c.Reply(c.T("memory.unavailable"))
		return
	}
	if c.Args == "" {
		c.Reply(c.T("forget.usage"))
		return
	}
	out, err := g.Memory.Forget(ctx, g.memoryAgent(ctx, c), c.Args)
	if err != nil {
		c.Reply(c.T("forget.failed", err.Error()))
		return
	}
	c.Reply(out)
}

// memoryAgent is the agent whose memory a command in this conversation
// acts on.
func (g *Gateway) memoryAgent(ctx context.Context, c *Call) string {
	agentName := c.Pick
	if conv, err := g.Store.Conversation(ctx, c.To); err == nil && conv != nil {
		agentName = conv.Agent
	}
	if _, ok := g.Runner.Agents[agentName]; !ok {
		agentName = c.Channel.AgentFor(c.To.Conversation)
	}
	return agentName
}

// MemoryHistoryCommands backs /memory and /undo-memory. A MemoryCommands
// that also implements it gets them.
type MemoryHistoryCommands interface {
	// MemoryHistory lists the agent's latest memory changes with their ids.
	MemoryHistory(ctx context.Context, agent string) (string, error)
	// UndoMemory puts back what one change replaced; the restore is itself
	// a recorded change.
	UndoMemory(ctx context.Context, agent, sessionID, id string) (string, error)
}

// /memory

func (g *Gateway) cmdMemory(ctx context.Context, c *Call) {
	h, ok := g.Memory.(MemoryHistoryCommands)
	if !ok {
		c.Reply(c.T("memory.unavailable"))
		return
	}
	out, err := h.MemoryHistory(ctx, g.memoryAgent(ctx, c))
	if err != nil {
		c.Reply("Couldn't read the memory history: " + err.Error())
		return
	}
	c.Reply(out)
}

// /undo-memory

func (g *Gateway) cmdUndoMemory(ctx context.Context, c *Call) {
	h, ok := g.Memory.(MemoryHistoryCommands)
	if !ok {
		c.Reply(c.T("memory.unavailable"))
		return
	}
	if c.Args == "" {
		c.Reply("Usage: /undo-memory <id> (the ids are in /memory)")
		return
	}
	sessionID := ""
	if conv, err := g.Store.Conversation(ctx, c.To); err == nil && conv != nil {
		sessionID = conv.SessionID
	}
	out, err := h.UndoMemory(ctx, g.memoryAgent(ctx, c), sessionID, c.Args)
	if err != nil {
		c.Reply("Couldn't undo that: " + err.Error())
		return
	}
	c.Reply(out)
}
