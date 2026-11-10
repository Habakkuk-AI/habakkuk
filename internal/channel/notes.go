package channel

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/core/state"
)

// Chat notes. A chat's session holds only the turns that chat started, but
// other messages land in the same chat: a scheduled run delivers its result
// there, another agent posts to its home chat, `hk send` and
// background results arrive. When the user answers one of those, the agent
// must know it was sent. So every delivery that is not the chat's own turn
// leaves a note (state.db), and the next turn of the chat's session gets
// the unseen notes at the start of its user message, labelled with who sent
// them and when. The transcript stays append-only: notes join a new user
// message, never a running turn.
//
// A turn claims its notes by job id before writing its message, and marks
// them seen once the message is in the transcript (agent.ChatNotes), so a
// turn that crashes before then shows the same notes when it runs again,
// and no turn shows a note twice.

const (
	maxNoteStored  = 4000 // runes of one delivery kept as a note
	maxNoteShown   = 1200 // runes of one note shown to the agent
	maxNotesShown  = 6000 // runes of notes put before one user message
	noteClaimStale = time.Hour
)

// recordNote notes a sent delivery for the chat it went to, unless the
// chat's own session wrote it (that is in the transcript already). A
// failure only logs: the message did go out.
func (g *Gateway) recordNote(ctx context.Context, jobID string, p deliverPayload) {
	if g.State == nil || g.Store == nil {
		return
	}
	text := strings.TrimSpace(p.Text)
	if p.File != nil {
		if p.File.Voice {
			return // a spoken reply; its text goes as its own delivery
		}
		text = strings.TrimSpace("(file " + p.File.Name + ") " + text)
	}
	if text == "" {
		return
	}
	to := g.chat(p.To)
	if p.Session != "" {
		if conv, err := g.Store.Conversation(ctx, to); err == nil && conv != nil && conv.SessionID == p.Session {
			return
		}
	}
	if utf8.RuneCountInString(text) > maxNoteStored {
		text = clip(text, maxNoteStored)
	}
	if err := g.State.AddChatNote(ctx, state.ChatNote{Conversation: to.key(), Delivery: jobID,
		Agent: p.Agent, What: p.What, Text: text, At: g.now(), Tainted: p.Tainted}); err != nil {
		g.Log.Warn("record chat note failed", "channel", to.Account, "err", err)
	}
}

// chat is the conversation an address names: no reply target, and an
// alias (whatsappagent's "creator") resolved to the chat it stands for.
func (g *Gateway) chat(a Address) Address {
	to := Address{Account: a.Account, Conversation: a.Conversation, Thread: a.Thread}
	if al, ok := g.Adapters[to.Account].(Aliaser); ok {
		if conv, ok := al.Resolve(to.Conversation); ok {
			to.Conversation = conv
		}
	}
	return to
}

// TakeNotes implements agent.ChatNotes.
func (g *Gateway) TakeNotes(ctx context.Context, sessionID, jobID string) string {
	if g.State == nil || g.Store == nil {
		return ""
	}
	conv, err := g.Store.BySession(ctx, sessionID)
	if err != nil || conv == nil {
		return ""
	}
	notes, err := g.State.ClaimChatNotes(ctx, conv.key(), jobID, g.now().Add(-noteClaimStale))
	if err != nil {
		g.Log.Warn("read chat notes failed", "session", sessionID, "err", err)
		return ""
	}
	return renderNotes(notes, conv.Agent, g.now(), g.location())
}

// NotesTaken implements agent.ChatNotes.
func (g *Gateway) NotesTaken(ctx context.Context, jobID string) bool {
	if g.State == nil {
		return false
	}
	taken, err := g.State.ChatNotesTaken(ctx, jobID)
	if err != nil {
		g.Log.Warn("read chat notes failed", "job", jobID, "err", err)
		return true // unknown: no rules for this turn
	}
	return taken
}

// NotesSeen implements agent.ChatNotes.
func (g *Gateway) NotesSeen(ctx context.Context, jobID string) {
	if g.State == nil {
		return
	}
	if err := g.State.ChatNotesSeen(ctx, jobID); err != nil {
		g.Log.Warn("mark chat notes seen failed", "job", jobID, "err", err)
	}
}

func (g *Gateway) location() *time.Location {
	if g.Runner != nil && g.Runner.Location != nil {
		return g.Runner.Location
	}
	return time.Local
}

// renderNotes lists notes one per line, oldest first, keeping the newest
// that fit in maxNotesShown and saying how many older ones were left out.
func renderNotes(notes []state.ChatNote, reader string, now time.Time, loc *time.Location) string {
	var lines []string
	total := 0
	for i := len(notes) - 1; i >= 0; i-- {
		line := noteLine(notes[i], reader, now, loc)
		n := utf8.RuneCountInString(line)
		if total+n > maxNotesShown && len(lines) > 0 {
			break
		}
		lines, total = append(lines, line), total+n
	}
	slices.Reverse(lines)
	switch dropped := len(notes) - len(lines); {
	case dropped == 1:
		lines = append([]string{"[1 older message sent in this chat is not shown.]"}, lines...)
	case dropped > 1:
		lines = append([]string{fmt.Sprintf("[%d older messages sent in this chat are not shown.]", dropped)}, lines...)
	}
	return strings.Join(lines, "\n")
}

// noteLine is one note as the agent reads it:
// [Earlier in this chat, at 18:44, your scheduled task "inbox" sent: "..."]
func noteLine(n state.ChatNote, reader string, now time.Time, loc *time.Location) string {
	at, today := n.At.In(loc), now.In(loc)
	when := "at " + at.Format("15:04")
	if at.YearDay() != today.YearDay() || at.Year() != today.Year() {
		when = "on " + at.Format("Mon 2006-01-02") + " at " + at.Format("15:04")
	}
	text := n.Text
	if utf8.RuneCountInString(text) > maxNoteShown {
		text = clip(text, maxNoteShown)
	}
	return fmt.Sprintf("[Earlier in this chat, %s, %s sent: \"%s\"]", when, noteSender(n, reader), text)
}

// noteSender names who sent a note, from the reading agent's side.
func noteSender(n state.ChatNote, reader string) string {
	switch {
	case n.Agent != "" && n.Agent == reader && n.What != "":
		return "your " + n.What
	case n.Agent != "" && n.Agent == reader:
		return "you (from another session)"
	case n.Agent != "" && n.What != "":
		return "agent " + n.Agent + "'s " + n.What
	case n.Agent != "":
		return "agent " + n.Agent
	case n.What != "":
		return n.What
	}
	return "Habakkuk"
}

// poster fills in who sends a delivery from ctx when the payload does not
// say: the turn that is running (HandleTurn sets it) or the caller of
// Deliver and SendText.
func (p *deliverPayload) poster(ctx context.Context) {
	if p.Session != "" || p.Agent != "" || p.What != "" {
		return
	}
	who := agent.PosterFrom(ctx)
	p.Session, p.Agent, p.What = who.Session, who.Agent, who.What
}
