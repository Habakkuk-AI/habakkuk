package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
)

// metaSchema (transcripts.db version 4) adds session metadata: where the
// title came from, so an automatic title never replaces one the user set;
// an archived flag; and the session a fork or rewind was copied from.
const metaSchema = `
ALTER TABLE sessions ADD COLUMN title_source TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN archived INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN parent TEXT NOT NULL DEFAULT '';
`

// Title sources.
const (
	TitleAuto = "auto" // a short title a model wrote after the first turn
	TitleUser = "user" // set by the user; never replaced automatically
)

// SetTitleFrom sets a session's title. A TitleUser title always applies; a
// TitleAuto title applies only while the title still comes from the first
// message. It reports whether the title changed.
func (s *Store) SetTitleFrom(ctx context.Context, id, title, source string) (bool, error) {
	q := `UPDATE sessions SET title = ?, title_source = ? WHERE id = ?`
	if source != TitleUser {
		q += ` AND title_source = ''`
	}
	res, err := s.db.ExecContext(ctx, q, title, source, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetArchived archives or restores a session.
func (s *Store) SetArchived(ctx context.Context, id string, archived bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET archived = ? WHERE id = ?`, archived, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("session %s not found", id)
	}
	return nil
}

// Delete removes a session with its messages, compactions and search rows.
func (s *Store) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("session %s not found", id)
	}
	return nil
}

// Fork creates a session that starts as a copy of src's first upto
// messages: the same agent and frozen system prompt (so the prompt cache
// still applies), the messages verbatim, and the compactions that cover
// only those messages. src is not changed. The copy's messages belong to no
// turn job, and its title is title.
func (s *Store) Fork(ctx context.Context, src string, upto int, title string) (*Session, error) {
	orig, err := s.Get(ctx, src)
	if err != nil {
		return nil, err
	}
	if upto < 0 {
		return nil, errors.New("fork: negative length")
	}
	if orig.TitleSource == TitleImported {
		return nil, ErrImported
	}
	now := s.now()
	sess := &Session{ID: jobs.NewID("ses"), Agent: orig.Agent, Title: title, SystemPrompt: orig.SystemPrompt,
		CreatedAt: now, UpdatedAt: now, Parent: src}
	if title != "" {
		sess.TitleSource = TitleUser
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, agent, title, system_prompt, created_at, updated_at, title_source, parent)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, sess.ID, sess.Agent, title, sess.SystemPrompt, now.UnixMilli(), now.UnixMilli(),
		sess.TitleSource, src); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages (session_id, seq, role, blocks, model, usage, job_id, created_at)
		SELECT ?, seq, role, blocks, model, usage, '', created_at FROM messages WHERE session_id = ? AND seq < ? ORDER BY seq`,
		sess.ID, src, upto); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO compactions (session_id, keep_from, through, summary, model, usage, job_id, created_at)
		SELECT ?, keep_from, min(through, ?), summary, model, usage, '', created_at FROM compactions
		WHERE session_id = ? AND keep_from < ?`, sess.ID, upto, src, upto); err != nil {
		return nil, err
	}
	return sess, tx.Commit()
}

// HasTurns reports whether any agent has ever answered: some session holds
// an assistant message. The dashboard uses it to tell a fresh install from
// one in use.
func (s *Store) HasTurns(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM messages WHERE role = 'assistant')`).Scan(&n)
	return n == 1, err
}
