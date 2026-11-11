package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/app"
	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/core/state"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/usage"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// AppBackend runs the UI against a Habakkuk app in this process: its workers
// run the turns, and stopping a turn cancels it at once rather than at its
// next heartbeat.
type AppBackend struct {
	A *app.App

	mu      sync.Mutex
	running map[string]context.CancelFunc // session -> the turn running here
}

// NewAppBackend wraps a. Without channels there is no clarify tool, so it
// registers one that asks through the UI.
func NewAppBackend(a *app.App) *AppBackend {
	if a.Gateway == nil {
		a.Tools.Register("core", channel.ClarifyTool(nil))
	}
	return &AppBackend{A: a, running: map[string]context.CancelFunc{}}
}

// Start connects MCP servers and runs the job workers until the returned
// func is called, which waits for them.
func (b *AppBackend) Start(ctx context.Context) func() {
	b.A.MCP.Start(ctx)
	ctx, cancel := context.WithCancel(ctx)
	pools := b.A.Pools(false)
	for _, p := range pools {
		if h, ok := p.Handlers[agent.KindTurn]; ok {
			p.Handlers[agent.KindTurn] = b.cancelable(h)
		}
	}
	var wg sync.WaitGroup
	for _, p := range pools {
		wg.Add(1)
		go func() { defer wg.Done(); p.Run(ctx) }()
	}
	return func() { cancel(); wg.Wait() }
}

// cancelable runs a turn under a context Cancel can end.
func (b *AppBackend) cancelable(h jobs.Handler) jobs.Handler {
	return func(ctx context.Context, j *jobs.Job, owner string) (string, error) {
		var p agent.TurnPayload
		if err := j.Decode(&p); err != nil {
			return h(ctx, j, owner)
		}
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		b.mu.Lock()
		b.running[p.Session] = cancel
		b.mu.Unlock()
		defer func() {
			b.mu.Lock()
			delete(b.running, p.Session)
			b.mu.Unlock()
		}()
		return h(ctx, j, owner)
	}
}

func (b *AppBackend) Agents() []string { return b.A.Cfg.AgentNames() }

func info(s *session.Session) SessionInfo {
	return SessionInfo{ID: s.ID, Agent: s.Agent, Title: s.Title, UpdatedAt: s.UpdatedAt}
}

func (b *AppBackend) NewSession(ctx context.Context, agentName string) (SessionInfo, error) {
	s, err := b.A.Runner.NewSessionWith(ctx, agentName, agent.SessionOptions{Chat: true})
	if err != nil {
		return SessionInfo{}, err
	}
	return info(s), nil
}

func (b *AppBackend) Session(ctx context.Context, id string) (SessionInfo, error) {
	s, err := b.A.Sessions.Get(ctx, id)
	if err != nil {
		return SessionInfo{}, err
	}
	return info(s), nil
}

func (b *AppBackend) RecentSessions(ctx context.Context, limit int) ([]SessionInfo, error) {
	list, err := b.A.Sessions.List(ctx, "", limit)
	if err != nil {
		return nil, err
	}
	out := make([]SessionInfo, len(list))
	for i := range list {
		out[i] = info(&list[i])
	}
	return out, nil
}

func (b *AppBackend) Transcript(ctx context.Context, id string) ([]session.Record, error) {
	return b.A.Sessions.Messages(ctx, id)
}

func (b *AppBackend) Busy(ctx context.Context, id string) bool {
	busy, err := b.A.Queue.Busy(ctx, id, "")
	return err == nil && busy
}

func (b *AppBackend) Submit(ctx context.Context, id, text string) error {
	if _, err := b.A.Runner.Submit(ctx, id, text); err != nil {
		return err
	}
	b.wake()
	return nil
}

func (b *AppBackend) SubmitTyped(ctx context.Context, id, text string) error {
	if _, err := b.A.Runner.SubmitTyped(ctx, id, text); err != nil {
		return err
	}
	b.wake()
	return nil
}

func (b *AppBackend) wake() {
	if b.A.Runner.Wake != nil {
		b.A.Runner.Wake()
	}
}

func (b *AppBackend) Cancel(ctx context.Context, id string) int {
	n, err := b.A.Queue.CancelSerial(ctx, id)
	if err != nil {
		b.A.Log.Warn("tui cancel failed", "session", id, "err", err)
	}
	b.mu.Lock()
	cancel := b.running[id]
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return int(n)
}

func (b *AppBackend) Subscribe(id string) (<-chan events.Event, func()) { return b.A.Bus.Subscribe(id) }

func (b *AppBackend) Attach(id string, a tools.Approver) func() { return b.A.Runner.Attach(id, a) }

func (b *AppBackend) Status(ctx context.Context, id string) Status {
	var st Status
	if chain, err := b.A.Runner.Chain(ctx, id); err == nil && len(chain) > 0 {
		st.Model = chain[0].String()
	}
	if b.A.Usage == nil {
		return st
	}
	calls, err := b.A.Usage.Calls(ctx, usage.Filter{Session: id})
	if err != nil {
		return st
	}
	t := usage.Summarize(calls, b.A.Pricer).Total
	st.Calls, st.In, st.Out = t.Calls, t.Prompt(), t.OutputTokens
	if t.Calls > 0 {
		st.Cost = t.CostString()
	}
	return st
}

func (b *AppBackend) Export(ctx context.Context, id, path string) (string, error) {
	sess, err := b.A.Sessions.Get(ctx, id)
	if err != nil {
		return "", err
	}
	recs, err := b.A.Sessions.Messages(ctx, id)
	if err != nil {
		return "", err
	}
	if path == "" {
		path = "habakkuk-" + id + ".md"
	}
	if path, err = filepath.Abs(path); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte(channel.ExportMarkdown(sess, recs, time.Now())), 0o600)
}

func (b *AppBackend) FollowUp(ctx context.Context, id string) bool {
	if b.A.State == nil {
		return false
	}
	notes, err := b.A.State.TakeSteers(ctx, id)
	if err != nil || len(notes) == 0 {
		return false
	}
	text := "[Guidance the user sent while you were working, which arrived after your last step. Act on it now.]\n" + strings.Join(notes, "\n\n")
	return b.Submit(ctx, id, text) == nil
}

// Command runs the backend commands of the registry.
func (b *AppBackend) Command(ctx context.Context, id, name, arg string) (CommandResult, error) {
	r := b.A.Runner
	switch name {
	case "/model":
		return CommandResult{Reply: r.ModelCommand(ctx, id, arg)}, nil
	case "/usage":
		return CommandResult{Reply: b.A.SessionUsage(ctx, id)}, nil
	case "/retry":
		recs, err := b.A.Sessions.Messages(ctx, id)
		if err != nil {
			return CommandResult{}, err
		}
		text, ok := lastTyped(recs)
		if !ok {
			return CommandResult{Reply: "There's nothing to retry yet."}, nil
		}
		if err := b.Submit(ctx, id, retryPrefix+text); err != nil {
			return CommandResult{}, err
		}
		return CommandResult{Reply: "Asking again: " + firstLine(text), Started: true}, nil
	case "/btw":
		if arg == "" {
			return CommandResult{Reply: "Usage: /btw <question>"}, nil
		}
		reply, err := b.btw(ctx, id, arg)
		return CommandResult{Reply: reply}, err
	case "/steer":
		return b.steer(ctx, id, arg)
	case "/think":
		return b.think(ctx, id, arg)
	case "/personality":
		return b.personality(ctx, id, arg)
	case "/compact":
		if _, err := r.SubmitTurn(ctx, agent.Turn{Session: id, Compact: true}); err != nil {
			return CommandResult{}, err
		}
		b.wake()
		return CommandResult{Reply: "Compacting the older part of this session…", Started: true}, nil
	case "/undo":
		if b.Busy(ctx, id) {
			return CommandResult{Reply: "A turn is still running. Stop it first (Ctrl+C), then /undo."}, nil
		}
		reply, err := b.A.Undo(ctx, id)
		return CommandResult{Reply: reply}, err
	}
	return CommandResult{}, fmt.Errorf("unknown command %s", name)
}

// lastTyped is the newest message the user typed.
func lastTyped(recs []session.Record) (string, bool) {
	for i := len(recs) - 1; i >= 0; i-- {
		m := recs[i].Message
		if m.Role != llm.User || len(m.Blocks) == 0 || m.Blocks[0].Type != llm.Text {
			continue
		}
		if text, typed := cleanUserText(m.Blocks[0].Text); typed && text != "" {
			return text, true
		}
	}
	return "", false
}

const maxBtwContext = 40000

// btw answers a side question in a throwaway session of the same agent,
// seeded with this one as text, and waits for the answer.
func (b *AppBackend) btw(ctx context.Context, id, question string) (string, error) {
	sess, err := b.A.Sessions.Get(ctx, id)
	if err != nil {
		return "", err
	}
	side, err := b.A.Runner.NewSession(ctx, sess.Agent)
	if err != nil {
		return "", err
	}
	text := "[Side question. The user asked this in passing while your main conversation with them goes on. Answer it briefly. This exchange is not added to the main conversation.]"
	if history := b.renderContext(ctx, id); history != "" {
		text += "\n\n<main_conversation>\n" + history + "\n</main_conversation>"
	}
	return b.A.Runner.Ask(ctx, agent.Turn{Session: side.ID, Text: text + "\n\nQuestion: " + question, Local: true}, nil)
}

// renderContext renders what the model sees of a session (the latest
// summary and the messages after it) as text, newest maxBtwContext bytes.
func (b *AppBackend) renderContext(ctx context.Context, id string) string {
	recs, err := b.A.Sessions.Messages(ctx, id)
	if err != nil {
		return ""
	}
	from := 0
	var parts []string
	if c, err := b.A.Sessions.LatestCompaction(ctx, id); err == nil && c != nil {
		from = c.KeepFrom
		parts = append(parts, "[summary of earlier messages] "+c.Summary)
	}
	cut := func(s string, n int) string {
		if r := []rune(s); len(r) > n {
			return string(r[:n]) + "…"
		}
		return s
	}
	for _, r := range recs {
		if r.Seq < from {
			continue
		}
		for _, bl := range r.Message.Blocks {
			switch bl.Type {
			case llm.Text:
				if strings.TrimSpace(bl.Text) != "" {
					parts = append(parts, fmt.Sprintf("[%s] %s", r.Message.Role, cut(bl.Text, 4000)))
				}
			case llm.ToolUse:
				parts = append(parts, fmt.Sprintf("[assistant called %s] %s", bl.ToolName, cut(string(bl.Input), 300)))
			case llm.ToolResult:
				parts = append(parts, "[tool result] "+cut(bl.Text, 600))
			}
		}
	}
	size, i := 0, len(parts)
	for i > 0 && size+len(parts[i-1]) < maxBtwContext {
		i--
		size += len(parts[i]) + 1
	}
	return strings.Join(parts[i:], "\n")
}

func (b *AppBackend) steer(ctx context.Context, id, arg string) (CommandResult, error) {
	if arg == "" {
		return CommandResult{Reply: "Usage: /steer <guidance for the task in progress>"}, nil
	}
	if !b.Busy(ctx, id) || b.A.State == nil {
		if err := b.Submit(ctx, id, arg); err != nil {
			return CommandResult{}, err
		}
		return CommandResult{Reply: "Nothing is running, so I'll take that as a new message.", Started: true}, nil
	}
	if err := b.A.State.AddSteer(ctx, id, arg); err != nil {
		return CommandResult{}, err
	}
	return CommandResult{Reply: "Got it. I'll take that in before my next step."}, nil
}

func (b *AppBackend) think(ctx context.Context, id, arg string) (CommandResult, error) {
	st := b.A.State
	if st == nil {
		return CommandResult{Reply: "That command isn't available here."}, nil
	}
	levels := efforts[:len(efforts)-1]
	level := strings.ToLower(arg)
	if level == "" {
		cur, _ := st.Settings(ctx, id)
		effort := cur.Effort
		if effort == "" {
			effort = "the agent's default"
			if sess, err := b.A.Sessions.Get(ctx, id); err == nil {
				if spec := b.A.Runner.Agents[sess.Agent]; spec != nil && spec.Effort != "" {
					effort += " (" + spec.Effort + ")"
				}
			}
		}
		return CommandResult{Reply: "Thinking effort: " + effort + ".\nChange it with /think " + strings.Join(efforts, "|") + "."}, nil
	}
	if level == "default" || level == "off" || level == "reset" {
		level = ""
	} else if !slices.Contains(levels, level) {
		return CommandResult{Reply: "Pick one of: " + strings.Join(levels, ", ") + ", or default."}, nil
	}
	if err := st.SetEffort(ctx, id, level); err != nil {
		return CommandResult{}, err
	}
	if level == "" {
		return CommandResult{Reply: "Back to the default thinking effort."}, nil
	}
	return CommandResult{Reply: "Thinking effort set to " + level + " for this session."}, nil
}

func (b *AppBackend) personality(ctx context.Context, id, arg string) (CommandResult, error) {
	ov, ok := b.A.Runner.Overlay.(state.Overlay)
	if !ok || b.A.State == nil {
		return CommandResult{Reply: "That command isn't available here."}, nil
	}
	names := ov.Personas.List()
	name := strings.ToLower(arg)
	if name == "" {
		cur, _ := b.A.State.Settings(ctx, id)
		p := cur.Personality
		if p == "" {
			p = "none (the agent's own)"
		}
		return CommandResult{Reply: "Persona: " + p + ".\nAvailable: " + strings.Join(names, ", ") + ".\nSwitch with /personality <name>, or /personality off."}, nil
	}
	if name == "off" || name == "default" || name == "none" {
		name = ""
	} else if _, err := ov.Personas.Text(name); err != nil {
		return CommandResult{Reply: fmt.Sprintf("I don't know the persona %q. Available: %s.", name, strings.Join(names, ", "))}, nil
	}
	if err := b.A.State.SetPersonality(ctx, id, name); err != nil {
		return CommandResult{}, err
	}
	if name == "" {
		return CommandResult{Reply: "Persona off; back to the agent's usual self."}, nil
	}
	return CommandResult{Reply: "Persona set to " + name + " for this session."}, nil
}

var _ Backend = (*AppBackend)(nil)
