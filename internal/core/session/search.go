package session

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
)

// searchSchema (transcripts.db version 3) indexes the text of user and
// assistant messages for full-text search. Triggers keep it in step with
// the transcript: an appended message adds its text blocks (tool calls and
// results are left out, and each message is cut to 8000 characters, so the
// index stays a fraction of the transcript), and a pruned session's rows go
// with it through the foreign key. Existing messages are indexed once here.
const searchSchema = `
CREATE TABLE message_text (
	id         INTEGER PRIMARY KEY,
	session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
	seq        INTEGER NOT NULL,
	role       TEXT NOT NULL,
	text       TEXT NOT NULL
);
CREATE INDEX message_text_session ON message_text (session_id, seq);
CREATE VIRTUAL TABLE message_fts USING fts5(text, content='message_text', content_rowid='id', tokenize='porter unicode61');
CREATE TRIGGER message_text_ai AFTER INSERT ON message_text BEGIN
	INSERT INTO message_fts (rowid, text) VALUES (new.id, new.text);
END;
CREATE TRIGGER message_text_ad AFTER DELETE ON message_text BEGIN
	INSERT INTO message_fts (message_fts, rowid, text) VALUES ('delete', old.id, old.text);
END;
CREATE TRIGGER messages_index AFTER INSERT ON messages WHEN new.role IN ('user', 'assistant') BEGIN
	INSERT INTO message_text (session_id, seq, role, text)
	SELECT new.session_id, new.seq, new.role, substr(group_concat(json_extract(b.value, '$.text'), char(10)), 1, 8000)
	FROM json_each(new.blocks) b
	WHERE json_extract(b.value, '$.type') = 'text' AND trim(COALESCE(json_extract(b.value, '$.text'), '')) != ''
	HAVING count(*) > 0;
END;
INSERT INTO message_text (session_id, seq, role, text)
	SELECT m.session_id, m.seq, m.role, substr(group_concat(json_extract(b.value, '$.text'), char(10)), 1, 8000)
	FROM messages m, json_each(m.blocks) b
	WHERE m.role IN ('user', 'assistant') AND json_extract(b.value, '$.type') = 'text'
		AND trim(COALESCE(json_extract(b.value, '$.text'), '')) != ''
	GROUP BY m.session_id, m.seq;
`

// Hit is one message matching a search.
type Hit struct {
	Session string
	Agent   string
	Title   string
	Seq     int
	Role    string
	At      time.Time // when the session was last updated
	Snippet string    // matched words in [brackets]
	// Imported marks a hit in a conversation imported from another agent:
	// history this agent did not take part in, from senders it cannot vet.
	Imported bool
}

var reTerm = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// matchQuery quotes every word (so a query can never be an FTS syntax
// error) and ORs them, letting bm25 rank messages matching more and rarer
// words first.
func matchQuery(q string) string {
	terms := reTerm.FindAllString(q, 32)
	for i, t := range terms {
		terms[i] = `"` + t + `"`
	}
	return strings.Join(terms, " OR ")
}

// Search ranks an agent's messages (every agent's, when agent is "") against
// query. except, when set, leaves one session out (the one searching).
func (s *Store) Search(ctx context.Context, agent, query, except string, limit int) ([]Hit, error) {
	mq := matchQuery(query)
	if mq == "" {
		return nil, errors.New("query is empty: give words to search for")
	}
	if limit <= 0 {
		limit = 10
	}
	limit = min(limit, 50)
	rows, err := s.db.QueryContext(ctx, `SELECT t.session_id, s.agent, s.title, t.seq, t.role, s.updated_at,
			snippet(message_fts, 0, '[', ']', '…', 24),
			EXISTS (SELECT 1 FROM imported_sessions i WHERE i.session_id = t.session_id)
		FROM message_fts JOIN message_text t ON t.id = message_fts.rowid JOIN sessions s ON s.id = t.session_id
		WHERE message_fts MATCH ? AND (? = '' OR s.agent = ?) AND t.session_id != ?
		ORDER BY bm25(message_fts), s.updated_at DESC LIMIT ?`, mq, agent, agent, except, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		var at int64
		if err := rows.Scan(&h.Session, &h.Agent, &h.Title, &h.Seq, &h.Role, &at, &h.Snippet, &h.Imported); err != nil {
			return nil, err
		}
		h.At = time.UnixMilli(at)
		h.Snippet = strings.Join(strings.Fields(h.Snippet), " ")
		out = append(out, h)
	}
	return out, rows.Err()
}

// Excerpt is one indexed message near a search hit.
type Excerpt struct {
	Seq  int
	Role string
	Text string
}

// Around returns the indexed messages within n of seq in one of the agent's
// sessions, oldest first.
func (s *Store) Around(ctx context.Context, agent, sessionID string, seq, n int) ([]Excerpt, error) {
	sess, err := s.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if sess.Agent != agent {
		return nil, errors.New("that session belongs to another agent")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq, role, text FROM message_text
		WHERE session_id = ? AND seq BETWEEN ? AND ? ORDER BY seq`, sessionID, seq-n, seq+n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Excerpt
	for rows.Next() {
		var e Excerpt
		if err := rows.Scan(&e.Seq, &e.Role, &e.Text); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
