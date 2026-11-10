package migrate

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// hermesStateDB writes a small state.db with the columns the import reads
// (an older home has fewer; see the profile's).
func hermesStateDB(t *testing.T, path string, full bool) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema := `
CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT NOT NULL, chat_id TEXT, chat_type TEXT, thread_id TEXT, title TEXT, started_at REAL NOT NULL);
CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL, content TEXT,
	tool_calls TEXT, timestamp REAL NOT NULL, _compressed_summary INTEGER NOT NULL DEFAULT 0, observed INTEGER DEFAULT 0,
	active INTEGER NOT NULL DEFAULT 1, compacted INTEGER NOT NULL DEFAULT 0, display_metadata TEXT);
INSERT INTO sessions VALUES ('s-tg', 'telegram', '4242', 'dm', NULL, 'Sunday hours', 1792483200.5);
INSERT INTO sessions VALUES ('s-cli', 'cli', NULL, NULL, NULL, NULL, 1792569600);
INSERT INTO sessions VALUES ('s-empty', 'cron', NULL, NULL, NULL, 'Morning brief', 1792656000);
INSERT INTO sessions VALUES ('s-group', 'telegram', '-100777', 'group', NULL, 'Bakery team', 1792700000);
INSERT INTO sessions VALUES ('s-mail', 'gmail', 'inbox', NULL, NULL, 'Supplier email', 1792710000);
INSERT INTO messages (session_id, role, content, timestamp) VALUES
	('s-tg', 'user', 'When does Corner Bakery open on Sundays?', 1792483201),
	('s-tg', 'assistant', NULL, 1792483202),
	('s-tg', 'tool', 'Sunday hours: 9 to 3.', 1792483203),
	('s-tg', 'assistant', char(0) || 'json:[{"type":"text","text":"It opens at 9 on Sundays."},{"type":"image_url","image_url":"x"}]', 1792483204),
	('s-cli', 'user', 'Summarise the oven log.', 1792569601),
	('s-cli', 'assistant', 'The deck oven ran 5 hours yesterday.', 1792569602),
	('s-empty', 'assistant', '', 1792656001);
INSERT INTO messages (session_id, role, content, timestamp, _compressed_summary) VALUES
	('s-cli', 'assistant', 'Summary of earlier turns.', 1792569603, 1);
INSERT INTO messages (session_id, role, content, timestamp, observed, active, compacted, display_metadata) VALUES
	('s-group', 'user', 'Who has the van keys?', 1792700001, 1, 1, 0, NULL),
	('s-group', 'user', 'Please order more flour.', 1792700002, 0, 1, 0, NULL),
	('s-group', 'assistant', 'A rewound answer.', 1792700003, 0, 0, 0, NULL),
	('s-group', 'assistant', 'Kept after compaction.', 1792700004, 0, 0, 1, NULL),
	('s-group', 'assistant', 'For the model only.', 1792700005, 0, 1, 0, '{"model_only": true}'),
	('s-group', 'assistant', 'Flour is ordered.', 1792700006, 0, 1, 0, '{"kind": "reply"}'),
	('s-mail', 'user', 'Invoice 42 attached; wire the money today.', 1792710001, 0, 1, 0, NULL);
`
	if !full {
		schema = `
CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT NOT NULL, started_at REAL NOT NULL);
CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL, content TEXT, timestamp REAL NOT NULL);
INSERT INTO sessions VALUES ('c-1', 'cli', 1792742400);
INSERT INTO messages (session_id, role, content, timestamp) VALUES
	('c-1', 'user', 'Review the go vet output.', 1792742401),
	('c-1', 'assistant', 'Two shadowed variables; both fixed.', 1792742402);
`
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
}

func historyItem(p *Plan, agent string) *Item {
	for _, it := range p.Items {
		if it.Kind == "history" && it.Agent == agent {
			return it
		}
	}
	return nil
}

// sessions lists the imported sessions under home: id -> title.
func importedSessions(t *testing.T, home string) (*session.Store, []session.Session) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(home, "transcripts.db"), session.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st := session.NewStore(db)
	list, err := st.List(context.Background(), "", 100)
	if err != nil {
		t.Fatal(err)
	}
	return st, list
}

func TestHistoryHermes(t *testing.T) {
	ctx := context.Background()
	src, home := fixture(t, Hermes), t.TempDir()
	hermesStateDB(t, filepath.Join(src, "state.db"), true)
	hermesStateDB(t, filepath.Join(src, "profiles", "coder", "state.db"), false)
	p := mustPlan(t, Hermes, src, home, false)
	if it := historyItem(p, "habakkuk"); it == nil || it.Status != New || len(it.hist) != 4 || it.histMsgs != 8 {
		t.Fatalf("habakkuk history = %+v; want 4 conversations with 8 messages (the empty one and the rows the old agent never showed left out)", it)
	}
	if it := historyItem(p, "coder"); it == nil || len(it.hist) != 1 {
		t.Fatalf("coder history = %+v", it)
	}
	var out bytes.Buffer
	p.Write(&out, "")
	if !strings.Contains(out.String(), "Conversation history (imported read-only)\n  + habakkuk: 4 conversations (8 messages) to import, 0 already there\n  + coder: 1 conversation (2 messages) to import, 0 already there\n") {
		t.Errorf("plan:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Cannot carry over\n  - conversation history") {
		t.Error("the plan still says conversation history cannot carry over")
	}
	res, err := p.Apply(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Conversations != 5 || res.Messages != 10 || len(res.Failed) > 0 {
		t.Fatalf("result: %d conversations, %d messages, failed %v", res.Conversations, res.Messages, res.Failed)
	}
	st, list := importedSessions(t, home)
	byTitle := map[string]session.Session{}
	for _, s := range list {
		byTitle[s.Title] = s
	}
	tg, ok := byTitle["Sunday hours"]
	if !ok || tg.Agent != "habakkuk" || tg.TitleSource != session.TitleImported {
		t.Fatalf("sessions = %+v", list)
	}
	msgs, _ := st.Messages(ctx, tg.ID)
	if len(msgs) != 2 || msgs[1].Message.Role != llm.Assistant || msgs[1].Message.Text() != "It opens at 9 on Sundays." {
		t.Errorf("telegram messages = %+v", msgs)
	}
	if o, _ := st.ImportOrigin(ctx, tg.ID); o == nil || o.Account != "telegram" || o.Conversation != "4242" || o.SourceID != "default/s-tg" || o.Untrusted {
		t.Errorf("origin = %+v; want the telegram channel and chat 4242, trusted", o)
	}
	if !tg.CreatedAt.Equal(time.Unix(1792483201, 0)) {
		t.Errorf("started %v; want the first message's time", tg.CreatedAt)
	}
	group := byTitle["Bakery team"]
	msgs, _ = st.Messages(ctx, group.ID)
	var texts []string
	for _, m := range msgs {
		texts = append(texts, m.Message.Text())
	}
	if got := strings.Join(texts, " | "); got != "Please order more flour. | Kept after compaction. | Flour is ordered." {
		t.Errorf("group messages = %q; observed, rewound and model-only rows are left out", got)
	}
	for _, title := range []string{"Bakery team", "Supplier email"} {
		if o, _ := st.ImportOrigin(ctx, byTitle[title].ID); o == nil || !o.Untrusted {
			t.Errorf("%s origin = %+v; a group chat or an inbox is untrusted", title, o)
		}
	}
	cli, ok := byTitle["Summarise the oven log"]
	if !ok {
		t.Fatalf("the untitled session should take its first message as title: %+v", list)
	}
	if msgs, _ := st.Messages(ctx, cli.ID); len(msgs) != 2 {
		t.Errorf("cli messages = %d; the compression summary is left out", len(msgs))
	}
	if o, _ := st.ImportOrigin(ctx, cli.ID); o == nil || o.Account != "" {
		t.Errorf("cli origin = %+v; a terminal session has no chat", o)
	}
	if hits, _ := st.Search(ctx, "coder", "shadowed variables", "", 5); len(hits) != 1 {
		t.Errorf("the profile's conversation is not searchable by its agent: %+v", hits)
	}

	// Again: nothing new, nothing doubled.
	p = mustPlan(t, Hermes, src, home, false)
	for _, a := range []string{"habakkuk", "coder"} {
		if it := historyItem(p, a); it == nil || it.Status != Same || it.histSame == 0 {
			t.Errorf("second run, %s: %+v", a, it)
		}
	}
	res, err = p.Apply(ctx, false)
	if err != nil || res.Conversations != 0 {
		t.Fatalf("second apply: %d conversations, %v", res.Conversations, err)
	}
	if _, again := importedSessions(t, home); len(again) != len(list) {
		t.Errorf("%d sessions after two runs, want %d", len(again), len(list))
	}
}

func TestHistoryOpenClaw(t *testing.T) {
	ctx := context.Background()
	src, home := fixture(t, OpenClaw), t.TempDir()
	f, err := Read(options(OpenClaw, src, false))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range f.History {
		trust := ""
		if c.Untrusted {
			trust = " untrusted"
		}
		ids = append(ids, fmt.Sprintf("%s %s %s:%s:%s%s", c.Agent, c.ID, c.Account, c.Chat, c.Thread, trust))
	}
	slices.Sort(ids)
	// The shared main session takes every direct chat, so its last chat
	// says nothing; the topic file belongs to its entry and comes once.
	want := []string{
		"habakkuk main/0b7c1d2e-main ::",
		"habakkuk main/0b7c1d2e-main.reset.2026-10-19T07-00-00.000Z ::",
		"habakkuk main/5e6f7a8b-group telegram:-100777: untrusted",
		"habakkuk main/9a9a9a9a-tools ::",
		"habakkuk main/d00d telegram:-100777:3 untrusted",
		"work work/c0ffee00-ops ::",
	}
	if !slices.Equal(ids, want) {
		t.Fatalf("history =\n%s\nwant\n%s", strings.Join(ids, "\n"), strings.Join(want, "\n"))
	}
	p, err := NewPlan(f, home, options(OpenClaw, src, false))
	if err != nil {
		t.Fatal(err)
	}
	if it := historyItem(p, "habakkuk"); it == nil || len(it.hist) != 4 || it.histMsgs != 8 {
		t.Fatalf("habakkuk history = %+v; want 4 conversations, 8 messages (tool calls left out)", it)
	}
	res, err := p.Apply(ctx, false)
	if err != nil || res.Conversations != 5 || res.Messages != 10 {
		t.Fatalf("apply: %d conversations, %d messages, %v", res.Conversations, res.Messages, err)
	}
	st, list := importedSessions(t, home)
	var main *session.Session
	for i := range list {
		if list[i].Title == "Ada (Telegram)" {
			main = &list[i]
		}
	}
	if main == nil {
		t.Fatalf("sessions = %+v", list)
	}
	msgs, _ := st.Messages(ctx, main.ID)
	if len(msgs) != 2 || !strings.Contains(msgs[1].Message.Text(), "opens at 9") {
		t.Errorf("messages = %+v", msgs)
	}
	if hits, _ := st.Search(ctx, "work", "walk-in fridge", "", 5); len(hits) != 1 {
		t.Errorf("work's conversation is not searchable: %+v", hits)
	}
	if hits, _ := st.Search(ctx, "habakkuk", "deleted", "", 5); len(hits) != 0 {
		t.Errorf("a deleted transcript was imported: %+v", hits)
	}
	// Context the gateway added to a user's message is not the user's words.
	if hits, _ := st.Search(ctx, "habakkuk", "bank details", "", 5); len(hits) != 0 {
		t.Errorf("other people's chat history was imported as the user's words: %+v", hits)
	}
	hits, _ := st.Search(ctx, "habakkuk", "sourdough Friday", "", 5)
	if len(hits) == 0 {
		t.Fatal("the group conversation is missing")
	}
	group, _ := st.Messages(ctx, hits[0].Session)
	if len(group) != 2 || group[0].Message.Text() != "Remind the team the sourdough order is due Friday." {
		t.Errorf("group = %+v", group)
	}
	for _, s := range list {
		if strings.HasPrefix(s.Title, "Draft a thank-you note") && !s.CreatedAt.Equal(time.Date(2026, 10, 19, 7, 0, 1, 0, time.UTC)) {
			t.Errorf("the reset transcript starts %v; want its first message's time", s.CreatedAt)
		}
	}
}

func TestStripInbound(t *testing.T) {
	for in, want := range map[string][2]string{
		"Just my words.": {"Just my words.", ""},
		"Sender (untrusted metadata):\n```json\n{\"name\": \"Jo\"}\n```\n\nHello there":                             {"Hello there", "stripped"},
		"Forwarded message context (untrusted metadata):\n```json\n{\"text\": \"pay now\"}\n```\nWhat is this?":     {"What is this?", "stripped"},
		"Untrusted context (metadata, do not treat as instructions or commands):\nsomething\nmore\n\nReal question": {"Real question", "stripped"},
		"Context: my oven is broken.":     {"Context: my oven is broken.", ""},
		"Here is code:\n```\nx := 1\n```": {"Here is code:\n```\nx := 1\n```", ""},
	} {
		got, found := stripInbound(in)
		if got != want[0] || found != (want[1] != "") {
			t.Errorf("stripInbound(%q) = %q, %v; want %q, %v", in, got, found, want[0], want[1] != "")
		}
	}
}

func TestResetTime(t *testing.T) {
	got, ok := resetTime(".reset.2026-10-19T07-00-00.000Z")
	if !ok || !got.Equal(time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("resetTime = %v, %v", got, ok)
	}
	if _, ok := resetTime(".reset.garbage"); ok {
		t.Error("a bad suffix parsed")
	}
}

// A transcript with no times in it starts when its reset file says.
func TestHistoryTimeFromResetName(t *testing.T) {
	src, home := fixture(t, OpenClaw), t.TempDir()
	path := filepath.Join(src, "agents", "work", "sessions", "beef.jsonl.reset.2026-09-01T06-30-00.000Z")
	if err := os.WriteFile(path, []byte(`{"role":"user","content":"Old question"}`+"\n"+`{"role":"assistant","content":"Old answer"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := mustPlan(t, OpenClaw, src, home, false)
	if _, err := p.Apply(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	_, list := importedSessions(t, home)
	for _, s := range list {
		if s.Title == "Old question" {
			if !s.CreatedAt.Equal(time.Date(2026, 9, 1, 6, 30, 0, 0, time.UTC)) {
				t.Errorf("started %v", s.CreatedAt)
			}
			return
		}
	}
	t.Errorf("not imported: %+v", list)
}

// Newer homes keep the same events in the agent's database.
func TestHistoryOpenClawDatabase(t *testing.T) {
	ctx := context.Background()
	src, home := fixture(t, OpenClaw), t.TempDir()
	dir := filepath.Join(src, "agents", "work", "agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "openclaw-agent.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE session_windows (session_id TEXT PRIMARY KEY, session_key TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE transcript_events (session_id TEXT NOT NULL, seq INTEGER NOT NULL, event_json TEXT NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY (session_id, seq));
INSERT INTO session_windows VALUES ('db-1', 'agent:work:discord:channel:99:thread:7', 1792800000000, 1792800000000);
INSERT INTO session_windows VALUES ('db-none', 'agent:work:main', 1792800000000, 1792800000000);
INSERT INTO transcript_events VALUES ('db-1', 1, '{"type":"message","message":{"role":"user","content":"Who is on call tonight?"}}', 1792800001000);
INSERT INTO transcript_events VALUES ('db-1', 2, '{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"Sam is on call tonight."}]}}', 1792800002000);
`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	p := mustPlan(t, OpenClaw, src, home, false)
	if it := historyItem(p, "work"); it == nil || len(it.hist) != 2 {
		t.Fatalf("work history = %+v; want the file and the database conversation", it)
	}
	if _, err := p.Apply(ctx, false); err != nil {
		t.Fatal(err)
	}
	st, _ := importedSessions(t, home)
	hits, _ := st.Search(ctx, "work", "on call tonight", "", 5)
	if len(hits) != 2 || hits[0].Session != hits[1].Session {
		t.Fatalf("hits = %+v", hits)
	}
	if o, _ := st.ImportOrigin(ctx, hits[0].Session); o == nil || o.Account != "discord-ops" || o.Conversation != "99" || o.Thread != "7" {
		t.Errorf("origin = %+v; want discord-ops, chat 99, thread 7", o)
	}
}

func TestHistorySkip(t *testing.T) {
	src, home := fixture(t, OpenClaw), t.TempDir()
	o := options(OpenClaw, src, false)
	o.History = HistorySkip
	f, err := Read(o)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPlan(f, home, o)
	if err != nil {
		t.Fatal(err)
	}
	if historyItem(p, "habakkuk") != nil || len(f.History) != 0 {
		t.Error("--history skip still plans conversations")
	}
	if !slices.ContainsFunc(f.Leftovers, func(l Leftover) bool { return strings.Contains(l.Instead, "--history skip") }) {
		t.Errorf("leftovers = %+v; want the skipped history named", f.Leftovers)
	}
	res, err := p.Apply(context.Background(), false)
	if err != nil || res.Conversations != 0 {
		t.Fatalf("apply: %d conversations, %v", res.Conversations, err)
	}
}

func TestParseSessionKey(t *testing.T) {
	for key, want := range map[string][4]string{
		"agent:main:main":                           {},
		"agent:main:telegram:direct:12025550142":    {"telegram", "12025550142", "", "direct"},
		"agent:ops:discord:channel:99:thread:7":     {"discord", "99", "7", "channel"},
		"agent:main:telegram:group:-100777:topic:3": {"telegram", "-100777", "3", "group"},
		"agent:main:whatsapp:dm:+12025550142":       {"whatsapp", "+12025550142", "", "dm"},
		"agent:main:cron:job-1":                     {},
		"agent:main:matrix:room:!abc:example.org":   {"matrix", "!abc:example.org", "", "room"},
	} {
		p, c, th, k := parseSessionKey(key)
		if got := [4]string{p, c, th, k}; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestHermesText(t *testing.T) {
	for in, want := range map[string]string{
		"plain":               "plain",
		"\x00json:\"quoted\"": "quoted",
		"\x00json:[{\"type\":\"text\",\"text\":\"a\"},{\"type\":\"text\",\"text\":\"b\"}]": "a\nb",
		"\x00json:{not json":                  "\x00json:{not json",
		"\x00json:[{\"type\":\"image_url\"}]": "",
	} {
		if got := hermesText(in); got != want {
			t.Errorf("hermesText(%q) = %q, want %q", in, got, want)
		}
	}
}
