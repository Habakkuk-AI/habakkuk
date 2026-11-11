package state

import (
	"context"
	"time"
)

// Chat notes are messages that reached a chat from outside its own turns:
// a scheduled run's result, another agent's post to its home chat, a
// message sent with `hk send`. The agent that answers the chat never
// wrote them, so they are not in its transcript. Each note waits here until
// a turn of that chat claims it and writes it into its user message.
//
// A claim names the turn's job. A rerun of the same job (after a crash)
// claims the same notes again, so they still reach the transcript; once the
// turn's user message is written, the notes are marked seen and no later
// turn shows them. A claim that was never confirmed goes stale after a
// while, so a turn that died for good does not hide its notes forever.

// ChatNote is one message sent to a chat by something other than the
// chat's own turns.
type ChatNote struct {
	ID           int64
	Conversation string // the chat's key: account|conversation|thread
	Delivery     string // the delivery job that sent it; one note per delivery
	Agent        string // the agent it came from, or ""
	What         string // what sent it, e.g. `scheduled task "inbox"`, or ""
	Text         string
	At           time.Time
	// Tainted: outside content may have steered the turn that sent it, so
	// the turn that takes it in gets no approval rules.
	Tainted bool
}

// AddChatNote records a note. A second note for the same delivery is
// ignored, so a retried delivery records once.
func (s *Store) AddChatNote(ctx context.Context, n ChatNote) error {
	at := n.At
	if at.IsZero() {
		at = s.now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_notes (conversation, delivery, agent, what, text, created_at, tainted)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (delivery) DO NOTHING`,
		n.Conversation, n.Delivery, n.Agent, n.What, n.Text, at.UnixMilli(), n.Tainted)
	return err
}

// ClaimChatNotes claims a chat's unseen notes for a turn and returns every
// note that turn holds, oldest first: notes nobody claimed, notes it claimed
// on an earlier attempt, and notes whose claim is older than staleBefore and
// was never confirmed.
func (s *Store) ClaimChatNotes(ctx context.Context, conversation, turn string, staleBefore time.Time) ([]ChatNote, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE chat_notes SET turn = ?, claimed_at = ?
		WHERE conversation = ? AND seen = 0 AND (turn = '' OR (turn != ? AND claimed_at < ?))`,
		turn, s.now().UnixMilli(), conversation, turn, staleBefore.UnixMilli()); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, delivery, agent, what, text, created_at, tainted FROM chat_notes
		WHERE conversation = ? AND turn = ? AND seen = 0 ORDER BY created_at, id`, conversation, turn)
	if err != nil {
		return nil, err
	}
	var out []ChatNote
	for rows.Next() {
		n := ChatNote{Conversation: conversation}
		var at int64
		if err := rows.Scan(&n.ID, &n.Delivery, &n.Agent, &n.What, &n.Text, &at, &n.Tainted); err != nil {
			rows.Close()
			return nil, err
		}
		n.At = time.UnixMilli(at)
		out = append(out, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// ChatNotesSeen marks a turn's claimed notes as written to its transcript.
func (s *Store) ChatNotesSeen(ctx context.Context, turn string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE chat_notes SET seen = 1 WHERE turn = ? AND seen = 0`, turn)
	return err
}

// ChatNotesTaken reports whether the turn claimed any note, seen or not.
func (s *Store) ChatNotesTaken(ctx context.Context, turn string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM chat_notes WHERE turn = ?`, turn).Scan(&n)
	return n > 0, err
}
