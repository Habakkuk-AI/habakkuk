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
}

type sourceKey struct{}

type source struct{ source, task string }

// WithSource notes what started the running turn (TurnPayload.Source and
// Task), so an approver can tell a scheduled run from a chat message. The
// runner sets it on every turn.
func WithSource(ctx context.Context, src, task string) context.Context {
	if src == "" {
		return ctx
	}
	return context.WithValue(ctx, sourceKey{}, source{src, task})
}

// SourceFrom is what started the turn running in ctx (SourceSchedule,
// SourceBoard, SourceA2A), or "" for a chat message or a local client.
func SourceFrom(ctx context.Context) string {
	s, _ := ctx.Value(sourceKey{}).(source)
	return s.source
}

// TaskFrom names the work that started the turn (`scheduled task
// "inbox"`), or "".
func TaskFrom(ctx context.Context) string {
	s, _ := ctx.Value(sourceKey{}).(source)
	return s.task
}
