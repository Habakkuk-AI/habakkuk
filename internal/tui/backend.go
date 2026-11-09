// Package tui is Habakkuk's full-screen terminal interface: a scrolling
// transcript with Markdown, live streaming of text, thinking and tool calls,
// inline approvals and questions, slash commands with completion, and a
// status bar. It drives a Backend, which AppBackend implements over a
// running app; the model itself holds no global state and is tested by
// feeding it messages.
package tui

import (
	"context"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// Backend is what the terminal UI needs from a running Habakkuk.
type Backend interface {
	// Agents lists the configured agents, sorted.
	Agents() []string
	NewSession(ctx context.Context, agent string) (SessionInfo, error)
	Session(ctx context.Context, id string) (SessionInfo, error)
	RecentSessions(ctx context.Context, limit int) ([]SessionInfo, error)
	Transcript(ctx context.Context, id string) ([]session.Record, error)
	// Busy reports whether the session has a turn queued or running.
	Busy(ctx context.Context, id string) bool
	// Submit queues a user message as a turn of the session.
	Submit(ctx context.Context, id, text string) error
	// Cancel stops the session's queued and running turns and returns how
	// many it stopped.
	Cancel(ctx context.Context, id string) int
	Subscribe(id string) (<-chan events.Event, func())
	// Attach makes a the session's approver while the UI shows it.
	Attach(id string, a tools.Approver) func()
	Status(ctx context.Context, id string) Status
	// Command runs one of the commands marked backend in the registry.
	Command(ctx context.Context, id, name, arg string) (CommandResult, error)
	// FollowUp queues guidance sent with /steer that the finished turn did
	// not take in, as a turn of its own; it reports whether it queued one.
	FollowUp(ctx context.Context, id string) bool
	// Export writes the transcript as Markdown to path (a default name when
	// empty) and returns the path written.
	Export(ctx context.Context, id, path string) (string, error)
}

// SessionInfo describes a session for the status bar and the picker.
type SessionInfo struct {
	ID, Agent, Title string
	UpdatedAt        time.Time
}

// Status is the model a session uses and what it has cost so far.
type Status struct {
	Model   string
	Calls   int
	In, Out int64  // tokens: prompt (cache included) and output
	Cost    string // formatted, e.g. "$0.04"; "" when unknown
}

// CommandResult is a command's reply. Started means it queued a turn, so
// the UI should show itself busy.
type CommandResult struct {
	Reply   string
	Started bool
}
