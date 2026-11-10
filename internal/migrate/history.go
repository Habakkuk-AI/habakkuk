package migrate

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// Conversation history. Each source keeps its past conversations in its
// own format:
//
//   - Hermes Agent: the sessions and messages tables of state.db, in the
//     main home and in each profile. A session records the platform it ran
//     on (source), its chat_type and, for a chat, its chat_id and
//     thread_id. Structured message content is stored as "\x00json:"
//     followed by JSON. What the agent shows as the conversation is the
//     rows that are active or were compacted (active = 1 or compacted = 1:
//     a rewound or undelivered turn has neither), whose display_kind is
//     empty or "steer" ("hidden" marks a compaction summary, others mark
//     notices such as a model switch), that are not observed group chatter
//     (observed = 1) and not for the model only ("model_only" or
//     "_compressed_summary" in display_metadata). Compaction keeps the
//     originals (active = 0, compacted = 1) and inserts active copies of the
//     turns it keeps, with the same display_order and message_uid: one of
//     each is read. Older summaries are user rows that start with
//     "[CONTEXT COMPACTION" or "[CONTEXT SUMMARY]".
//   - OpenClaw: per agent, agents/<id>/sessions/<session>.jsonl (one JSON
//     event per line, messages as {"type":"message","message":{...}}) with
//     sessions.json mapping session keys such as
//     "agent:<id>:telegram:direct:<peer>" to them; newer homes keep the same
//     events in the transcript_events table of
//     agents/<id>/agent/openclaw-agent.sqlite, keyed through session_windows.
//     A user message there can carry context blocks the gateway added
//     (sender metadata, forwarded and replied-to messages, the chat history
//     since the last reply); they are not the user's words.
//
// Only the text of user and assistant messages comes over: tool calls,
// tool results, reasoning and summaries stay behind. Each conversation
// becomes a read-only session of the agent it belonged to, searchable with
// session_search. The plan only counts messages; they are read once, when
// the import writes them.

// History options.
const (
	HistoryCopy = "copy" // import past conversations (the default)
	HistorySkip = "skip" // leave them in the source home
)

// Conversation is one past conversation to import.
type Conversation struct {
	Agent   string // the Habakkuk agent
	ID      string // its key in the source, unique within the source home
	Title   string
	Started time.Time // when the source says it started; zero when unknown
	// Platform is where it took place in the source (telegram, cli, ...).
	Platform string
	// Account is the Habakkuk channel it maps to, with the chat and thread
	// in that channel; all "" when the source recorded none or the channel
	// is not imported.
	Account, Chat, Thread string
	// Untrusted marks a conversation whose text came from senders nobody
	// vets: a group chat, an inbox, a webhook.
	Untrusted bool
	// Count is how many user and assistant messages with text it holds, as
	// counted for the plan without reading them.
	Count int

	// load reads the messages; untrusted is set when they carried context
	// from other senders that was taken out.
	load func() (msgs []session.ImportedMessage, untrusted bool, err error)
}

// Load reads the conversation's messages. untrusted reports text from
// other senders found (and taken out) on the way.
func (c *Conversation) Load() ([]session.ImportedMessage, bool, error) {
	if c.load == nil {
		return nil, false, nil
	}
	return c.load()
}

// readHistory lists each agent's past conversations.
func (f *Found) readHistory() {
	switch f.From {
	case Hermes:
		for _, a := range f.Agents {
			dir := f.Dir
			if !a.Main {
				dir = filepath.Join(f.Dir, "profiles", a.From)
			}
			f.hermesHistory(a, filepath.Join(dir, "state.db"))
		}
	case OpenClaw:
		for _, a := range f.Agents {
			f.openClawHistory(a, filepath.Join(f.Dir, "agents", a.From))
		}
	}
}

// channelFor finds the imported channel a source platform's chat maps to:
// one of that type answered by the agent, else any of that type.
func (f *Found) channelFor(platform, agentName string) string {
	platform = strings.ToLower(platform)
	other := ""
	for _, c := range f.Channels {
		if c.Type != platform {
			continue
		}
		if c.Agent == agentName {
			return c.Name
		}
		if other == "" {
			other = c.Name
		}
	}
	return other
}

func (f *Found) conversation(a *Agent, c *Conversation) {
	c.Agent = a.Name
	if c.Chat != "" && slices.Contains(chatTypes, strings.ToLower(c.Platform)) {
		c.Account = f.channelFor(c.Platform, a.Name)
	}
	if c.Account == "" {
		c.Chat, c.Thread = "", ""
	}
	f.History = append(f.History, c)
}

// roDB is one connection to a source database for the whole import,
// opened so that nothing is written beside it (openSourceDB); Found.Close
// closes it.
func (f *Found) roDB(path string) (*sql.DB, error) {
	if db := f.dbs[path]; db != nil {
		return db.db, nil
	}
	db, closeFn, err := openSourceDB(path)
	if err != nil {
		return nil, err
	}
	if f.dbs == nil {
		f.dbs = map[string]*sourceDB{}
	}
	f.dbs[path] = &sourceDB{db, closeFn}
	return db, nil
}

type sourceDB struct {
	db    *sql.DB
	close func() error
}

// columns lists a table's columns; nil when the table is missing.
func columns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out map[string]bool
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[n] = true
	}
	return out, rows.Err()
}

// col is a column, or a fallback when the table lacks it.
func col(have map[string]bool, name, fallback string) string {
	if have[name] {
		return "COALESCE(" + name + ", " + fallback + ")"
	}
	return fallback
}

// unixTime reads seconds or milliseconds since 1970.
func unixTime(v float64) time.Time {
	switch {
	case v <= 0:
		return time.Time{}
	case v > 1e12:
		return time.UnixMilli(int64(v))
	}
	return time.UnixMilli(int64(v * 1000))
}

// Platforms whose senders nobody vets, and chat types with more than one
// person in them.
var (
	unvettedSources = []string{"email", "gmail", "webhook", "msgraph_webhook", "api_server", "hook"}
	directChats     = []string{"", "dm", "direct", "private"}
)

// hermesHistory lists the sessions in one home's state.db, with how many
// messages each holds.
func (f *Found) hermesHistory(a *Agent, path string) {
	if !exists(path) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := f.roDB(path)
	if err != nil {
		f.skip("the conversations in %s: %v", path, err)
		return
	}
	sc, err := columns(ctx, db, "sessions")
	if err == nil && (sc == nil || !sc["id"]) {
		return
	}
	mc, err2 := columns(ctx, db, "messages")
	if err = errors.Join(err, err2); err != nil || mc == nil || !mc["role"] || !mc["content"] || !mc["session_id"] {
		if err != nil {
			f.skip("the conversations in %s: %v", path, err)
		}
		return
	}
	// The rows that were part of the conversation as its people saw it.
	shown := `role IN ('user', 'assistant') AND content IS NOT NULL AND trim(content) != ''`
	switch {
	case mc["active"] && mc["compacted"]:
		shown += ` AND (COALESCE(active, 1) = 1 OR COALESCE(compacted, 0) = 1)`
	case mc["active"]:
		shown += ` AND COALESCE(active, 1) != 0`
	}
	if mc["observed"] {
		shown += ` AND COALESCE(observed, 0) = 0`
	}
	if mc["_compressed_summary"] {
		shown += ` AND COALESCE(_compressed_summary, 0) = 0`
	}
	if mc["display_kind"] {
		shown += ` AND COALESCE(display_kind, '') IN ('', 'steer')`
	}
	if mc["display_metadata"] {
		for _, k := range []string{"model_only", "_compressed_summary"} {
			shown += ` AND COALESCE(CASE WHEN json_valid(display_metadata) THEN json_extract(display_metadata, '$.` + k + `') END, 0) NOT IN (1, 'true')`
		}
	}
	// Copies a compaction made share their original's display_order and
	// message_uid.
	key := "CAST(id AS TEXT)"
	switch {
	case mc["display_order"]:
		key = "COALESCE('o' || display_order, 'u' || message_uid, 'i' || id)"
	case mc["message_uid"]:
		key = "COALESCE('u' || message_uid, 'i' || id)"
	case !mc["id"]:
		key = "CAST(rowid AS TEXT)"
	}
	uid := "''"
	if mc["message_uid"] {
		uid = "COALESCE(message_uid, '')"
	}
	counts := map[string]int{}
	rows, err := db.QueryContext(ctx, `SELECT session_id, count(DISTINCT `+key+`) FROM messages WHERE `+shown+` GROUP BY session_id`)
	if err != nil {
		f.skip("the conversations in %s: %v", path, err)
		return
	}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			f.skip("the conversations in %s: %v", path, err)
			return
		}
		counts[id] = n
	}
	rows.Close()
	q := `SELECT id, ` + col(sc, "source", "''") + `, ` + col(sc, "chat_id", "''") + `, ` + col(sc, "thread_id", "''") + `, ` +
		col(sc, "chat_type", "''") + `, ` + col(sc, "title", "''") + `, ` + col(sc, "started_at", "0") +
		` FROM sessions ORDER BY ` + col(sc, "started_at", "0") + `, id`
	rows, err = db.QueryContext(ctx, q)
	if err != nil {
		f.skip("the conversations in %s: %v", path, err)
		return
	}
	defer rows.Close()
	order := "rowid"
	if mc["id"] {
		order = "id"
	}
	ts := col(mc, "timestamp", "0")
	for rows.Next() {
		var id, src, chat, thread, chatType, title string
		var started float64
		if err := rows.Scan(&id, &src, &chat, &thread, &chatType, &title, &started); err != nil {
			f.skip("the conversations in %s: %v", path, err)
			return
		}
		sid := id
		f.conversation(a, &Conversation{
			ID: a.From + "/" + id, Title: title, Started: unixTime(started), Platform: src, Chat: chat, Thread: thread,
			Untrusted: slices.Contains(unvettedSources, strings.ToLower(src)) || !slices.Contains(directChats, strings.ToLower(chatType)),
			Count:     counts[id],
			load: func() ([]session.ImportedMessage, bool, error) {
				rows, err := db.Query(`SELECT role, content, `+ts+`, `+key+`, `+uid+` FROM messages WHERE session_id = ? AND `+shown+` ORDER BY `+order, sid)
				if err != nil {
					return nil, false, err
				}
				defer rows.Close()
				var out []session.ImportedMessage
				seen := map[string]bool{}
				for rows.Next() {
					var role, content, k, u string
					var at float64
					if err := rows.Scan(&role, &content, &at, &k, &u); err != nil {
						return nil, false, err
					}
					if seen[k] || u != "" && seen["u"+u] {
						continue // a copy of a row already read
					}
					seen[k], seen["u"+u] = true, u != ""
					if text := hermesText(content); text != "" && !summaryText(text) {
						out = append(out, session.ImportedMessage{Role: llm.Role(role), Text: text, At: unixTime(at)})
					}
				}
				return out, false, rows.Err()
			},
		})
	}
	if err := rows.Err(); err != nil {
		f.skip("the conversations in %s: %v", path, err)
	}
}

// summaryText reports a compaction summary written as a user message.
func summaryText(text string) bool {
	return strings.HasPrefix(text, "[CONTEXT COMPACTION") || strings.HasPrefix(text, "[CONTEXT SUMMARY]")
}

// hermesText is the text of a stored message: plain, or the text parts of
// structured content.
func hermesText(content string) string {
	if rest, ok := strings.CutPrefix(content, "\x00json:"); ok {
		var v any
		if json.Unmarshal([]byte(rest), &v) != nil {
			return strings.TrimSpace(content)
		}
		return partsText(v)
	}
	return strings.TrimSpace(content)
}

// partsText reads message content: a string, or a list of parts whose
// text parts are joined.
func partsText(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case []any:
		var parts []string
		for _, p := range t {
			switch pt := p.(type) {
			case string:
				parts = append(parts, pt)
			case map[string]any:
				switch typ, _ := pt["type"].(string); typ {
				case "text", "input_text", "output_text", "":
					if s, ok := pt["text"].(string); ok {
						parts = append(parts, s)
					}
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	case map[string]any:
		if s, ok := t["text"].(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// ocEntry is what sessions.json (or session_windows) says about a session.
type ocEntry struct {
	key      string
	title    string
	channel  string
	to       string
	thread   string
	chatType string
	updated  float64
	created  float64
	fromFile string // the transcript file, when there is one
}

// openClawHistory lists one agent's sessions: the transcript files in
// sessions/, and the transcripts in the agent's database.
func (f *Found) openClawHistory(a *Agent, dir string) {
	entries := map[string]*ocEntry{} // session id -> entry
	claimed := map[string]bool{}     // transcript files an entry already has
	var order []string
	add := func(id string, e *ocEntry) {
		if old, ok := entries[id]; ok {
			if old.fromFile == "" && e.fromFile != "" {
				old.fromFile = e.fromFile
				claimed[e.fromFile] = true
			}
			return
		}
		entries[id] = e
		order = append(order, id)
		if e.fromFile != "" {
			claimed[e.fromFile] = true
		}
	}
	sdir := filepath.Join(dir, "sessions")
	if data, err := os.ReadFile(filepath.Join(sdir, "sessions.json")); err == nil {
		var index map[string]map[string]any
		if err := json.Unmarshal(data, &index); err != nil {
			f.skip("%s: %v", filepath.Join(sdir, "sessions.json"), err)
		}
		for _, key := range sortedKeys(index) {
			m := index[key]
			id, _ := m["sessionId"].(string)
			if id == "" {
				continue
			}
			e := ocEntryFrom(key, m)
			if file, _ := m["sessionFile"].(string); file != "" {
				if !filepath.IsAbs(file) {
					file = filepath.Join(sdir, file)
				}
				if under(dir, file) && exists(file) {
					e.fromFile = filepath.Clean(file)
				}
			}
			if e.fromFile == "" && exists(filepath.Join(sdir, id+".jsonl")) {
				e.fromFile = filepath.Join(sdir, id+".jsonl")
			}
			add(id, e)
		}
	}
	files, _ := os.ReadDir(sdir)
	for _, fi := range files {
		name := fi.Name()
		if fi.IsDir() || !strings.Contains(name, ".jsonl") || strings.Contains(name, ".deleted") {
			continue
		}
		id, rest, _ := strings.Cut(name, ".jsonl")
		if rest != "" && !strings.HasPrefix(rest, ".reset") {
			continue
		}
		path := filepath.Join(sdir, name)
		if claimed[path] {
			continue
		}
		e := &ocEntry{fromFile: path}
		if rest != "" {
			id += rest // a transcript set aside by a reset is a conversation of its own
			if t, ok := resetTime(rest); ok {
				e.created = float64(t.UnixMilli())
			}
		}
		add(id, e)
	}
	dbPath := filepath.Join(dir, "agent", "openclaw-agent.sqlite")
	dbIDs := map[string]int{} // session id -> messages counted in the database
	var db *sql.DB
	if exists(dbPath) {
		var err error
		if db, err = f.roDB(dbPath); err == nil {
			err = ocDBSessions(db, func(id string, n int, e *ocEntry) {
				dbIDs[id] = n
				if old, ok := entries[id]; ok {
					if old.title == "" {
						old.title = e.title
					}
					if old.key == "" {
						old.key, old.channel = e.key, e.channel
					}
					if old.created == 0 {
						old.created = e.created
					}
					return
				}
				add(id, e)
			})
		}
		if err != nil {
			f.skip("the conversations in %s: %v", dbPath, err)
		}
	}
	for _, id := range order {
		e := entries[id]
		sid := id
		n, inDB := dbIDs[id]
		if e.fromFile == "" && !inDB {
			continue
		}
		platform, chat, thread, kind := e.channel, e.to, e.thread, e.chatType
		p, c, t, k := parseSessionKey(e.key)
		if platform == "" {
			platform = p
		}
		if chat == "" && (platform == p || p == "") {
			chat = c
		}
		if thread == "" {
			thread = t
		}
		if kind == "" {
			kind = k
		}
		if pfx, rest, ok := strings.Cut(chat, ":"); ok && slices.Contains(chatTypes, pfx) {
			if platform == "" {
				platform = pfx
			}
			chat = rest
		}
		start := e.created
		if start == 0 {
			start = e.updated
		}
		file := e.fromFile
		if file != "" {
			n = countJSONLMessages(file)
		}
		f.conversation(a, &Conversation{
			ID: a.From + "/" + sid, Title: e.title, Started: unixTime(start), Platform: platform, Chat: chat, Thread: thread,
			Untrusted: unvettedKey(e.key) || !slices.Contains(directChats, strings.ToLower(kind)), Count: n,
			load: func() ([]session.ImportedMessage, bool, error) {
				if file != "" {
					return readJSONLTranscript(file)
				}
				return ocDBTranscript(db, sid)
			},
		})
	}
}

// resetTime reads the time in a ".reset.<iso>" suffix, where the time's
// colons are written as dashes: ".reset.2026-10-19T07-00-00.000Z".
func resetTime(suffix string) (time.Time, bool) {
	s, ok := strings.CutPrefix(suffix, ".reset.")
	if !ok {
		return time.Time{}, false
	}
	date, clock, ok := strings.Cut(s, "T")
	if !ok {
		return time.Time{}, false
	}
	clock = strings.Replace(strings.Replace(clock, "-", ":", 1), "-", ":", 1)
	t, err := time.Parse(time.RFC3339Nano, date+"T"+clock)
	return t, err == nil
}

// ocEntryFrom reads a sessions.json entry. The agent's shared main session
// ("agent:<id>:main") takes messages from every direct chat, so its last
// channel and recipient say nothing about where it took place.
func ocEntryFrom(key string, m map[string]any) *ocEntry {
	e := &ocEntry{key: key}
	for _, k := range []string{"displayName", "label", "subject", "title"} {
		if s, _ := m[k].(string); s != "" {
			e.title = s
			break
		}
	}
	e.chatType, _ = m["chatType"].(string)
	if !mainSessionKey(key) {
		e.channel, _ = m["lastChannel"].(string)
		if e.channel == "" {
			e.channel, _ = m["channel"].(string)
		}
		e.to, _ = m["lastTo"].(string)
		if t, ok := m["lastThreadId"]; ok && t != nil {
			e.thread = fmt.Sprint(t)
		}
	}
	if v, ok := toFloat(m["updatedAt"]); ok {
		e.updated = v
	}
	if v, ok := toFloat(m["createdAt"]); ok {
		e.created = v
	}
	return e
}

// mainSessionKey reports an agent's shared main session: "agent:<id>:main"
// or "main".
func mainSessionKey(key string) bool {
	parts := strings.Split(key, ":")
	if len(parts) >= 2 && parts[0] == "agent" {
		parts = parts[2:]
	}
	return len(parts) == 1 && parts[0] == "main"
}

// unvettedKey reports a session key for mail or webhook input.
func unvettedKey(key string) bool {
	for _, p := range strings.Split(strings.ToLower(key), ":") {
		if slices.Contains(unvettedSources, p) {
			return true
		}
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int64:
		return float64(t), true
	case string:
		if tm, ok := parseTime(t); ok {
			return float64(tm.UnixMilli()), true
		}
	}
	return 0, false
}

// parseSessionKey reads the platform, chat, thread and kind of chat out of
// a session key such as "agent:main:telegram:direct:12025550142" or
// "agent:main:discord:channel:123:thread:456". The agent's own main
// session ("agent:main:main") names none.
func parseSessionKey(key string) (platform, chat, thread, kind string) {
	parts := strings.Split(key, ":")
	if len(parts) >= 2 && parts[0] == "agent" {
		parts = parts[2:]
	}
	if len(parts) < 2 || !slices.Contains(chatTypes, strings.ToLower(parts[0])) {
		return "", "", "", ""
	}
	platform = strings.ToLower(parts[0])
	rest := parts[1:]
	switch k := strings.ToLower(rest[0]); k {
	case "direct", "dm", "group", "channel", "room":
		kind, rest = k, rest[1:]
	}
	for i := 0; i < len(rest); i++ {
		switch strings.ToLower(rest[i]) {
		case "thread", "topic":
			if i+1 < len(rest) {
				thread = rest[i+1]
			}
			return platform, chat, thread, kind
		}
		if chat == "" {
			chat = rest[i]
		} else {
			chat += ":" + rest[i]
		}
	}
	return platform, chat, thread, kind
}

// ocDBSessions lists the sessions in an agent database that hold
// transcript events, with a count of their user and assistant messages.
func ocDBSessions(db *sql.DB, fn func(id string, n int, e *ocEntry)) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	wc, err := columns(ctx, db, "session_windows")
	if err != nil {
		return err
	}
	ec, err := columns(ctx, db, "transcript_events")
	if err != nil {
		return err
	}
	if wc == nil || ec == nil || !wc["session_id"] || !ec["event_json"] {
		return nil
	}
	q := `SELECT session_id, ` + col(wc, "session_key", "''") + `, ` + col(wc, "created_at", "0") + `, ` +
		col(wc, "channel", "''") + `, (SELECT count(*) FROM transcript_events t WHERE t.session_id = session_windows.session_id
			AND typeof(t.event_json) = 'text' AND (t.event_json LIKE '%"role":"user"%' OR t.event_json LIKE '%"role":"assistant"%')
			AND (t.event_json LIKE '%"type":"text"%' OR t.event_json LIKE '%"content":"%'))
		FROM session_windows WHERE session_id IN (SELECT DISTINCT session_id FROM transcript_events)
		ORDER BY ` + col(wc, "created_at", "0") + `, session_id`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, key, channel string
		var created float64
		var n int
		if err := rows.Scan(&id, &key, &created, &channel, &n); err != nil {
			return err
		}
		fn(id, n, &ocEntry{key: key, created: created, channel: channel})
	}
	return rows.Err()
}

// ocDBTranscript reads one session's events from an agent database.
func ocDBTranscript(db *sql.DB, id string) ([]session.ImportedMessage, bool, error) {
	rows, err := db.Query(`SELECT event_json FROM transcript_events WHERE session_id = ? AND typeof(event_json) = 'text' ORDER BY seq`, id)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []session.ImportedMessage
	untrusted := false
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, false, err
		}
		if m, other, ok := transcriptLine([]byte(line)); ok {
			out = append(out, m)
			untrusted = untrusted || other
		} else if other {
			untrusted = true
		}
	}
	return out, untrusted, rows.Err()
}

// countJSONLMessages counts the lines of a transcript file that hold a user
// or assistant message with text, without decoding them: a count for the
// plan.
func countJSONLMessages(path string) int {
	fh, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer fh.Close()
	n := 0
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		l := sc.Bytes()
		role := bytes.Contains(l, []byte(`"role":"user"`)) || bytes.Contains(l, []byte(`"role":"assistant"`)) ||
			bytes.Contains(l, []byte(`"role": "user"`)) || bytes.Contains(l, []byte(`"role": "assistant"`))
		text := bytes.Contains(l, []byte(`"type":"text"`)) || bytes.Contains(l, []byte(`"type": "text"`)) ||
			bytes.Contains(l, []byte(`"content":"`)) || bytes.Contains(l, []byte(`"content": "`))
		if role && text {
			n++
		}
	}
	return n
}

// readJSONLTranscript reads a transcript file of JSON events.
func readJSONLTranscript(path string) ([]session.ImportedMessage, bool, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer fh.Close()
	var out []session.ImportedMessage
	untrusted := false
	r := bufio.NewReaderSize(fh, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if m, other, ok := transcriptLine(line); ok {
			out = append(out, m)
			untrusted = untrusted || other
		} else if other {
			untrusted = true
		}
		if errors.Is(err, io.EOF) {
			return out, untrusted, nil
		}
		if err != nil {
			return out, untrusted, err
		}
	}
}

// transcriptLine reads one event: a message wrapped as {"type":"message",
// "message":{...}} or, in older files, the message itself. other reports
// context from other senders that was taken out of a user message.
func transcriptLine(line []byte) (msg session.ImportedMessage, other, ok bool) {
	var ev struct {
		Type      string          `json:"type"`
		Timestamp any             `json:"timestamp"`
		Message   json.RawMessage `json:"message"`
		Role      string          `json:"role"`
		Content   any             `json:"content"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return msg, false, false
	}
	role, content, ts := ev.Role, ev.Content, ev.Timestamp
	if len(ev.Message) > 0 {
		if ev.Type != "" && ev.Type != "message" {
			return msg, false, false
		}
		var m struct {
			Role      string `json:"role"`
			Content   any    `json:"content"`
			Timestamp any    `json:"timestamp"`
		}
		if json.Unmarshal(ev.Message, &m) != nil {
			return msg, false, false
		}
		role, content = m.Role, m.Content
		if m.Timestamp != nil {
			ts = m.Timestamp
		}
	}
	if role != "user" && role != "assistant" {
		return msg, false, false
	}
	text := partsText(content)
	if role == "user" {
		text, other = stripInbound(text)
	}
	if text == "" {
		return msg, other, false
	}
	var at time.Time
	if v, ok := toFloat(ts); ok {
		at = unixTime(v)
	}
	return session.ImportedMessage{Role: llm.Role(role), Text: text, At: at}, other, true
}

// The context blocks the source's gateway puts around a user's message: a
// label line, then a fenced JSON block (sender, forwarded and replied-to
// messages and the like) or a list (untrusted context, the chat history
// since the last reply). They are other people's words or metadata, so
// they are taken out. Newer gateways add them after the user's text,
// separated by a blank line; older ones put them first. A message that is
// only observed room activity is not the user's at all.
var (
	reFencedLabel = regexp.MustCompile(`^(Conversation info|Sender|Thread starter|Reply chain of current user message|Reply target of current user message|Replied message|Forwarded message context|Forwarded message|Location|Structured object)\b[^\n]*:\s*(⟦[^⟧]*⟧)?\s*$`)
	reBlockLabel  = regexp.MustCompile(`^(Untrusted context|Chat history since last reply|Recent chat history|Conversation context)\b[^\n]*:\s*(⟦[^⟧]*⟧)?\s*$|\(untrusted[^)]*\):\s*$|⟦[^⟧]*ctx⟧`)
	reEntry       = regexp.MustCompile(`^(\[|\{|- |\* )`)
)

// stripInbound takes those context blocks out of a user's message and
// reports whether it found any.
func stripInbound(text string) (string, bool) {
	if strings.HasPrefix(strings.TrimSpace(text), "[OpenClaw room event]") {
		return "", true
	}
	if !strings.Contains(text, "ntrusted") && !strings.Contains(text, "⟦") && !strings.Contains(text, "```") &&
		!strings.Contains(text, "chat history") && !strings.Contains(text, "Conversation context") {
		return text, false
	}
	lines := strings.Split(text, "\n")
	var keep []string
	found := false
	label := func(l string) bool {
		return reBlockLabel.MatchString(l) || reFencedLabel.MatchString(l)
	}
	for i := 0; i < len(lines); i++ {
		l := strings.TrimSpace(lines[i])
		switch {
		case reFencedLabel.MatchString(l) && i+1 < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i+1]), "```"):
			found = true
			i += 2
			for i < len(lines) && strings.TrimSpace(lines[i]) != "```" {
				i++
			}
		case reBlockLabel.MatchString(l):
			found = true
			if strings.TrimSpace(strings.Join(keep, "\n")) != "" {
				// After the user's text: every block from here on is context.
				i = len(lines)
				continue
			}
			// Before it: the block runs on, across blank lines, while its
			// lines look like entries.
			for i+1 < len(lines) {
				next := strings.TrimSpace(lines[i+1])
				if next == "" {
					j := i + 1
					for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
						j++
					}
					if j < len(lines) && (reEntry.MatchString(strings.TrimSpace(lines[j])) && !label(strings.TrimSpace(lines[j]))) {
						i = j
						continue
					}
					break
				}
				if label(next) {
					break
				}
				i++
			}
		default:
			keep = append(keep, lines[i])
		}
	}
	return strings.TrimSpace(strings.Join(keep, "\n")), found
}

// planHistory decides which conversations are new: the ones the Habakkuk
// home does not hold yet. It reads transcripts.db without changing it,
// and counts messages without reading them.
func (p *Plan) planHistory() error {
	f := p.Found
	if p.opts.History == HistorySkip || len(f.History) == 0 {
		return nil
	}
	have := map[string]bool{}
	path := filepath.Join(p.Home, "transcripts.db")
	if exists(path) {
		q := url.Values{}
		q.Add("mode", "ro")
		q.Add("_pragma", "busy_timeout(10000)")
		db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
		if err != nil {
			return err
		}
		defer db.Close()
		rows, err := db.Query(`SELECT source_id FROM imported_sessions WHERE source = ?`, f.From)
		switch {
		case err != nil && strings.Contains(err.Error(), "no such table"):
		case err != nil:
			return fmt.Errorf("read %s: %w", path, err)
		default:
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				have[id] = true
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
	}
	items := map[string]*Item{}
	for _, c := range f.History {
		it := items[c.Agent]
		if it == nil {
			it = p.add(&Item{Kind: "history", Agent: c.Agent, Name: c.Agent, Status: Same})
			items[c.Agent] = it
		}
		if have[c.ID] {
			it.histSame++
			continue
		}
		if c.Count == 0 {
			continue // nothing said in it but tool calls, or nothing at all
		}
		it.hist = append(it.hist, c)
		it.histMsgs += c.Count
		it.Status = New
	}
	for _, it := range p.Items {
		if it.Kind != "history" {
			continue
		}
		it.Detail = fmt.Sprintf("%s (%s) to import, %d already there", plural(len(it.hist), "conversation"), plural(it.histMsgs, "message"), it.histSame)
		if len(it.hist) == 0 && it.histSame == 0 {
			it.Detail = "no conversations with text"
		}
	}
	return nil
}

// applyHistory writes the new conversations as read-only sessions.
func (p *Plan) applyHistory(ctx context.Context, res *Result) error {
	var todo []*Item
	for _, it := range p.Items {
		if it.Kind == "history" && it.Changes() {
			todo = append(todo, it)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	db, err := store.Open(ctx, filepath.Join(p.Home, "transcripts.db"), session.Migrations)
	if err != nil {
		return err
	}
	defer db.Close()
	st := session.NewStore(db)
	for _, it := range todo {
		n := 0
		var errs []error
		for _, c := range it.hist {
			msgs, other, err := c.Load()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", c.ID, err))
				continue
			}
			if len(msgs) == 0 {
				continue
			}
			title := c.Title
			if title == "" {
				for _, m := range msgs {
					if m.Role == llm.User {
						title = titleOf(m.Text)
						break
					}
				}
			}
			_, added, err := st.Import(ctx, session.Import{Source: p.Found.From, SourceID: c.ID, Agent: c.Agent, Title: title,
				Started: c.Started, Account: c.Account, Conversation: c.Chat, Thread: c.Thread, Untrusted: c.Untrusted || other,
				Messages: msgs})
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if added {
				n++
				res.Messages += len(msgs)
			}
		}
		res.Conversations += n
		if len(errs) > 0 {
			res.Failed[it] = errors.Join(errs...)
		}
		if n > 0 {
			res.Done = append(res.Done, it)
		}
	}
	return nil
}
