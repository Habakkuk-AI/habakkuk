// Package session stores conversations. Transcripts are append-only: a
// message, once written, is never edited, which keeps the provider prompt
// cache valid and satisfies reasoning-block binding rules.
package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

var Migrations = []store.Migration{{Version: 1, SQL: `
CREATE TABLE sessions (
	id            TEXT PRIMARY KEY,
	agent         TEXT NOT NULL,
	title         TEXT NOT NULL DEFAULT '',
	system_prompt TEXT NOT NULL,
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL
);
CREATE INDEX sessions_agent ON sessions (agent, updated_at);
CREATE TABLE messages (
	session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
	seq        INTEGER NOT NULL,
	role       TEXT NOT NULL,
	blocks     TEXT NOT NULL,
	model      TEXT NOT NULL DEFAULT '',
	usage      TEXT NOT NULL DEFAULT '',
	job_id     TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	PRIMARY KEY (session_id, seq)
) WITHOUT ROWID;
`}, {Version: 2, SQL: `
CREATE TABLE compactions (
	session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
	keep_from  INTEGER NOT NULL,
	through    INTEGER NOT NULL,
	summary    TEXT NOT NULL,
	model      TEXT NOT NULL DEFAULT '',
	usage      TEXT NOT NULL DEFAULT '',
	job_id     TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	PRIMARY KEY (session_id, keep_from)
) WITHOUT ROWID;
`}, {Version: 3, SQL: searchSchema}, {Version: 4, SQL: metaSchema}}

type Session struct {
	ID           string
	Agent        string
	Title        string
	SystemPrompt string // frozen at creation so the cache prefix never shifts
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// TitleSource says where Title came from: "" (the first message),
	// TitleAuto (a model wrote it) or TitleUser (set with /title).
	TitleSource string
	Archived    bool   // hidden from lists; kept until deleted or pruned
	Parent      string // the session this one was forked or rewound from
}

type Record struct {
	Seq     int
	Message llm.Message
	Model   string
	Usage   *llm.Usage
	JobID   string // the turn job that wrote it; makes turn start idempotent
}

type Store struct {
	db  *sql.DB
	now func() time.Time
}

func NewStore(db *sql.DB) *Store { return &Store{db: db, now: time.Now} }

func (s *Store) Create(ctx context.Context, agent, systemPrompt string) (*Session, error) {
	now := s.now()
	sess := &Session{ID: jobs.NewID("ses"), Agent: agent, SystemPrompt: systemPrompt, CreatedAt: now, UpdatedAt: now}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id, agent, system_prompt, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`, sess.ID, agent, systemPrompt, now.UnixMilli(), now.UnixMilli())
	return sess, err
}

func (s *Store) Get(ctx context.Context, id string) (*Session, error) {
	var sess Session
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id, agent, title, system_prompt, created_at, updated_at, title_source, archived, parent
		FROM sessions WHERE id = ?`, id).Scan(&sess.ID, &sess.Agent, &sess.Title, &sess.SystemPrompt, &created, &updated,
		&sess.TitleSource, &sess.Archived, &sess.Parent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("session %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	sess.CreatedAt, sess.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return &sess, nil
}

// List returns an agent's (or, with agent "", every) session that is not
// archived, most recently updated first.
func (s *Store) List(ctx context.Context, agent string, limit int) ([]Session, error) {
	return s.list(ctx, agent, limit, false)
}

// ListArchived is List for archived sessions.
func (s *Store) ListArchived(ctx context.Context, agent string, limit int) ([]Session, error) {
	return s.list(ctx, agent, limit, true)
}

func (s *Store) list(ctx context.Context, agent string, limit int, archived bool) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, agent, title, created_at, updated_at, title_source, archived, parent FROM sessions
		WHERE (? = '' OR agent = ?) AND archived = ? ORDER BY updated_at DESC LIMIT ?`, agent, agent, archived, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var sess Session
		var created, updated int64
		if err := rows.Scan(&sess.ID, &sess.Agent, &sess.Title, &created, &updated, &sess.TitleSource, &sess.Archived, &sess.Parent); err != nil {
			return nil, err
		}
		sess.CreatedAt, sess.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *Store) SetTitle(ctx context.Context, id, title string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET title = ? WHERE id = ? AND title = ''`, title, id)
	return err
}

// Append adds a message at the next sequence number. expectSeq guards against
// two writers racing on one session: if another message already took that
// slot the append fails instead of interleaving.
func (s *Store) Append(ctx context.Context, sessionID, jobID string, expectSeq int, m llm.Message, model string, usage *llm.Usage) error {
	blocks, err := json.Marshal(m.Blocks)
	if err != nil {
		return err
	}
	var u []byte
	if usage != nil {
		u, _ = json.Marshal(usage)
	}
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages (session_id, seq, role, blocks, model, usage, job_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, sessionID, expectSeq, m.Role, string(blocks), model, string(u), jobID, now); err != nil {
		return fmt.Errorf("append seq %d to %s: %w", expectSeq, sessionID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, now, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Messages(ctx context.Context, sessionID string) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, role, blocks, model, usage, job_id FROM messages
		WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var role, blocks, usage string
		if err := rows.Scan(&r.Seq, &role, &blocks, &r.Model, &usage, &r.JobID); err != nil {
			return nil, err
		}
		r.Message.Role = llm.Role(role)
		if err := json.Unmarshal([]byte(blocks), &r.Message.Blocks); err != nil {
			return nil, fmt.Errorf("message %d: %w", r.Seq, err)
		}
		if usage != "" {
			r.Usage = &llm.Usage{}
			_ = json.Unmarshal([]byte(usage), r.Usage)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Compaction stands in for the start of a long transcript. The model sees
// Summary followed by the messages from KeepFrom on; the messages before
// KeepFrom stay in the transcript but are no longer sent.
type Compaction struct {
	KeepFrom  int    // seq of the first message kept verbatim
	Through   int    // transcript length when the summary was written
	Summary   string // covers every message before KeepFrom, earlier summaries included
	Model     string
	Usage     *llm.Usage // of the summarizing call
	JobID     string
	CreatedAt time.Time
}

// AddCompaction appends a compaction record. Like Append it never rewrites
// anything: it fails if the transcript is no longer Through messages long or
// if KeepFrom does not move past the latest compaction, so a stale writer
// cannot record a summary of a transcript that has moved on.
func (s *Store) AddCompaction(ctx context.Context, sessionID string, c Compaction) error {
	var u []byte
	if c.Usage != nil {
		u, _ = json.Marshal(c.Usage)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n, last int
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM messages WHERE session_id = ?),
		(SELECT COALESCE(MAX(keep_from), 0) FROM compactions WHERE session_id = ?)`,
		sessionID, sessionID).Scan(&n, &last); err != nil {
		return err
	}
	if n != c.Through || c.KeepFrom <= last || c.KeepFrom >= n {
		return fmt.Errorf("compact %s: transcript has %d messages, latest compaction keeps from %d; cannot keep from %d of %d",
			sessionID, n, last, c.KeepFrom, c.Through)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO compactions (session_id, keep_from, through, summary, model, usage, job_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, sessionID, c.KeepFrom, c.Through, c.Summary, c.Model, string(u), c.JobID, s.now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// LatestCompaction returns the newest compaction, or nil if there is none.
func (s *Store) LatestCompaction(ctx context.Context, sessionID string) (*Compaction, error) {
	var c Compaction
	var usage string
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT keep_from, through, summary, model, usage, job_id, created_at
		FROM compactions WHERE session_id = ? ORDER BY keep_from DESC LIMIT 1`, sessionID).
		Scan(&c.KeepFrom, &c.Through, &c.Summary, &c.Model, &usage, &c.JobID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if usage != "" {
		c.Usage = &llm.Usage{}
		_ = json.Unmarshal([]byte(usage), c.Usage)
	}
	c.CreatedAt = time.UnixMilli(created)
	return &c, nil
}

// Prune deletes sessions not updated since the cutoff.
func (s *Store) Prune(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE updated_at < ?`, olderThan.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Repair returns the messages that must be appended before the transcript is
// valid again after a crash: every tool call without a recorded result gets an
// error result. Tools are not re-run blindly because they may have had side
// effects before the crash.
func Repair(msgs []llm.Message) []llm.Message {
	if len(msgs) == 0 {
		return nil
	}
	last := msgs[len(msgs)-1]
	if last.Role != llm.Assistant {
		return nil
	}
	uses := last.ToolUses()
	if len(uses) == 0 {
		return nil
	}
	fix := llm.Message{Role: llm.User}
	for _, u := range uses {
		fix.Blocks = append(fix.Blocks, llm.Block{
			Type: llm.ToolResult, ToolUseID: u.ToolUseID, IsError: true,
			Text: "interrupted: the agent restarted before this tool's result was recorded. It may or may not have run; check the current state before retrying.",
		})
	}
	return []llm.Message{fix}
}
