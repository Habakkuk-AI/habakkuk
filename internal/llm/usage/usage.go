// Package usage records the tokens every model call spends and turns them
// into totals and costs. It keeps its own small database, usage.db, which
// also holds per-session model choices made with /model.
package usage

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

var Migrations = []store.Migration{{Version: 1, SQL: `
CREATE TABLE calls (
	id          INTEGER PRIMARY KEY,
	at          INTEGER NOT NULL,
	provider    TEXT NOT NULL,
	model       TEXT NOT NULL,
	agent       TEXT NOT NULL DEFAULT '',
	session     TEXT NOT NULL DEFAULT '',
	input       INTEGER NOT NULL,
	output      INTEGER NOT NULL,
	cache_read  INTEGER NOT NULL,
	cache_write INTEGER NOT NULL
);
CREATE INDEX calls_at ON calls (at);
CREATE INDEX calls_session ON calls (session);
CREATE TABLE session_models (
	session    TEXT PRIMARY KEY,
	model      TEXT NOT NULL,
	updated_at INTEGER NOT NULL
) WITHOUT ROWID;
`}}

// Call is one successful model call.
type Call struct {
	At       time.Time
	Provider string // configured provider name
	Model    string
	Agent    string
	Session  string
	llm.Usage
}

type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (creating if needed) usage.db at path.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := store.Open(ctx, path, Migrations)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Record stores one call. A zero At means now.
func (s *Store) Record(ctx context.Context, c Call) error {
	if c.At.IsZero() {
		c.At = s.now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO calls (at, provider, model, agent, session, input, output, cache_read, cache_write)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, c.At.UnixMilli(), c.Provider, c.Model, c.Agent, c.Session,
		c.InputTokens, c.OutputTokens, c.CacheReadTokens, c.CacheWriteTokens)
	return err
}

// Filter selects calls; zero fields match everything.
type Filter struct {
	Since   time.Time
	Agent   string
	Session string
}

// Calls returns the calls matching f, oldest first.
func (s *Store) Calls(ctx context.Context, f Filter) ([]Call, error) {
	var since int64
	if !f.Since.IsZero() {
		since = f.Since.UnixMilli()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT at, provider, model, agent, session, input, output, cache_read, cache_write
		FROM calls WHERE at >= ? AND (? = '' OR agent = ?) AND (? = '' OR session = ?) ORDER BY at, id`,
		since, f.Agent, f.Agent, f.Session, f.Session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Call
	for rows.Next() {
		var c Call
		var at int64
		if err := rows.Scan(&at, &c.Provider, &c.Model, &c.Agent, &c.Session,
			&c.InputTokens, &c.OutputTokens, &c.CacheReadTokens, &c.CacheWriteTokens); err != nil {
			return nil, err
		}
		c.At = time.UnixMilli(at)
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetSessionModel stores the model a session uses from now on, as
// "<provider>/<model>"; "" goes back to the agent's own models.
func (s *Store) SetSessionModel(ctx context.Context, session, ref string) error {
	if ref == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM session_models WHERE session = ?`, session)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO session_models (session, model, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (session) DO UPDATE SET model = excluded.model, updated_at = excluded.updated_at`,
		session, ref, s.now().UnixMilli())
	return err
}

// SessionModel returns a session's chosen model, or "" for none.
func (s *Store) SessionModel(ctx context.Context, session string) (string, error) {
	var ref string
	err := s.db.QueryRowContext(ctx, `SELECT model FROM session_models WHERE session = ?`, session).Scan(&ref)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return ref, err
}

// Prune deletes calls, and model choices not touched, since the cutoff.
func (s *Store) Prune(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM calls WHERE at < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM session_models WHERE updated_at < ?`, before.UnixMilli()); err != nil {
		return n, err
	}
	return n, nil
}

// DB exposes the database for maintenance (checkpoints, integrity checks).
func (s *Store) DB() *sql.DB { return s.db }
