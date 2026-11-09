// Package memory is an agent's long-term memory. The source of truth is one
// Markdown file per memory under agents/<agent>/memory/, so people can read
// and edit them; a SQLite FTS5 index in memory.db makes them searchable and
// is brought up to date from file mtimes before every search and after every
// write, so hand edits are picked up without a restart. With an Embedder,
// notes are also embedded (cached by content hash) and search fuses the
// full-text and vector rankings.
package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/frontmatter"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
)

// Limits on what the model can store in one memory.
const (
	MaxTitle   = 200
	MaxContent = 64 << 10
	MaxTags    = 16
)

// Migrations is the schema for memory.db. memory_fts rowids are memories ids.
var Migrations = []store.Migration{{Version: 1, SQL: `
CREATE TABLE memories (
	id      INTEGER PRIMARY KEY,
	agent   TEXT NOT NULL,
	slug    TEXT NOT NULL,
	title   TEXT NOT NULL,
	tags    TEXT NOT NULL DEFAULT '', -- ",a,b," so a tag filter is one instr()
	updated INTEGER NOT NULL,         -- unix seconds, from frontmatter or mtime
	mtime   INTEGER NOT NULL,         -- file mtime in ns; with size, detects edits
	size    INTEGER NOT NULL,
	UNIQUE (agent, slug)
);
CREATE INDEX memories_recent ON memories (agent, updated DESC);
CREATE VIRTUAL TABLE memory_fts USING fts5(title, tags, body, tokenize = 'porter unicode61');
`}, {Version: 2, SQL: `
-- The index is rebuilt from the files, so it is simply emptied: the next
-- pass refills it, new columns included.
DELETE FROM memory_fts;
DELETE FROM memories;
ALTER TABLE memories ADD COLUMN trust TEXT NOT NULL DEFAULT '';
ALTER TABLE memories ADD COLUMN chash TEXT NOT NULL DEFAULT ''; -- sha256 of the text that is embedded
-- Vectors cached by content: a note that does not change is embedded once,
-- however often it is reindexed. vec is little-endian float32, unit length.
CREATE TABLE embeddings (
	model TEXT NOT NULL,
	hash  TEXT NOT NULL,
	vec   BLOB NOT NULL,
	used  INTEGER NOT NULL, -- unix seconds
	PRIMARY KEY (model, hash)
) WITHOUT ROWID;
-- When a note was last opened, so the nightly pass archives only notes that
-- are neither updated nor read. Kept apart from memories, which reindexing
-- rewrites.
CREATE TABLE recalls (
	agent TEXT NOT NULL,
	slug  TEXT NOT NULL,
	at    INTEGER NOT NULL,
	PRIMARY KEY (agent, slug)
) WITHOUT ROWID;
-- Background review: how far each session has been reviewed, and each
-- review or nightly job's planned changes with how many are applied, so a
-- job that crashed resumes where it stopped instead of asking the model
-- again and applying twice.
CREATE TABLE reviews (
	session TEXT PRIMARY KEY,
	agent   TEXT NOT NULL,
	through INTEGER NOT NULL, -- messages before this seq are reviewed
	at      INTEGER NOT NULL
) WITHOUT ROWID;
CREATE TABLE review_runs (
	job     TEXT PRIMARY KEY,
	agent   TEXT NOT NULL,
	kind    TEXT NOT NULL,
	ops     TEXT NOT NULL,
	next    INTEGER NOT NULL DEFAULT 0,
	done    INTEGER NOT NULL DEFAULT 0,
	summary TEXT NOT NULL DEFAULT '',
	at      INTEGER NOT NULL
) WITHOUT ROWID;
`}}

// Provenance values in a note's frontmatter.
const (
	SourceAgent  = "agent"  // memory_save
	SourceUser   = "user"   // /remember, the CLI
	SourceReview = "review" // background review and the nightly pass

	TrustUntrusted = "untrusted" // written from external content
	TrustTrusted   = "trusted"   // vouched for by the owner (habakkuk memory trust)
)

// Memory is one note.
type Memory struct {
	Slug    string
	Title   string
	Tags    []string
	Created time.Time
	Updated time.Time
	Body    string
	// Provenance: who wrote the note (SourceAgent, SourceUser,
	// SourceReview, or "" for a hand-written file), in which session, and
	// whether it came from external content.
	Source  string
	Session string
	Trust   string
}

// Untrusted reports whether the note was written from external content.
func (m Memory) Untrusted() bool { return m.Trust == TrustUntrusted }

// Entry is a memory as listed from the index.
type Entry struct {
	Slug      string
	Title     string
	Tags      []string
	Updated   time.Time
	Untrusted bool
}

// Hit is one search result.
type Hit struct {
	Entry
	Snippet string
}

// Note is a write to memory.
type Note struct {
	Title   string
	Content string
	Tags    []string // nil keeps an existing note's tags; empty clears them
	Source  string
	Session string
	// Untrusted marks content that came from outside: tool output from the
	// network, web pages, inbound email. Rewriting an untrusted note keeps
	// it untrusted unless the user writes it.
	Untrusted bool
	// Unique saves under a fresh slug (title-2, title-3, ...) instead of
	// replacing a note with the same title.
	Unique bool
}

// Store manages every agent's memory files and the shared index.
type Store struct {
	db   *sql.DB
	home string
	now  func() time.Time
	mu   sync.Mutex // serializes read-modify-write of files within this process

	emb     Embedder
	log     *slog.Logger
	embMu   sync.Mutex
	embDown time.Time // embedding calls are skipped until then, after a failure

	hist *history.Log // records every change to notes and USER.md; nil records nothing
}

// Open opens (creating if needed) $HABAKKUK_HOME/memory.db.
func Open(ctx context.Context, home string) (*Store, error) {
	db, err := store.Open(ctx, filepath.Join(home, "memory.db"), Migrations)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, home: home, now: time.Now, log: slog.New(slog.DiscardHandler)}, nil
}

func (s *Store) Close() { s.db.Close() }

// DB exposes the index for health checks and the review's bookkeeping.
func (s *Store) DB() *sql.DB { return s.db }

// UseEmbedder turns on semantic search. Call it before the store is used.
func (s *Store) UseEmbedder(e Embedder, log *slog.Logger) {
	s.emb = e
	if log != nil {
		s.log = log
	}
}

// Embedder is the embedder in use, or nil.
func (s *Store) Embedder() Embedder { return s.emb }

// Dir is where an agent's memory files live.
func (s *Store) Dir(agent string) string {
	return filepath.Join(s.home, "agents", agent, "memory")
}

func (s *Store) dir(agent string) (string, error) {
	if agent == "" || agent == "." || agent == ".." || strings.ContainsAny(agent, `/\`) {
		return "", fmt.Errorf("invalid agent name %q", agent)
	}
	return s.Dir(agent), nil
}

var (
	reSlugJunk = regexp.MustCompile(`[^a-z0-9]+`)
	reSlug     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
)

// Slugify derives a memory's file name from its title.
func Slugify(title string) string {
	s := reSlugJunk.ReplaceAllString(strings.ToLower(title), "-")
	s = strings.Trim(s, "-")
	if len(s) > 64 {
		s = strings.TrimRight(s[:64], "-")
	}
	return s
}

// ValidSlug reports whether slug can name a memory.
func ValidSlug(slug string) bool { return reSlug.MatchString(slug) }

func validSlug(slug string) error {
	if !reSlug.MatchString(slug) {
		return fmt.Errorf("invalid memory slug %q", slug)
	}
	return nil
}

// NormalizeTags lowercases tags, replaces anything but letters, digits, "-"
// and "_" with "-", and drops empties and duplicates, keeping order.
func NormalizeTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		t = strings.Trim(reTagJunk.ReplaceAllString(strings.ToLower(strings.TrimSpace(t)), "-"), "-")
		if t != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

var reTagJunk = regexp.MustCompile(`[^\p{L}\p{N}_-]+`)

// frontmatter as written to disk.
type meta struct {
	Title   string   `yaml:"title"`
	Tags    []string `yaml:"tags,flow"`
	Created string   `yaml:"created"`
	Updated string   `yaml:"updated"`
	Source  string   `yaml:"source,omitempty"`
	Session string   `yaml:"session,omitempty"`
	Trust   string   `yaml:"trust,omitempty"`
}

// looseMeta accepts what a person might type: tags as a list or a
// comma-separated string, dates with or without a time.
type looseMeta struct {
	Title   string `yaml:"title"`
	Tags    any    `yaml:"tags"`
	Created string `yaml:"created"`
	Updated string `yaml:"updated"`
	Source  string `yaml:"source"`
	Session string `yaml:"session"`
	Trust   string `yaml:"trust"`
}

func (m Memory) format() ([]byte, error) {
	return frontmatter.Format(meta{
		Title: m.Title, Tags: m.Tags,
		Created: m.Created.Format(time.RFC3339), Updated: m.Updated.Format(time.RFC3339),
		Source: m.Source, Session: m.Session, Trust: m.Trust,
	}, m.Body)
}

// Save creates or updates the memory whose slug derives from title. tags nil
// keeps an existing memory's tags; an empty slice clears them.
func (s *Store) Save(ctx context.Context, agent, title, content string, tags []string) (m Memory, created bool, err error) {
	return s.Put(ctx, agent, Note{Title: title, Content: content, Tags: tags})
}

// Put writes a note with its provenance: it creates or replaces the note
// whose slug derives from the title (or takes a fresh slug with Unique),
// refuses content that looks like a secret, and embeds the note when
// semantic search is on.
func (s *Store) Put(ctx context.Context, agent string, n Note) (m Memory, created bool, err error) {
	m, created, err = s.put(ctx, agent, n)
	if err == nil {
		s.EmbedPending(ctx, agent)
	}
	return m, created, err
}

func (s *Store) put(ctx context.Context, agent string, n Note) (m Memory, created bool, err error) {
	title := strings.Join(strings.Fields(n.Title), " ")
	content := strings.TrimSpace(n.Content)
	switch {
	case title == "":
		return m, false, errors.New("title is empty")
	case len(title) > MaxTitle:
		return m, false, fmt.Errorf("title is longer than %d characters", MaxTitle)
	case content == "":
		return m, false, errors.New("content is empty")
	case len(content) > MaxContent:
		return m, false, fmt.Errorf("content is longer than %d bytes; split it into several memories", MaxContent)
	}
	if err := CheckSecrets(title + "\n" + content); err != nil {
		return m, false, err
	}
	slug := Slugify(title)
	if slug == "" {
		return m, false, errors.New("title must contain letters or digits")
	}
	dir, err := s.dir(agent)
	if err != nil {
		return m, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if n.Unique {
		base := slug
		for i := 2; ; i++ {
			if _, err := os.Lstat(filepath.Join(dir, slug+".md")); errors.Is(err, fs.ErrNotExist) {
				break
			}
			if i > 99 {
				return m, false, fmt.Errorf("too many memories titled %q", title)
			}
			suffix := fmt.Sprintf("-%d", i)
			slug = strings.TrimRight(base[:min(len(base), 64-len(suffix))], "-") + suffix
		}
	}
	now := s.now().UTC().Truncate(time.Second)
	m = Memory{Slug: slug, Title: title, Created: now, Updated: now, Body: content, Tags: NormalizeTags(n.Tags),
		Source: n.Source, Session: n.Session}
	if n.Untrusted {
		m.Trust = TrustUntrusted
	}
	if len(m.Tags) > MaxTags {
		return m, false, fmt.Errorf("at most %d tags", MaxTags)
	}
	path := filepath.Join(dir, slug+".md")
	if old, err := readFile(path); err == nil {
		m.Created = old.Created
		if n.Tags == nil {
			m.Tags = old.Tags
		}
		// A rewrite by the agent or the review usually carries the old text
		// along, so external content stays marked until the user rewrites
		// the note or vouches for it.
		if old.Untrusted() && n.Source != SourceUser {
			m.Trust = TrustUntrusted
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		created = true
	} else {
		return m, false, err
	}
	data, err := m.format()
	if err != nil {
		return m, false, err
	}
	if err := s.writeTracked(ctx, history.NoteTarget(s.home, agent, slug), data); err != nil {
		return m, false, err
	}
	return m, created, s.reindex(ctx, agent, dir)
}

// Read returns a memory straight from its file.
func (s *Store) Read(ctx context.Context, agent, slug string) (Memory, error) {
	if err := validSlug(slug); err != nil {
		return Memory{}, err
	}
	dir, err := s.dir(agent)
	if err != nil {
		return Memory{}, err
	}
	m, err := readFile(filepath.Join(dir, slug+".md"))
	if errors.Is(err, fs.ErrNotExist) {
		return m, fmt.Errorf("no memory %q; find slugs with memory_search", slug)
	}
	return m, err
}

// SetTrust records whether a note may be relied on: TrustTrusted when the
// owner vouches for it, TrustUntrusted to mark it as external content. The
// note's dates and text are unchanged.
func (s *Store) SetTrust(ctx context.Context, agent, slug, trust string) (Memory, error) {
	if trust != TrustTrusted && trust != TrustUntrusted {
		return Memory{}, fmt.Errorf("trust must be %s or %s", TrustTrusted, TrustUntrusted)
	}
	if err := validSlug(slug); err != nil {
		return Memory{}, err
	}
	dir, err := s.dir(agent)
	if err != nil {
		return Memory{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(dir, slug+".md")
	m, err := readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, fmt.Errorf("no memory %q", slug)
	} else if err != nil {
		return m, err
	}
	m.Trust = trust
	data, err := m.format()
	if err != nil {
		return m, err
	}
	if err := s.writeTracked(ctx, history.NoteTarget(s.home, agent, slug), data); err != nil {
		return m, err
	}
	return m, s.reindex(ctx, agent, dir)
}

// Recalled records that a note was opened, for the nightly pass's staleness
// check. It writes at most once a day per note.
func (s *Store) Recalled(ctx context.Context, agent, slug string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO recalls (agent, slug, at) VALUES (?, ?, ?)
		ON CONFLICT (agent, slug) DO UPDATE SET at = excluded.at WHERE recalls.at < excluded.at - 86400`,
		agent, slug, s.now().Unix())
	return err
}

// LastRecalled is when each of an agent's notes was last opened.
func (s *Store) LastRecalled(ctx context.Context, agent string) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT slug, at FROM recalls WHERE agent = ?`, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var slug string
		var at int64
		if err := rows.Scan(&slug, &at); err != nil {
			return nil, err
		}
		out[slug] = time.Unix(at, 0).UTC()
	}
	return out, rows.Err()
}

// Forget moves a memory to memory/.trash/, where a person can still recover
// it, and drops it from the index.
func (s *Store) Forget(ctx context.Context, agent, slug string) (string, error) {
	if err := validSlug(slug); err != nil {
		return "", err
	}
	dir, err := s.dir(agent)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src := filepath.Join(dir, slug+".md")
	if _, err := os.Lstat(src); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("no memory %q", slug)
		}
		return "", err
	}
	trash := filepath.Join(dir, ".trash")
	if err := os.MkdirAll(trash, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(trash, fmt.Sprintf("%s.%s.md", slug, s.now().UTC().Format("20060102T150405.000000000")))
	if err := s.removeTracked(ctx, history.NoteTarget(s.home, agent, slug), func() error { return os.Rename(src, dst) }); err != nil {
		return "", err
	}
	return dst, s.reindex(ctx, agent, dir)
}

// Reindex brings an agent's index rows up to date with its files.
func (s *Store) Reindex(ctx context.Context, agent string) error {
	dir, err := s.dir(agent)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reindex(ctx, agent, dir)
}

type fileStat struct {
	mtime, size int64
}

// reindex compares file mtimes and sizes with the index and rewrites only the
// rows that differ. When nothing changed it does not open a write transaction.
func (s *Store) reindex(ctx context.Context, agent, dir string) error {
	files := map[string]fileStat{}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		slug, ok := strings.CutSuffix(e.Name(), ".md")
		if !ok || !e.Type().IsRegular() || validSlug(slug) != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed since ReadDir
		}
		files[slug] = fileStat{info.ModTime().UnixNano(), info.Size()}
	}

	rows, err := s.db.QueryContext(ctx, `SELECT id, slug, mtime, size FROM memories WHERE agent = ?`, agent)
	if err != nil {
		return err
	}
	indexed := map[string]fileStat{}
	ids := map[string]int64{}
	for rows.Next() {
		var slug string
		var id int64
		var st fileStat
		if err := rows.Scan(&id, &slug, &st.mtime, &st.size); err != nil {
			rows.Close()
			return err
		}
		indexed[slug], ids[slug] = st, id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var changed, gone []string
	for slug, st := range files {
		if old, ok := indexed[slug]; !ok || old != st {
			changed = append(changed, slug)
		}
	}
	for slug := range indexed {
		if _, ok := files[slug]; !ok {
			gone = append(gone, slug)
		}
	}
	if len(changed) == 0 && len(gone) == 0 {
		return nil
	}
	slices.Sort(changed)
	// A note that changed without going through this store was edited by
	// hand: the history records it (its own writes are already recorded).
	if s.hist != nil {
		for _, slug := range slices.Concat(changed, gone) {
			if err := s.hist.Observe(ctx, history.NoteTarget(s.home, agent, slug)); err != nil {
				s.log.Warn("memory history: hand edit check failed", "agent", agent, "slug", slug, "err", err)
			}
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, slug := range gone {
		if err := deleteRow(ctx, tx, ids[slug]); err != nil {
			return err
		}
	}
	for _, slug := range changed {
		m, err := readFile(filepath.Join(dir, slug+".md"))
		if err != nil {
			// Removed or replaced while we were indexing, or unreadable: leave
			// the old row (if any) and let the next pass settle it, rather than
			// failing every search over one file.
			continue
		}
		st := files[slug]
		if id, ok := ids[slug]; ok {
			if err := deleteRow(ctx, tx, id); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO memories (agent, slug, title, tags, updated, mtime, size, trust, chash) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			agent, slug, m.Title, joinTags(m.Tags), m.Updated.Unix(), st.mtime, st.size, m.Trust, hashText(embedText(m)))
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_fts (rowid, title, tags, body) VALUES (?, ?, ?, ?)`,
			id, m.Title, strings.Join(m.Tags, " "), m.Body); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func deleteRow(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE rowid = ?`, id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, id)
	return err
}

// readFile parses one memory file. A file without frontmatter (or with
// frontmatter that is not valid YAML) is all body, titled after its slug and
// dated by its mtime. Symlinks are refused so a memory cannot point outside
// the memory directory.
func readFile(path string) (Memory, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Memory{}, err
	}
	if !info.Mode().IsRegular() {
		return Memory{}, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Memory{}, err
	}
	var lm looseMeta
	body, _ := frontmatter.Parse(data, &lm)
	m := Memory{
		Slug:    strings.TrimSuffix(filepath.Base(path), ".md"),
		Title:   strings.Join(strings.Fields(lm.Title), " "),
		Body:    body,
		Source:  strings.TrimSpace(lm.Source),
		Session: strings.TrimSpace(lm.Session),
	}
	switch strings.ToLower(strings.TrimSpace(lm.Trust)) {
	case TrustUntrusted:
		m.Trust = TrustUntrusted
	case TrustTrusted:
		m.Trust = TrustTrusted
	}
	if m.Title == "" {
		m.Title = m.Slug
	}
	switch t := lm.Tags.(type) {
	case string:
		m.Tags = NormalizeTags(strings.Split(t, ","))
	case []any:
		var ss []string
		for _, v := range t {
			ss = append(ss, fmt.Sprint(v))
		}
		m.Tags = NormalizeTags(ss)
	}
	mtime := info.ModTime().UTC().Truncate(time.Second)
	m.Created = parseTime(lm.Created, mtime)
	m.Updated = parseTime(lm.Updated, mtime)
	return m, nil
}

func parseTime(s string, fallback time.Time) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t.UTC()
		}
	}
	return fallback
}

// maxEmbedChars caps the text a note's vector is computed from; embedding
// models take a few thousand tokens.
const maxEmbedChars = 8000

// embedText is what a note's vector is computed from.
func embedText(m Memory) string {
	t := m.Title + "\n"
	if len(m.Tags) > 0 {
		t += "tags: " + strings.Join(m.Tags, ", ") + "\n"
	}
	t += "\n" + m.Body
	return cutUTF8(t, maxEmbedChars)
}

func hashText(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// cutUTF8 shortens s to at most n bytes on a character boundary.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

func joinTags(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	return "," + strings.Join(tags, ",") + ","
}

func splitTags(s string) []string {
	s = strings.Trim(s, ",")
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

var reTerm = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// matchQuery turns free text into an FTS5 query: every word quoted (so the
// model cannot write a syntax error) and OR-ed, letting bm25 rank memories
// that match more of the words, and rarer words, higher.
func matchQuery(q string) string {
	terms := reTerm.FindAllString(q, 32)
	for i, t := range terms {
		terms[i] = `"` + t + `"`
	}
	return strings.Join(terms, " OR ")
}

// Search ranks an agent's memories against query. Full-text matches are
// ranked with bm25 (title matches weigh most, then tags, then body); with an
// embedder, notes are also ranked by cosine similarity to the query and the
// two rankings are fused (reciprocal rank fusion), so a note phrased
// differently from the query is still found. Any embedding failure falls
// back to full text alone. Every tag in tags must be present. An empty query
// with tags lists the tagged memories, newest first.
func (s *Store) Search(ctx context.Context, agent, query string, tags []string, limit int) ([]Hit, error) {
	if limit <= 0 {
		limit = 10
	}
	limit = min(limit, 50)
	if err := s.Reindex(ctx, agent); err != nil {
		return nil, err
	}
	tags = NormalizeTags(tags)
	filter, filterArgs := tagFilter(tags)
	mq := matchQuery(query)
	if mq == "" {
		if len(tags) == 0 {
			return nil, errors.New("query is empty: give words to search for, or tags")
		}
		return s.queryHits(ctx, `SELECT m.slug, m.title, m.tags, m.updated, m.trust, '' FROM memories m
			WHERE m.agent = ?`+filter+` ORDER BY m.updated DESC, m.slug LIMIT ?`,
			append(append([]any{agent}, filterArgs...), limit)...)
	}
	pool := limit
	if s.emb != nil {
		pool = max(limit*3, 30) // a deeper list for fusion to draw on
	}
	text, err := s.queryHits(ctx, `SELECT m.slug, m.title, m.tags, m.updated, m.trust, snippet(memory_fts, 2, '[', ']', '…', 16)
		FROM memory_fts JOIN memories m ON m.id = memory_fts.rowid
		WHERE memory_fts MATCH ? AND m.agent = ?`+filter+`
		ORDER BY bm25(memory_fts, 10.0, 5.0, 1.0), m.updated DESC, m.slug LIMIT ?`,
		append(append([]any{mq, agent}, filterArgs...), pool)...)
	if err != nil {
		return nil, err
	}
	if s.emb == nil {
		return text, nil
	}
	sem, err := s.semantic(ctx, agent, query, filter, filterArgs, pool)
	if err != nil {
		s.log.Warn("semantic memory search failed; using full-text results", "agent", agent, "err", err)
		return text[:min(len(text), limit)], nil
	}
	return fuse(text, sem, limit), nil
}

func tagFilter(tags []string) (string, []any) {
	var where []string
	var args []any
	for _, t := range tags {
		where = append(where, "instr(m.tags, ?) > 0")
		args = append(args, ","+t+",")
	}
	if len(where) == 0 {
		return "", nil
	}
	return " AND " + strings.Join(where, " AND "), args
}

func (s *Store) queryHits(ctx context.Context, q string, args ...any) ([]Hit, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var h Hit
		var tags, trust string
		var updated int64
		if err := rows.Scan(&h.Slug, &h.Title, &tags, &updated, &trust, &h.Snippet); err != nil {
			return nil, err
		}
		h.Tags, h.Updated, h.Untrusted = splitTags(tags), time.Unix(updated, 0).UTC(), trust == TrustUntrusted
		h.Snippet = strings.Join(strings.Fields(h.Snippet), " ")
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// rrfK damps the weight of top ranks in reciprocal rank fusion; 60 is the
// customary value.
const rrfK = 60

// fuse merges two rankings by reciprocal rank fusion: each list adds
// 1/(rrfK+rank) to a note's score. A note found by full text keeps its
// snippet with the matched words marked.
func fuse(text, sem []Hit, limit int) []Hit {
	score := map[string]float64{}
	byslug := map[string]Hit{}
	var order []string
	add := func(list []Hit) {
		for i, h := range list {
			if _, ok := byslug[h.Slug]; !ok {
				byslug[h.Slug] = h
				order = append(order, h.Slug)
			}
			score[h.Slug] += 1 / float64(rrfK+i+1)
		}
	}
	add(text)
	add(sem)
	slices.SortStableFunc(order, func(a, b string) int {
		switch {
		case score[a] > score[b]:
			return -1
		case score[a] < score[b]:
			return 1
		}
		return 0
	})
	out := make([]Hit, 0, min(limit, len(order)))
	for _, slug := range order[:min(limit, len(order))] {
		out = append(out, byslug[slug])
	}
	return out
}

// Recent lists up to limit memories, most recently updated first, and the
// total number the agent has.
func (s *Store) Recent(ctx context.Context, agent string, limit int) ([]Entry, int, error) {
	if err := s.Reindex(ctx, agent); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM memories WHERE agent = ?`, agent).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT slug, title, tags, updated, trust FROM memories WHERE agent = ? ORDER BY updated DESC, slug LIMIT ?`, agent, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var tags, trust string
		var updated int64
		if err := rows.Scan(&e.Slug, &e.Title, &tags, &updated, &trust); err != nil {
			return nil, 0, err
		}
		e.Tags, e.Updated, e.Untrusted = splitTags(tags), time.Unix(updated, 0).UTC(), trust == TrustUntrusted
		out = append(out, e)
	}
	return out, total, rows.Err()
}
