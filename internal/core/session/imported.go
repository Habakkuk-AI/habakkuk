package session

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// importedSchema (transcripts.db version 5) records the sessions that
// `hk migrate` brought over from another agent: where each came from and,
// when the other agent recorded it, the channel and chat it belonged to.
// An imported session is history. It shows in lists and in session_search,
// but no turn runs in it and retention never prunes it.
const importedSchema = `
CREATE TABLE imported_sessions (
	session_id   TEXT PRIMARY KEY REFERENCES sessions (id) ON DELETE CASCADE,
	source       TEXT NOT NULL,
	source_id    TEXT NOT NULL,
	account      TEXT NOT NULL DEFAULT '',
	conversation TEXT NOT NULL DEFAULT '',
	thread       TEXT NOT NULL DEFAULT '',
	imported_at  INTEGER NOT NULL
);
CREATE UNIQUE INDEX imported_sessions_source ON imported_sessions (source, source_id);
`

// TitleImported is the title source of a session imported from another
// agent. Such a session is read-only: see Store.Import.
const TitleImported = "import"

// ErrImported is returned for a turn in an imported session.
var ErrImported = errors.New("this conversation was imported from another agent and is read-only; start a new session to go on")

// ImportedMessage is one message of an imported conversation.
type ImportedMessage struct {
	Role llm.Role // llm.User or llm.Assistant
	Text string
	At   time.Time // zero: the session's start
}

// Import is one conversation from another agent.
type Import struct {
	Source   string // the agent it comes from, such as an import source's name
	SourceID string // its id there; with Source it keys the import
	Agent    string // the Habakkuk agent it belongs to
	Title    string
	Started  time.Time
	// Account, Conversation and Thread are the chat it took place in, in
	// Habakkuk's terms, when the source recorded one; all "" otherwise.
	Account, Conversation, Thread string
	Messages                      []ImportedMessage
}

// ImportID is the session id an import gets: the same for the same
// conversation every time, so importing again adds nothing.
func ImportID(source, sourceID string) string {
	h := sha256.Sum256([]byte(source + "\x00" + sourceID))
	return "ses_imp" + hex.EncodeToString(h[:8])
}

// Imported reports whether a conversation from that source is already
// here.
func (s *Store) Imported(ctx context.Context, source, sourceID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM imported_sessions WHERE source = ? AND source_id = ?`,
		source, sourceID).Scan(&n)
	return n > 0, err
}

// Import stores a conversation from another agent as a read-only session,
// with its messages' text indexed for session_search. It reports false,
// and changes nothing, when that conversation is already here.
func (s *Store) Import(ctx context.Context, im Import) (string, bool, error) {
	if im.Source == "" || im.SourceID == "" || im.Agent == "" {
		return "", false, errors.New("import: source, source id and agent are needed")
	}
	id := ImportID(im.Source, im.SourceID)
	start := im.Started
	if start.IsZero() {
		start = s.now()
	}
	last := start
	for _, m := range im.Messages {
		if m.At.After(last) {
			last = m.At
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM imported_sessions WHERE source = ? AND source_id = ?`,
		im.Source, im.SourceID).Scan(&n); err != nil {
		return "", false, err
	}
	if n > 0 {
		return id, false, nil
	}
	title := strings.Join(strings.Fields(im.Title), " ")
	if r := []rune(title); len(r) > 80 {
		title = string(r[:79]) + "…"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, agent, title, system_prompt, created_at, updated_at, title_source)
		VALUES (?, ?, ?, '', ?, ?, ?)`, id, im.Agent, title, start.UnixMilli(), last.UnixMilli(), TitleImported); err != nil {
		return "", false, fmt.Errorf("import %s: %w", im.SourceID, err)
	}
	seq := 0
	for _, m := range im.Messages {
		text := strings.TrimSpace(m.Text)
		if text == "" || (m.Role != llm.User && m.Role != llm.Assistant) {
			continue
		}
		blocks, err := json.Marshal([]llm.Block{{Type: llm.Text, Text: text}})
		if err != nil {
			return "", false, err
		}
		at := m.At
		if at.IsZero() {
			at = start
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages (session_id, seq, role, blocks, created_at) VALUES (?, ?, ?, ?, ?)`,
			id, seq, m.Role, string(blocks), at.UnixMilli()); err != nil {
			return "", false, err
		}
		seq++
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO imported_sessions (session_id, source, source_id, account, conversation, thread, imported_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, im.Source, im.SourceID, im.Account, im.Conversation, im.Thread, s.now().UnixMilli()); err != nil {
		return "", false, err
	}
	return id, true, tx.Commit()
}

// Origin is where an imported session came from.
type Origin struct {
	Source, SourceID              string
	Account, Conversation, Thread string
	ImportedAt                    time.Time
}

// ImportOrigin returns where an imported session came from, or nil for a
// session that was not imported.
func (s *Store) ImportOrigin(ctx context.Context, id string) (*Origin, error) {
	var o Origin
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT source, source_id, account, conversation, thread, imported_at
		FROM imported_sessions WHERE session_id = ?`, id).Scan(&o.Source, &o.SourceID, &o.Account, &o.Conversation, &o.Thread, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o.ImportedAt = time.UnixMilli(at)
	return &o, nil
}
