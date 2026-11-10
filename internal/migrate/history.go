package migrate

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
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
//     on (source) and, for a chat, its chat_id and thread_id. Structured
//     message content is stored as "\x00json:" followed by JSON.
//   - OpenClaw: per agent, agents/<id>/sessions/<session>.jsonl (one JSON
//     event per line, messages as {"type":"message","message":{...}}) with
//     sessions.json mapping session keys such as
//     "agent:<id>:telegram:direct:<peer>" to them; newer homes keep the same
//     events in the transcript_events table of
//     agents/<id>/agent/openclaw-agent.sqlite, keyed through session_windows.
//
// Only the text of user and assistant messages comes over: tool calls,
// tool results, reasoning and summaries stay behind. Each conversation
// becomes a read-only session of the agent it belonged to, searchable with
// session_search.

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
	Started time.Time
	// Platform is where it took place in the source (telegram, cli, ...).
	Platform string
	// Account is the Habakkuk channel it maps to, with the chat and thread
	// in that channel; all "" when the source recorded none or the channel
	// is not imported.
	Account, Chat, Thread string

	load func() ([]session.ImportedMessage, error)
}

// Load reads the conversation's messages.
func (c *Conversation) Load() ([]session.ImportedMessage, error) {
	if c.load == nil {
		return nil, nil
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

// openRO opens a SQLite file read-only.
func openRO(path string) (*sql.DB, error) {
	q := url.Values{}
	q.Add("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	return sql.Open("sqlite", "file:"+path+"?"+q.Encode())
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

// hermesHistory lists the sessions in one home's state.db.
func (f *Found) hermesHistory(a *Agent, path string) {
	if !exists(path) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := openRO(path)
	if err != nil {
		f.skip("the conversations in %s: %v", path, err)
		return
	}
	defer db.Close()
	sc, err := columns(ctx, db, "sessions")
	if err == nil && (sc == nil || !sc["id"]) {
		return
	}
	mc, err2 := columns(ctx, db, "messages")
	if err = errors.Join(err, err2); err != nil || mc == nil || !mc["role"] || !mc["content"] {
		if err != nil {
			f.skip("the conversations in %s: %v", path, err)
		}
		return
	}
	q := `SELECT id, ` + col(sc, "source", "''") + `, ` + col(sc, "chat_id", "''") + `, ` + col(sc, "thread_id", "''") + `, ` +
		col(sc, "title", "''") + `, ` + col(sc, "started_at", "0") + ` FROM sessions ORDER BY ` + col(sc, "started_at", "0") + `, id`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		f.skip("the conversations in %s: %v", path, err)
		return
	}
	defer rows.Close()
	where := `session_id = ? AND role IN ('user', 'assistant')`
	if mc["_compressed_summary"] {
		where += ` AND COALESCE(_compressed_summary, 0) = 0`
	}
	order := "rowid"
	if mc["id"] {
		order = "id"
	}
	ts := col(mc, "timestamp", "0")
	for rows.Next() {
		var id, src, chat, thread, title string
		var started float64
		if err := rows.Scan(&id, &src, &chat, &thread, &title, &started); err != nil {
			f.skip("the conversations in %s: %v", path, err)
			return
		}
		sid := id
		f.conversation(a, &Conversation{
			ID: a.From + "/" + id, Title: title, Started: unixTime(started), Platform: src, Chat: chat, Thread: thread,
			load: func() ([]session.ImportedMessage, error) {
				db, err := openRO(path)
				if err != nil {
					return nil, err
				}
				defer db.Close()
				rows, err := db.Query(`SELECT role, COALESCE(content, ''), `+ts+` FROM messages WHERE `+where+` ORDER BY `+order, sid)
				if err != nil {
					return nil, err
				}
				defer rows.Close()
				var out []session.ImportedMessage
				for rows.Next() {
					var role, content string
					var at float64
					if err := rows.Scan(&role, &content, &at); err != nil {
						return nil, err
					}
					if text := hermesText(content); text != "" {
						out = append(out, session.ImportedMessage{Role: llm.Role(role), Text: text, At: unixTime(at)})
					}
				}
				return out, rows.Err()
			},
		})
	}
	if err := rows.Err(); err != nil {
		f.skip("the conversations in %s: %v", path, err)
	}
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
	updated  float64
	created  float64
	fromFile string // the transcript file, when there is one
}

// openClawHistory lists one agent's sessions: the transcript files in
// sessions/, and the transcripts in the agent's database.
func (f *Found) openClawHistory(a *Agent, dir string) {
	entries := map[string]*ocEntry{} // session id -> entry
	var order []string
	add := func(id string, e *ocEntry) {
		if old, ok := entries[id]; ok {
			if old.fromFile == "" {
				old.fromFile = e.fromFile
			}
			return
		}
		entries[id] = e
		order = append(order, id)
	}
	sdir := filepath.Join(dir, "sessions")
	if data, err := os.ReadFile(filepath.Join(sdir, "sessions.json")); err == nil {
		var store map[string]map[string]any
		if err := json.Unmarshal(data, &store); err != nil {
			f.skip("%s: %v", filepath.Join(sdir, "sessions.json"), err)
		}
		for _, key := range sortedKeys(store) {
			m := store[key]
			id, _ := m["sessionId"].(string)
			if id == "" {
				continue
			}
			e := ocEntryFrom(key, m)
			if file, _ := m["sessionFile"].(string); file != "" && filepath.IsAbs(file) && under(dir, file) {
				e.fromFile = file
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
		if rest != "" {
			id += rest // a transcript set aside by a reset is a conversation of its own
		}
		if e, ok := entries[id]; ok && e.fromFile == "" {
			e.fromFile = path
			continue
		}
		add(id, &ocEntry{fromFile: path})
	}
	dbPath := filepath.Join(dir, "agent", "openclaw-agent.sqlite")
	dbIDs := map[string]bool{}
	if exists(dbPath) {
		if err := ocDBSessions(dbPath, func(id string, e *ocEntry) {
			dbIDs[id] = true
			if old, ok := entries[id]; ok {
				if old.title == "" {
					old.title = e.title
				}
				if old.key == "" {
					old.key, old.channel, old.to, old.thread = e.key, e.channel, e.to, e.thread
				}
				if old.created == 0 {
					old.created = e.created
				}
				return
			}
			add(id, e)
		}); err != nil {
			f.skip("the conversations in %s: %v", dbPath, err)
		}
	}
	for _, id := range order {
		e := entries[id]
		sid := id
		inDB := dbIDs[id]
		if e.fromFile == "" && !inDB {
			continue
		}
		platform, chat, thread := e.channel, e.to, e.thread
		if p, c, t := parseSessionKey(e.key); platform == "" || chat == "" {
			if platform == "" {
				platform = p
			}
			if chat == "" && (platform == p || p == "") {
				chat = c
			}
			if thread == "" {
				thread = t
			}
		}
		if p, rest, ok := strings.Cut(chat, ":"); ok && slices.Contains(chatTypes, p) {
			if platform == "" {
				platform = p
			}
			chat = rest
		}
		start := e.created
		if start == 0 {
			start = e.updated
		}
		file := e.fromFile
		f.conversation(a, &Conversation{
			ID: a.From + "/" + sid, Title: e.title, Started: unixTime(start), Platform: platform, Chat: chat, Thread: thread,
			load: func() ([]session.ImportedMessage, error) {
				if file != "" {
					return readJSONLTranscript(file)
				}
				return ocDBTranscript(dbPath, sid)
			},
		})
	}
}

// ocEntryFrom reads a sessions.json entry.
func ocEntryFrom(key string, m map[string]any) *ocEntry {
	e := &ocEntry{key: key}
	for _, k := range []string{"displayName", "label", "subject", "title"} {
		if s, _ := m[k].(string); s != "" {
			e.title = s
			break
		}
	}
	e.channel, _ = m["lastChannel"].(string)
	if e.channel == "" {
		e.channel, _ = m["channel"].(string)
	}
	e.to, _ = m["lastTo"].(string)
	if t, ok := m["lastThreadId"]; ok && t != nil {
		e.thread = fmt.Sprint(t)
	}
	if v, ok := toFloat(m["updatedAt"]); ok {
		e.updated = v
	}
	if v, ok := toFloat(m["createdAt"]); ok {
		e.created = v
	}
	return e
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

// parseSessionKey reads the platform, chat and thread out of a session key
// such as "agent:main:telegram:direct:12025550142" or
// "agent:main:discord:channel:123:thread:456". The agent's own main
// session ("agent:main:main") names none.
func parseSessionKey(key string) (platform, chat, thread string) {
	parts := strings.Split(key, ":")
	if len(parts) >= 2 && parts[0] == "agent" {
		parts = parts[2:]
	}
	if len(parts) < 2 || !slices.Contains(chatTypes, strings.ToLower(parts[0])) {
		return "", "", ""
	}
	platform = strings.ToLower(parts[0])
	rest := parts[1:]
	switch strings.ToLower(rest[0]) {
	case "direct", "dm", "group", "channel", "room":
		rest = rest[1:]
	}
	for i := 0; i < len(rest); i++ {
		switch strings.ToLower(rest[i]) {
		case "thread", "topic":
			if i+1 < len(rest) {
				thread = rest[i+1]
			}
			return platform, chat, thread
		}
		if chat == "" {
			chat = rest[i]
		} else {
			chat += ":" + rest[i]
		}
	}
	return platform, chat, thread
}

// ocDBSessions lists the sessions in an agent database that hold
// transcript events.
func ocDBSessions(path string, fn func(id string, e *ocEntry)) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := openRO(path)
	if err != nil {
		return err
	}
	defer db.Close()
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
		col(wc, "channel", "''") + ` FROM session_windows WHERE session_id IN (SELECT DISTINCT session_id FROM transcript_events)
		ORDER BY ` + col(wc, "created_at", "0") + `, session_id`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, key, channel string
		var created float64
		if err := rows.Scan(&id, &key, &created, &channel); err != nil {
			return err
		}
		fn(id, &ocEntry{key: key, created: created, channel: channel})
	}
	return rows.Err()
}

// ocDBTranscript reads one session's events from an agent database.
func ocDBTranscript(path, id string) ([]session.ImportedMessage, error) {
	db, err := openRO(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT event_json FROM transcript_events WHERE session_id = ? AND typeof(event_json) = 'text' ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []session.ImportedMessage
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		if m, ok := transcriptLine([]byte(line)); ok {
			out = append(out, m)
		}
	}
	return out, rows.Err()
}

// readJSONLTranscript reads a transcript file of JSON events.
func readJSONLTranscript(path string) ([]session.ImportedMessage, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	var out []session.ImportedMessage
	r := bufio.NewReaderSize(fh, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if m, ok := transcriptLine(line); ok {
			out = append(out, m)
		}
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

// transcriptLine reads one event: a message wrapped as {"type":"message",
// "message":{...}} or, in older files, the message itself.
func transcriptLine(line []byte) (session.ImportedMessage, bool) {
	var ev struct {
		Type      string          `json:"type"`
		Timestamp any             `json:"timestamp"`
		Message   json.RawMessage `json:"message"`
		Role      string          `json:"role"`
		Content   any             `json:"content"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return session.ImportedMessage{}, false
	}
	role, content, ts := ev.Role, ev.Content, ev.Timestamp
	if len(ev.Message) > 0 {
		if ev.Type != "" && ev.Type != "message" {
			return session.ImportedMessage{}, false
		}
		var m struct {
			Role      string `json:"role"`
			Content   any    `json:"content"`
			Timestamp any    `json:"timestamp"`
		}
		if json.Unmarshal(ev.Message, &m) != nil {
			return session.ImportedMessage{}, false
		}
		role, content = m.Role, m.Content
		if m.Timestamp != nil {
			ts = m.Timestamp
		}
	}
	if role != "user" && role != "assistant" {
		return session.ImportedMessage{}, false
	}
	text := partsText(content)
	if text == "" {
		return session.ImportedMessage{}, false
	}
	var at time.Time
	if v, ok := toFloat(ts); ok {
		at = unixTime(v)
	}
	return session.ImportedMessage{Role: llm.Role(role), Text: text, At: at}, true
}

// planHistory decides which conversations are new: the ones the Habakkuk
// home does not hold yet. It reads transcripts.db without changing it.
func (p *Plan) planHistory() error {
	f := p.Found
	if p.opts.History == HistorySkip || len(f.History) == 0 {
		return nil
	}
	have := map[string]bool{}
	path := filepath.Join(p.Home, "transcripts.db")
	if exists(path) {
		db, err := openRO(path)
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
			defer rows.Close()
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				have[id] = true
			}
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
		msgs, err := c.Load()
		if err != nil {
			f.skip("conversation %s: %v", c.ID, err)
			continue
		}
		if len(msgs) == 0 {
			continue // nothing said in it but tool calls, or nothing at all
		}
		it.hist = append(it.hist, c)
		it.histMsgs += len(msgs)
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
			msgs, err := c.Load()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", c.ID, err))
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
				Started: c.Started, Account: c.Account, Conversation: c.Chat, Thread: c.Thread, Messages: msgs})
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
