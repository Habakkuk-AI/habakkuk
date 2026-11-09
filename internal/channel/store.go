package channel

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// Migrations for the channel tables in control.db. They continue the jobs
// migrations, so versions start at 2.
var Migrations = []store.Migration{{Version: 2, SQL: `
CREATE TABLE conversations (
	account      TEXT NOT NULL,
	conversation TEXT NOT NULL,
	thread       TEXT NOT NULL DEFAULT '',
	agent        TEXT NOT NULL,
	session_id   TEXT NOT NULL,
	last_at      INTEGER NOT NULL,
	PRIMARY KEY (account, conversation, thread)
) WITHOUT ROWID;
CREATE INDEX conversations_session ON conversations (session_id);

CREATE TABLE channel_status (
	account    TEXT PRIMARY KEY,
	state      TEXT NOT NULL,
	detail     TEXT NOT NULL DEFAULT '',
	last_error TEXT NOT NULL DEFAULT '',
	restarts   INTEGER NOT NULL DEFAULT 0,
	since      INTEGER NOT NULL,
	last_beat  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE channel_allow (
	account  TEXT NOT NULL,
	sender   TEXT NOT NULL,
	name     TEXT NOT NULL DEFAULT '',
	added_at INTEGER NOT NULL,
	PRIMARY KEY (account, sender)
) WITHOUT ROWID;

CREATE TABLE pair_codes (
	code       TEXT PRIMARY KEY,
	account    TEXT NOT NULL,
	sender     TEXT NOT NULL,
	name       TEXT NOT NULL DEFAULT '',
	expires_at INTEGER NOT NULL
);

CREATE TABLE delivery_progress (
	job_id TEXT PRIMARY KEY,
	sent   INTEGER NOT NULL
) WITHOUT ROWID;
`}}

// Store keeps conversation bindings, channel health, the sender allowlist
// and delivery progress.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

func NewStore(db *sql.DB) *Store { return &Store{db: db, now: time.Now} }

type Conversation struct {
	Address
	Agent     string
	SessionID string
	LastAt    time.Time
}

func (s *Store) Conversation(ctx context.Context, a Address) (*Conversation, error) {
	c := &Conversation{Address: a}
	var last int64
	err := s.db.QueryRowContext(ctx, `SELECT agent, session_id, last_at FROM conversations
		WHERE account = ? AND conversation = ? AND thread = ?`, a.Account, a.Conversation, a.Thread).
		Scan(&c.Agent, &c.SessionID, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	c.LastAt = time.UnixMilli(last)
	return c, err
}

// BySession finds the conversation a session belongs to.
func (s *Store) BySession(ctx context.Context, sessionID string) (*Conversation, error) {
	c := &Conversation{SessionID: sessionID}
	var last int64
	err := s.db.QueryRowContext(ctx, `SELECT account, conversation, thread, agent, last_at FROM conversations
		WHERE session_id = ?`, sessionID).Scan(&c.Account, &c.Conversation, &c.Thread, &c.Agent, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	c.LastAt = time.UnixMilli(last)
	return c, err
}

func (s *Store) Bind(ctx context.Context, a Address, agent, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO conversations (account, conversation, thread, agent, session_id, last_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (account, conversation, thread)
		DO UPDATE SET agent = excluded.agent, session_id = excluded.session_id, last_at = excluded.last_at`,
		a.Account, a.Conversation, a.Thread, agent, sessionID, s.now().UnixMilli())
	return err
}

func (s *Store) Touch(ctx context.Context, a Address) error {
	_, err := s.db.ExecContext(ctx, `UPDATE conversations SET last_at = ?
		WHERE account = ? AND conversation = ? AND thread = ?`, s.now().UnixMilli(), a.Account, a.Conversation, a.Thread)
	return err
}

func (s *Store) Allowed(ctx context.Context, account string, senders ...string) (bool, error) {
	for _, sender := range senders {
		if sender == "" {
			continue
		}
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM channel_allow WHERE account = ? AND sender = ?`,
			account, sender).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	return false, nil
}

// PairCode returns a live pairing code for an unknown sender, reusing one
// already issued so repeated messages do not mint new codes.
func (s *Store) PairCode(ctx context.Context, account, sender, name string) (code string, fresh bool, err error) {
	now := s.now()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pair_codes WHERE expires_at < ?`, now.UnixMilli()); err != nil {
		return "", false, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT code FROM pair_codes WHERE account = ? AND sender = ?`, account, sender).Scan(&code)
	if err == nil {
		return code, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	code = newPairCode()
	_, err = s.db.ExecContext(ctx, `INSERT INTO pair_codes (code, account, sender, name, expires_at) VALUES (?, ?, ?, ?, ?)`,
		code, account, sender, name, now.Add(time.Hour).UnixMilli())
	return code, true, err
}

// Pair approves the sender behind a pairing code.
func (s *Store) Pair(ctx context.Context, code string) (account, sender, name string, err error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	err = s.db.QueryRowContext(ctx, `DELETE FROM pair_codes WHERE code = ? AND expires_at >= ?
		RETURNING account, sender, name`, code, s.now().UnixMilli()).Scan(&account, &sender, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", errors.New("no such pairing code, or it expired (codes last one hour)")
	}
	if err != nil {
		return "", "", "", err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO channel_allow (account, sender, name, added_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (account, sender) DO NOTHING`, account, sender, name, s.now().UnixMilli())
	return account, sender, name, err
}

func newPairCode() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O or 1/I
	var b [6]byte
	rand.Read(b[:])
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b[:])
}

// Delivery progress lets a retried delivery skip chunks already sent.

func (s *Store) Sent(ctx context.Context, jobID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT sent FROM delivery_progress WHERE job_id = ?`, jobID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

func (s *Store) MarkSent(ctx context.Context, jobID string, n int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO delivery_progress (job_id, sent) VALUES (?, ?)
		ON CONFLICT (job_id) DO UPDATE SET sent = excluded.sent`, jobID, n)
	return err
}

func (s *Store) ForgetDelivery(ctx context.Context, jobID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM delivery_progress WHERE job_id = ?`, jobID)
	return err
}

// Status is one account's health as last recorded by the supervisor.
type Status struct {
	Account   string
	State     string // connecting | connected | backoff | stopped
	Detail    string
	LastError string
	Restarts  int
	Since     time.Time
	LastBeat  time.Time
}

func (s *Store) SetStatus(ctx context.Context, st Status) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO channel_status (account, state, detail, last_error, restarts, since, last_beat)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (account) DO UPDATE SET state = excluded.state,
		detail = excluded.detail, last_error = excluded.last_error, restarts = excluded.restarts,
		since = excluded.since, last_beat = excluded.last_beat`,
		st.Account, st.State, st.Detail, st.LastError, st.Restarts, st.Since.UnixMilli(), st.LastBeat.UnixMilli())
	return err
}

func (s *Store) Statuses(ctx context.Context) ([]Status, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account, state, detail, last_error, restarts, since, last_beat
		FROM channel_status ORDER BY account`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Status
	for rows.Next() {
		var st Status
		var since, beat int64
		if err := rows.Scan(&st.Account, &st.State, &st.Detail, &st.LastError, &st.Restarts, &since, &beat); err != nil {
			return nil, err
		}
		st.Since, st.LastBeat = time.UnixMilli(since), time.UnixMilli(beat)
		out = append(out, st)
	}
	return out, rows.Err()
}
