// Package state keeps the small per-session and per-conversation settings
// that chat commands and tools change while a conversation goes on: effort
// and persona overrides, standing goals, steering notes for a running turn,
// task lists, a conversation's chosen agent, progress drafts, and notes of
// messages a chat got from outside its own turns (notes.go). It lives
// in its own database, state.db, so it never migrates control.db, and
// everything in it can be lost without harming a transcript.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

var Migrations = []store.Migration{{Version: 1, SQL: `
CREATE TABLE settings (
	session_id  TEXT PRIMARY KEY,
	effort      TEXT NOT NULL DEFAULT '',
	personality TEXT NOT NULL DEFAULT '',
	updated_at  INTEGER NOT NULL
) WITHOUT ROWID;

CREATE TABLE goals (
	session_id TEXT PRIMARY KEY,
	text       TEXT NOT NULL,
	budget     INTEGER NOT NULL,
	turns      INTEGER NOT NULL DEFAULT 0,
	last_job   TEXT NOT NULL DEFAULT '',
	origin     TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL
) WITHOUT ROWID;

CREATE TABLE steers (
	id         INTEGER PRIMARY KEY,
	session_id TEXT NOT NULL,
	text       TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX steers_session ON steers (session_id, id);

CREATE TABLE todos (
	session_id TEXT NOT NULL,
	id         INTEGER NOT NULL,
	text       TEXT NOT NULL,
	status     TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (session_id, id)
) WITHOUT ROWID;

CREATE TABLE conversation_agents (
	conversation TEXT PRIMARY KEY,
	agent        TEXT NOT NULL,
	updated_at   INTEGER NOT NULL
) WITHOUT ROWID;

CREATE TABLE drafts (
	turn_id    TEXT PRIMARY KEY,
	address    TEXT NOT NULL,
	message_id TEXT NOT NULL,
	created_at INTEGER NOT NULL
) WITHOUT ROWID;
`}, {Version: 2, SQL: `
CREATE TABLE chat_notes (
	id           INTEGER PRIMARY KEY,
	conversation TEXT NOT NULL,
	delivery     TEXT NOT NULL UNIQUE,
	agent        TEXT NOT NULL DEFAULT '',
	what         TEXT NOT NULL DEFAULT '',
	text         TEXT NOT NULL,
	created_at   INTEGER NOT NULL,
	turn         TEXT NOT NULL DEFAULT '',
	claimed_at   INTEGER NOT NULL DEFAULT 0,
	seen         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX chat_notes_open ON chat_notes (conversation, seen);
CREATE INDEX chat_notes_turn ON chat_notes (turn);
`}, {Version: 3, SQL: `
CREATE TABLE asked_approvals (
	conversation TEXT PRIMARY KEY,
	hash         TEXT NOT NULL,
	created_at   INTEGER NOT NULL
) WITHOUT ROWID;
`}, {Version: 4, SQL: `
ALTER TABLE drafts ADD COLUMN answer TEXT NOT NULL DEFAULT '';
`}}

type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (creating if needed) state.db at path.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := store.Open(ctx, path, Migrations)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}

func (s *Store) Close()      { s.db.Close() }
func (s *Store) DB() *sql.DB { return s.db }

// Settings are a session's overrides; empty fields mean the agent's own.
type Settings struct {
	Effort      string
	Personality string
}

func (s *Store) Settings(ctx context.Context, sessionID string) (Settings, error) {
	var st Settings
	err := s.db.QueryRowContext(ctx, `SELECT effort, personality FROM settings WHERE session_id = ?`, sessionID).
		Scan(&st.Effort, &st.Personality)
	if errors.Is(err, sql.ErrNoRows) {
		return Settings{}, nil
	}
	return st, err
}

func (s *Store) SetEffort(ctx context.Context, sessionID, effort string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (session_id, effort, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (session_id) DO UPDATE SET effort = excluded.effort, updated_at = excluded.updated_at`,
		sessionID, effort, s.now().UnixMilli())
	return err
}

func (s *Store) SetPersonality(ctx context.Context, sessionID, name string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (session_id, personality, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (session_id) DO UPDATE SET personality = excluded.personality, updated_at = excluded.updated_at`,
		sessionID, name, s.now().UnixMilli())
	return err
}

// Goal is a standing goal: after each turn the session is prompted again
// until the agent reports it done or Budget turns have run.
type Goal struct {
	Text   string
	Budget int
	Turns  int    // goal turns run so far
	Origin string // the chat's reply address, as JSON
}

func (s *Store) SetGoal(ctx context.Context, sessionID, text string, budget int, origin string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO goals (session_id, text, budget, turns, last_job, origin, updated_at)
		VALUES (?, ?, ?, 0, '', ?, ?) ON CONFLICT (session_id) DO UPDATE SET text = excluded.text,
		budget = excluded.budget, turns = 0, last_job = '', origin = excluded.origin, updated_at = excluded.updated_at`,
		sessionID, text, budget, origin, s.now().UnixMilli())
	return err
}

// Goal returns the session's goal, or nil.
func (s *Store) Goal(ctx context.Context, sessionID string) (*Goal, error) {
	var g Goal
	err := s.db.QueryRowContext(ctx, `SELECT text, budget, turns, origin FROM goals WHERE session_id = ?`, sessionID).
		Scan(&g.Text, &g.Budget, &g.Turns, &g.Origin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// ClearGoal removes the goal and reports whether there was one.
func (s *Store) ClearGoal(ctx context.Context, sessionID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM goals WHERE session_id = ?`, sessionID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// CountGoalTurn records that the turn jobID finished while the goal was
// set and returns the goal afterwards. Counting the same job twice (a turn
// that reran after a crash) counts it once.
func (s *Store) CountGoalTurn(ctx context.Context, sessionID, jobID string) (*Goal, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE goals SET turns = turns + 1, last_job = ?, updated_at = ?
		WHERE session_id = ? AND last_job != ?`, jobID, s.now().UnixMilli(), sessionID, jobID); err != nil {
		return nil, err
	}
	return s.Goal(ctx, sessionID)
}

// AddSteer queues guidance for the session's running turn.
func (s *Store) AddSteer(ctx context.Context, sessionID, text string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO steers (session_id, text, created_at) VALUES (?, ?, ?)`,
		sessionID, text, s.now().UnixMilli())
	return err
}

// TakeSteers removes and returns the session's queued guidance, oldest first.
func (s *Store) TakeSteers(ctx context.Context, sessionID string) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT text FROM steers WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(out) == 0 {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM steers WHERE session_id = ?`, sessionID); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// Todo statuses.
const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoDone       = "done"
)

type Todo struct {
	ID     int
	Text   string
	Status string
}

// MaxTodos bounds one session's task list.
const MaxTodos = 50

// AddTodos appends items to the session's task list, numbered after the
// highest existing id.
func (s *Store) AddTodos(ctx context.Context, sessionID string, items []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n, next int
	if err := tx.QueryRowContext(ctx, `SELECT count(*), COALESCE(MAX(id), 0) + 1 FROM todos WHERE session_id = ?`,
		sessionID).Scan(&n, &next); err != nil {
		return err
	}
	if n+len(items) > MaxTodos {
		return fmt.Errorf("the task list holds at most %d items; complete or clear some first", MaxTodos)
	}
	now := s.now().UnixMilli()
	for _, it := range items {
		if it = strings.TrimSpace(it); it == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO todos (session_id, id, text, status, updated_at) VALUES (?, ?, ?, ?, ?)`,
			sessionID, next, it, TodoPending, now); err != nil {
			return err
		}
		next++
	}
	return tx.Commit()
}

// UpdateTodo changes an item's text and/or status; empty leaves it as is.
func (s *Store) UpdateTodo(ctx context.Context, sessionID string, id int, text, status string) error {
	switch status {
	case "", TodoPending, TodoInProgress, TodoDone:
	default:
		return fmt.Errorf("status %q must be pending, in_progress or done", status)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE todos SET
		text = CASE WHEN ? = '' THEN text ELSE ? END,
		status = CASE WHEN ? = '' THEN status ELSE ? END,
		updated_at = ? WHERE session_id = ? AND id = ?`,
		text, text, status, status, s.now().UnixMilli(), sessionID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no task %d in this session's list", id)
	}
	return nil
}

func (s *Store) Todos(ctx context.Context, sessionID string) ([]Todo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, text, status FROM todos WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Todo
	for rows.Next() {
		var t Todo
		if err := rows.Scan(&t.ID, &t.Text, &t.Status); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) ClearTodos(ctx context.Context, sessionID string, doneOnly bool) error {
	q := `DELETE FROM todos WHERE session_id = ?`
	if doneOnly {
		q += ` AND status = 'done'`
	}
	_, err := s.db.ExecContext(ctx, q, sessionID)
	return err
}

// ConversationAgent is the agent picked for a conversation with /agent, or "".
func (s *Store) ConversationAgent(ctx context.Context, key string) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT agent FROM conversation_agents WHERE conversation = ?`, key).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return name, err
}

// SetConversationAgent picks an agent for a conversation; "" goes back to
// the channel's routing.
func (s *Store) SetConversationAgent(ctx context.Context, key, agent string) error {
	if agent == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM conversation_agents WHERE conversation = ?`, key)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO conversation_agents (conversation, agent, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (conversation) DO UPDATE SET agent = excluded.agent, updated_at = excluded.updated_at`,
		key, agent, s.now().UnixMilli())
	return err
}

// Draft is a progress message posted for a turn, kept so a rerun of the
// turn edits the same message and its delivery can remove it, or, when it
// shows the reply being written, turn it into the reply.
type Draft struct {
	Address   string // the chat, as JSON
	MessageID string
	// Answer is the reply text the message shows while it is streamed, as
	// Markdown; empty while it shows progress.
	Answer string
}

func (s *Store) SaveDraft(ctx context.Context, turnID string, d Draft) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO drafts (turn_id, address, message_id, answer, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (turn_id) DO UPDATE SET address = excluded.address, message_id = excluded.message_id, answer = excluded.answer`,
		turnID, d.Address, d.MessageID, d.Answer, s.now().UnixMilli())
	return err
}

// Draft returns the turn's draft, or nil.
func (s *Store) Draft(ctx context.Context, turnID string) (*Draft, error) {
	var d Draft
	err := s.db.QueryRowContext(ctx, `SELECT address, message_id, answer FROM drafts WHERE turn_id = ?`, turnID).
		Scan(&d.Address, &d.MessageID, &d.Answer)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *Store) DeleteDraft(ctx context.Context, turnID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM drafts WHERE turn_id = ?`, turnID)
	return err
}

// An approval prompt that is waiting in a chat. The answer itself is held
// in memory by the process that asked; this row is what survives a restart,
// so the next process knows a "yes" in that chat answers a prompt nobody is
// waiting on any more, and can say so instead of taking it as a new message.

// SaveAsked records that the conversation has an approval prompt waiting,
// for the call with the given hash. A newer prompt replaces the record.
func (s *Store) SaveAsked(ctx context.Context, conversation, hash string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO asked_approvals (conversation, hash, created_at) VALUES (?, ?, ?)
		ON CONFLICT (conversation) DO UPDATE SET hash = excluded.hash, created_at = excluded.created_at`,
		conversation, hash, s.now().UnixMilli())
	return err
}

// Asked returns the hash of the call the conversation's waiting prompt is
// about, and whether there is one.
func (s *Store) Asked(ctx context.Context, conversation string) (hash string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT hash FROM asked_approvals WHERE conversation = ?`, conversation).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return hash, err == nil, err
}

// ForgetAsked drops the conversation's record: its prompt was answered,
// timed out, or the turn that asked is over.
func (s *Store) ForgetAsked(ctx context.Context, conversation string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM asked_approvals WHERE conversation = ?`, conversation)
	return err
}

// Prune drops session state not touched since the cutoff, following
// transcript retention. A conversation's chosen agent is kept.
func (s *Store) Prune(ctx context.Context, olderThan time.Time) (int64, error) {
	cut := olderThan.UnixMilli()
	var total int64
	for _, q := range []string{
		`DELETE FROM settings WHERE updated_at < ?`,
		`DELETE FROM goals WHERE updated_at < ?`,
		`DELETE FROM steers WHERE created_at < ?`,
		`DELETE FROM todos WHERE session_id IN (SELECT session_id FROM todos GROUP BY session_id HAVING MAX(updated_at) < ?)`,
		`DELETE FROM drafts WHERE created_at < ?`,
		`DELETE FROM asked_approvals WHERE created_at < ?`,
		`DELETE FROM chat_notes WHERE created_at < ?`,
	} {
		res, err := s.db.ExecContext(ctx, q, cut)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}
