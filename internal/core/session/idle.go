package session

import (
	"context"
	"time"
)

// Activity is a session's agent, size and last change, for background work
// that follows conversations (memory review).
type Activity struct {
	ID        string
	Agent     string
	Messages  int // the transcript's length: the next message's seq
	UpdatedAt time.Time
}

// Quiet lists sessions last changed between from and to, oldest change
// first, at most limit of them. Sessions imported from another agent are
// left out: their text was never part of a conversation with this agent,
// so it is not for memory review.
func (s *Store) Quiet(ctx context.Context, from, to time.Time, limit int) ([]Activity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id, s.agent, s.updated_at,
			COALESCE((SELECT max(m.seq) + 1 FROM messages m WHERE m.session_id = s.id), 0)
		FROM sessions s WHERE s.updated_at >= ? AND s.updated_at < ?
			AND s.id NOT IN (SELECT session_id FROM imported_sessions)
		ORDER BY s.updated_at LIMIT ?`, from.UnixMilli(), to.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Activity
	for rows.Next() {
		var a Activity
		var at int64
		if err := rows.Scan(&a.ID, &a.Agent, &at, &a.Messages); err != nil {
			return nil, err
		}
		a.UpdatedAt = time.UnixMilli(at)
		out = append(out, a)
	}
	return out, rows.Err()
}
