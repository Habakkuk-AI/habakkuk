package agent

import (
	"context"
	"encoding/json"
)

// OriginFrom is the reply address of the turn running in ctx (its payload's
// Origin), so a tool that reports later, such as a meeting attendance,
// answers in the chat the turn came from, scheduled turns included.
func OriginFrom(ctx context.Context) json.RawMessage { return originFrom(ctx) }

// Poster names who sends a message to a chat from outside that chat's own
// turns, such as a scheduled script that delivers its output. The channel
// gateway notes such messages for the agent that answers the chat
// (Runner.Notes), and uses Poster to say where each one came from.
type Poster struct {
	Agent string // the agent it comes from, or ""
	// What sent it. With an agent it is a noun phrase that agent owns
	// (`scheduled task "inbox"`); without one it stands alone
	// ("`hk send`").
	What string
	// Session is the session whose turn sends it, or "". A chat's own
	// session needs no note: its turns are in its transcript.
	Session string
}

type posterKey struct{}

// WithPoster says who is sending the messages queued under ctx.
func WithPoster(ctx context.Context, p Poster) context.Context {
	return context.WithValue(ctx, posterKey{}, p)
}

// PosterFrom is the Poster set on ctx, or the zero Poster.
func PosterFrom(ctx context.Context) Poster {
	p, _ := ctx.Value(posterKey{}).(Poster)
	return p
}

// ChatNotes are messages that reached a chat from outside its own turns: a
// scheduled run's result, another agent's post, `hk send`. The agent
// answering the chat did not write them, so without them it cannot tell
// what the user is replying to. The channel gateway implements it.
type ChatNotes interface {
	// TakeNotes returns the notes the chat of a session has not shown yet,
	// as text for the start of a turn's user message, and claims them for
	// that turn's job: a rerun of the job gets the same notes. It returns
	// "" for a session that is not a chat's.
	TakeNotes(ctx context.Context, sessionID, jobID string) string
	// NotesSeen records that the job's user message, notes and all, is in
	// the transcript, so no later turn shows them again.
	NotesSeen(ctx context.Context, jobID string)
	// NotesTaken reports whether the job took any note: text that reached
	// the chat from elsewhere, which approval rules never act on.
	NotesTaken(ctx context.Context, jobID string) bool
}

type turnInfoKey struct{}

// TurnInfo says what started the running turn, for approvers and the
// turn policy (approval rules answer only for the owner's own turns).
type TurnInfo struct {
	Source string // SourceSchedule, SourceBoard, SourceA2A, or "" (TurnPayload.Source)
	Task   string // names that work in one line (`scheduled task "inbox"`)
	// Local: the owner started it on this machine (the CLI, the TUI, hk
	// talk); TurnPayload.Local.
	Local bool
	// Typed: the turn's whole message is plain text the owner just sent
	// (TurnPayload.Typed).
	Typed bool
	// Background: nobody sent it a fresh message (TurnPayload.Background);
	// Delegated: a sub-agent's task; Tainted: it starts tainted.
	Background, Delegated, Tainted bool
}

// OwnerMessage reports whether the turn answers a fresh message: not
// background work, a delegated task, or another system's request.
func (i TurnInfo) OwnerMessage() bool {
	return i.Source == "" && !i.Background && !i.Delegated
}

// WithTurnInfo notes what started the running turn. The runner sets it on
// every turn.
func WithTurnInfo(ctx context.Context, i TurnInfo) context.Context {
	return context.WithValue(ctx, turnInfoKey{}, i)
}

// TurnInfoFrom is what started the turn running in ctx.
func TurnInfoFrom(ctx context.Context) TurnInfo {
	i, _ := ctx.Value(turnInfoKey{}).(TurnInfo)
	return i
}

// SourceFrom is what started the turn running in ctx (SourceSchedule,
// SourceBoard, SourceA2A), or "" for a chat message or a local client.
func SourceFrom(ctx context.Context) string { return TurnInfoFrom(ctx).Source }

// TaskFrom names the work that started the turn (`scheduled task
// "inbox"`), or "".
func TaskFrom(ctx context.Context) string { return TurnInfoFrom(ctx).Task }

type taintedKey struct{}

// WithTainted marks what is delivered under ctx as outside content: a chat
// note it leaves is tainted (meeting notes, a tainted card's result).
func WithTainted(ctx context.Context) context.Context {
	return context.WithValue(ctx, taintedKey{}, true)
}

// TaintedFrom reports whether ctx was marked with WithTainted.
func TaintedFrom(ctx context.Context) bool {
	v, _ := ctx.Value(taintedKey{}).(bool)
	return v
}
