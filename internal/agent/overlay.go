package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// Overlay is per-session state that chat commands and tools change without
// touching the session itself: an effort override, a persona, a task list
// and guidance queued for the running turn. The frozen system prompt and the
// append-only transcript stay as they are; the overlay only adds to them.
type Overlay interface {
	// Effort is the session's effort override, or "" for the agent's own.
	Effort(ctx context.Context, sessionID string) string
	// System is appended to the frozen system prompt (a persona overlay), or
	// "". It changes rarely, so the cached prefix survives between changes.
	System(ctx context.Context, sessionID string) string
	// Preamble is added to a turn's user message when it is written (the
	// task list), or "".
	Preamble(ctx context.Context, sessionID string) string
	// Steer takes the guidance queued for the session's running turn. Taken
	// notes are gone; each is delivered once.
	Steer(ctx context.Context, sessionID string) []string
}

// Hook events the runner fires. Channels and schedules fire their own
// (message_in, message_out, schedule_run) through Runner.Fire.
const (
	HookTurnStart  = "turn_start"
	HookTurnEnd    = "turn_end"
	HookToolBefore = "tool_before"
	HookToolAfter  = "tool_after"
	HookSessionNew = "session_new"
)

// Hooks runs user-configured commands on events. subject is what a hook's
// match is tested against (a tool, agent, channel or schedule name). Fire
// waits only for events that can veto (tool_before) and returns the reason
// of the first veto, or "".
type Hooks interface {
	Fire(ctx context.Context, event, subject string, payload any) (veto string)
}

// Fire runs the configured hooks for an event. It is safe on a nil runner
// and with no hooks configured.
func (r *Runner) Fire(ctx context.Context, event, subject string, payload any) string {
	if r == nil || r.Hooks == nil {
		return ""
	}
	return r.Hooks.Fire(ctx, event, subject, payload)
}

func (r *Runner) effort(ctx context.Context, spec *Spec, sessionID string) string {
	if r.Overlay != nil {
		if e := r.Overlay.Effort(ctx, sessionID); e != "" {
			return e
		}
	}
	return spec.Effort
}

func (r *Runner) system(ctx context.Context, frozen, sessionID string) string {
	if r.Overlay != nil {
		if extra := r.Overlay.System(ctx, sessionID); extra != "" {
			return frozen + "\n\n" + extra
		}
	}
	return frozen
}

func (r *Runner) preamble(ctx context.Context, sessionID string) string {
	if r.Overlay == nil {
		return ""
	}
	return r.Overlay.Preamble(ctx, sessionID)
}

// SteerPrefix starts each steering note in the transcript.
const SteerPrefix = "[Guidance from the user while you were working; take it into account from here on]\n"

// steer turns queued guidance into text blocks for the next user message.
func (r *Runner) steer(ctx context.Context, sessionID string) []llm.Block {
	if r.Overlay == nil {
		return nil
	}
	var out []llm.Block
	for _, note := range r.Overlay.Steer(ctx, sessionID) {
		out = append(out, llm.Block{Type: llm.Text, Text: SteerPrefix + note})
	}
	return out
}

// TurnHookPayload is the JSON a turn_start or turn_end hook reads.
type TurnHookPayload struct {
	Event   string `json:"event"`
	Agent   string `json:"agent"`
	Session string `json:"session"`
	Job     string `json:"job"`
	Text    string `json:"text,omitempty"`  // the user message (turn_start)
	Final   string `json:"final,omitempty"` // the reply (turn_end)
	Error   string `json:"error,omitempty"`
}

// ToolHookPayload is the JSON a tool_before or tool_after hook reads.
type ToolHookPayload struct {
	Event   string `json:"event"`
	Agent   string `json:"agent"`
	Session string `json:"session"`
	Tool    string `json:"tool"`
	CallID  string `json:"call_id"`
	Input   any    `json:"input"`
	Result  string `json:"result,omitempty"` // tool_after
	IsError bool   `json:"is_error,omitempty"`
}

// runTool runs one call between its tool_before and tool_after hooks. A
// tool_before hook that vetoes turns the call into an error result carrying
// its reason, so the model can take another route.
// ran is how long the tool itself ran: hooks and approval waits are left
// out (tools.WithRunTime), as the loop guard needs.
func (t *turn) runTool(env *tools.Env, c llm.Block) (res llm.Block, media []llm.Block, ran time.Duration) {
	p := ToolHookPayload{Event: HookToolBefore, Agent: t.spec.Name, Session: t.sess.ID, Tool: c.ToolName,
		CallID: c.ToolUseID, Input: rawJSON(c.Input)}
	if veto := t.r.Fire(t.ctx, HookToolBefore, c.ToolName, p); veto != "" {
		return llm.Block{Type: llm.ToolResult, ToolUseID: c.ToolUseID, IsError: true,
			Text: "blocked by a tool_before hook: " + veto}, nil, 0
	}
	ctx, dur := tools.WithRunTime(t.ctx)
	res, media = t.r.Tools.ExecuteMedia(ctx, env, c)
	p.Event, p.Result, p.IsError = HookToolAfter, res.Text, res.IsError
	t.r.Fire(t.ctx, HookToolAfter, c.ToolName, p)
	return res, media, *dur
}

// rawJSON keeps valid JSON input as JSON in hook payloads.
func rawJSON(b []byte) any {
	if json.Valid(b) {
		return json.RawMessage(b)
	}
	return string(b)
}
