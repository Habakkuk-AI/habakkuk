package web

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// Migrations returns web.db's schema steps, for version reports and upgrade
// checks.
func Migrations() []store.Migration { return migrations }

// migrations for web.db, which keeps the web chat's message log so a client
// that reconnects (or opens the chat on another device) can catch up.
var migrations = []store.Migration{{Version: 1, SQL: `
CREATE TABLE messages (
	seq          INTEGER PRIMARY KEY AUTOINCREMENT,
	conversation TEXT NOT NULL,
	role         TEXT NOT NULL,
	ref          TEXT NOT NULL DEFAULT '',
	text         TEXT NOT NULL,
	at           INTEGER NOT NULL
);
CREATE INDEX messages_conversation ON messages (conversation, seq);
CREATE UNIQUE INDEX messages_ref ON messages (conversation, ref) WHERE ref != '';
CREATE INDEX messages_at ON messages (at);
`}, {Version: 2, SQL: `
CREATE TABLE api_sessions (
	key        TEXT PRIMARY KEY,
	agent      TEXT NOT NULL,
	session_id TEXT NOT NULL,
	at         INTEGER NOT NULL
) WITHOUT ROWID;
`}}

// Event is one message in a web conversation, as streamed to clients.
type Event struct {
	Seq  int64     `json:"seq"`
	Role string    `json:"role"` // user | assistant
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// Store is the web chat's message log. Sequence numbers are global and only
// grow, so "everything after N" is a cheap, exact catch-up query.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

func OpenStore(ctx context.Context, path string) (*Store, error) {
	db, err := store.Open(ctx, path, migrations)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Append records a message. A non-empty ref (the client's message id) makes
// it idempotent: a second append with the same ref adds nothing.
func (s *Store) Append(ctx context.Context, conversation, role, ref, text string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO messages (conversation, role, ref, text, at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, conversation, role, ref, text, s.now().UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// After returns up to limit messages of a conversation with seq > after.
func (s *Store) After(ctx context.Context, conversation string, after int64, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, role, text, at FROM messages
		WHERE conversation = ? AND seq > ? ORDER BY seq LIMIT ?`, conversation, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.Seq, &e.Role, &e.Text, &at); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// Prune deletes messages, and API session keys unused since, older than the
// cutoff.
func (s *Store) Prune(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE at < ?`, olderThan.UnixMilli())
	if err != nil {
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM api_sessions WHERE at < ?`, olderThan.UnixMilli()); err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// APISession returns the agent and session an API client's key (the
// OpenAI-style "user" field) is bound to, or empty strings.
func (s *Store) APISession(ctx context.Context, key string) (agent, sessionID string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT agent, session_id FROM api_sessions WHERE key = ?`, key).Scan(&agent, &sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return agent, sessionID, err
}

// BindAPISession points an API client's key at a session.
func (s *Store) BindAPISession(ctx context.Context, key, agent, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_sessions (key, agent, session_id, at) VALUES (?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET agent = excluded.agent, session_id = excluded.session_id, at = excluded.at`,
		key, agent, sessionID, s.now().UnixMilli())
	return err
}

// TouchAPISession keeps a key in use from being pruned.
func (s *Store) TouchAPISession(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_sessions SET at = ? WHERE key = ?`, s.now().UnixMilli(), key)
	return err
}
