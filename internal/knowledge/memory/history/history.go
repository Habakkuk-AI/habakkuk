// Package history records every change to what an agent remembers: its
// memory notes, USER.md, IDENTITY.md and the skills in its own folder.
// Each change keeps the content before and after, who made it (the agent
// through a tool, the user, the background review, the skills curator, or a
// hand edit on disk), in which session, whether external content had
// entered that turn, and when. Any change can be shown as a diff and either
// side of it restored; a restore is recorded like any other change.
//
// Changes live in $HABAKKUK_HOME/history.db. Content is stored once per
// distinct text (content-addressed, zlib-compressed), so a note rewritten a
// hundred times with small edits costs little.
//
// A file write and its history row cannot share one transaction, so a
// change is written in three steps: a pending row (with both contents) is
// committed first, then the file is written atomically, then the row is
// marked committed. A pending row is never listed. After a crash, the
// next recovery pass compares the file with the pending row: if the file
// holds the new content the row is committed, otherwise it is dropped. So
// a change never appears without having happened, and a change that
// happened is never lost; one the database could not keep (a power cut
// before SQLite synced) is still caught by hand-edit detection, below.
//
// Hand edits: for every tracked file the log remembers the hash, mtime and
// size it last saw. Scan (run when the server starts and once a minute)
// and Observe (run when the memory index notices a changed note) compare
// the file on disk with that, and record a difference as a change by
// "hand". The first scan of an agent only takes a baseline, so upgrading
// does not log every existing file as new.
//
// Pins: a pinned note, profile or skill is never changed by a background
// job (actor Background); the write is refused with ErrPinned.
package history

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// What a change is to.
const (
	KindNote     = "note"     // agents/<agent>/memory/<slug>.md
	KindProfile  = "profile"  // agents/<agent>/USER.md
	KindIdentity = "identity" // agents/<agent>/IDENTITY.md
	KindSkill    = "skill"    // agents/<agent>/skills/<name>/SKILL.md
)

// Who made a change.
const (
	ByAgent   = "agent"   // the agent, through a tool
	ByUser    = "user"    // the user: a chat command, the CLI, the dashboard
	ByReview  = "review"  // background memory review and the nightly pass
	ByCurator = "curator" // the skills curator
	ByHand    = "hand"    // an edit made on disk, noticed afterwards
	BySystem  = "system"  // anything that did not say
)

// File names of the single-file kinds.
const (
	ProfileFile  = "USER.md"
	IdentityFile = "IDENTITY.md"
	SkillFile    = "SKILL.md"
)

// ErrPinned refuses a background change to a pinned file.
var ErrPinned = errors.New("pinned, so background jobs leave it alone")

// staleAfter is how old a pending row must be before recovery settles it;
// younger ones may belong to a write still in flight in another process.
const staleAfter = time.Minute

// Migrations is the schema of history.db.
var Migrations = []store.Migration{{Version: 1, SQL: `
-- Content, stored once per distinct text.
CREATE TABLE blobs (
	hash TEXT PRIMARY KEY, -- sha256 of the raw content
	size INTEGER NOT NULL, -- raw length
	z    INTEGER NOT NULL, -- 1 when data is zlib-compressed
	data BLOB NOT NULL
) WITHOUT ROWID;
CREATE TABLE changes (
	seq      INTEGER PRIMARY KEY AUTOINCREMENT,
	id       TEXT NOT NULL UNIQUE,  -- short random id people type
	state    INTEGER NOT NULL,      -- 0 pending (file write in flight), 1 committed
	agent    TEXT NOT NULL,
	kind     TEXT NOT NULL,
	name     TEXT NOT NULL,         -- slug, USER.md, IDENTITY.md or skill name
	path     TEXT NOT NULL,         -- relative to the home directory
	old_hash TEXT NOT NULL,         -- '' when the file did not exist
	new_hash TEXT NOT NULL,         -- '' when the change removed it
	actor    TEXT NOT NULL,
	tool     TEXT NOT NULL,
	session  TEXT NOT NULL,
	tainted  INTEGER NOT NULL,
	note     TEXT NOT NULL,
	restores TEXT NOT NULL,         -- for a restore, the change it went back to
	at       INTEGER NOT NULL       -- unix milliseconds
);
CREATE INDEX changes_agent ON changes (agent, seq);
CREATE INDEX changes_path ON changes (path, seq);
CREATE INDEX changes_old ON changes (old_hash);
CREATE INDEX changes_new ON changes (new_hash);
-- What each tracked file held when last seen, to notice hand edits.
CREATE TABLE files (
	path  TEXT PRIMARY KEY,
	agent TEXT NOT NULL,
	kind  TEXT NOT NULL,
	name  TEXT NOT NULL,
	hash  TEXT NOT NULL, -- '' when last seen absent
	mtime INTEGER NOT NULL,
	size  INTEGER NOT NULL
) WITHOUT ROWID;
-- Agents whose files have had a first scan.
CREATE TABLE baselines (
	agent TEXT PRIMARY KEY,
	at    INTEGER NOT NULL
) WITHOUT ROWID;
CREATE TABLE pins (
	agent TEXT NOT NULL,
	kind  TEXT NOT NULL,
	name  TEXT NOT NULL,
	at    INTEGER NOT NULL,
	PRIMARY KEY (agent, kind, name)
) WITHOUT ROWID;
`}}

// Target is one tracked file.
type Target struct {
	Agent string
	Kind  string
	Name  string
	Path  string // absolute
}

// Change is one recorded change.
type Change struct {
	ID       string    `json:"id"`
	Agent    string    `json:"agent"`
	Kind     string    `json:"kind"`
	Name     string    `json:"name"`
	Path     string    `json:"path"` // relative to the home directory
	Old      string    `json:"old_hash,omitempty"`
	New      string    `json:"new_hash,omitempty"`
	By       string    `json:"by"`
	Tool     string    `json:"tool,omitempty"`
	Session  string    `json:"session,omitempty"`
	Tainted  bool      `json:"tainted"`
	Note     string    `json:"note,omitempty"`
	Restores string    `json:"restores,omitempty"`
	At       time.Time `json:"at"`

	seq int64
}

// Op is what the change did: created, updated or deleted.
func (c Change) Op() string {
	switch {
	case c.Old == "":
		return "created"
	case c.New == "":
		return "deleted"
	}
	return "updated"
}

// Describe is one line about the change, for people and the model.
func (c Change) Describe() string {
	what := c.Name
	switch c.Kind {
	case KindNote:
		what = "note " + c.Name
	case KindSkill:
		what = "skill " + c.Name
	}
	by := c.By
	if c.Tool != "" && c.Tool != c.By {
		by += " via " + c.Tool
	}
	line := fmt.Sprintf("%s  %s  %s %s by %s", c.ID, c.At.Local().Format("2006-01-02 15:04"), what, c.Op(), by)
	if c.Session != "" {
		line += ", session " + c.Session
	}
	if c.Tainted {
		line += " (external content was in the turn)"
	}
	if c.Note != "" {
		line += "; " + c.Note
	}
	return line
}

// Actor says who is making the changes a context carries.
type Actor struct {
	By      string // ByAgent, ByUser, ...
	Tool    string // the tool, command or job: memory_save, /remember, cli, memory_review, ...
	Session string
	// Tainted marks a change made in a turn that external content entered.
	Tainted bool
	// Background marks a job nobody is watching (the review, the curator):
	// it may not change pinned files.
	Background bool
	Note       string

	restores string
}

type actorKey struct{}

// WithActor returns ctx carrying a.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom is the actor ctx carries; By is BySystem when none was set.
func ActorFrom(ctx context.Context) Actor {
	a, _ := ctx.Value(actorKey{}).(Actor)
	if a.By == "" {
		a.By = BySystem
	}
	return a
}

// Log is the change history of every agent under one home directory.
type Log struct {
	db   *sql.DB
	home string
	now  func() time.Time
	log  *slog.Logger
	mu   sync.Mutex // one change at a time in this process: read before, write, commit

	// crash, in tests, stops a change after a step as if the process died.
	crash func(step string) bool
}

var errCrash = errors.New("simulated crash")

// Open opens (creating if needed) $HABAKKUK_HOME/history.db and settles
// changes a crashed process left pending.
func Open(ctx context.Context, home string) (*Log, error) {
	db, err := store.Open(ctx, filepath.Join(home, "history.db"), Migrations)
	if err != nil {
		return nil, err
	}
	l := &Log{db: db, home: home, now: time.Now, log: slog.New(slog.DiscardHandler)}
	if err := l.recover(ctx, ""); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
}

// SetLogger sets where warnings go.
func (l *Log) SetLogger(log *slog.Logger) {
	if log != nil {
		l.log = log
	}
}

func (l *Log) Close() { l.db.Close() }

// Home is the directory paths are relative to.
func (l *Log) Home() string { return l.home }

// Layout: where each kind of file lives.

// AgentDir is an agent's directory.
func AgentDir(home, agent string) string { return filepath.Join(home, "agents", agent) }

// NoteTarget is a memory note.
func NoteTarget(home, agent, slug string) Target {
	return Target{Agent: agent, Kind: KindNote, Name: slug, Path: filepath.Join(AgentDir(home, agent), "memory", slug+".md")}
}

// ProfileTarget is an agent's USER.md.
func ProfileTarget(home, agent string) Target {
	return Target{Agent: agent, Kind: KindProfile, Name: ProfileFile, Path: filepath.Join(AgentDir(home, agent), ProfileFile)}
}

// IdentityTarget is an agent's IDENTITY.md.
func IdentityTarget(home, agent string) Target {
	return Target{Agent: agent, Kind: KindIdentity, Name: IdentityFile, Path: filepath.Join(AgentDir(home, agent), IdentityFile)}
}

// SkillTarget is the SKILL.md of one of an agent's own skills.
func SkillTarget(home, agent, name string) Target {
	return Target{Agent: agent, Kind: KindSkill, Name: name, Path: filepath.Join(AgentDir(home, agent), "skills", name, SkillFile)}
}

var reName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func validAgent(agent string) error {
	if agent == "" || agent == "." || agent == ".." || strings.ContainsAny(agent, `/\`) {
		return fmt.Errorf("invalid agent name %q", agent)
	}
	return nil
}

func (t Target) check() error {
	if err := validAgent(t.Agent); err != nil {
		return err
	}
	switch t.Kind {
	case KindNote, KindProfile, KindIdentity, KindSkill:
	default:
		return fmt.Errorf("unknown kind %q", t.Kind)
	}
	if t.Name == "" || !filepath.IsAbs(t.Path) {
		return fmt.Errorf("incomplete history target %+v", t)
	}
	return nil
}

// rel stores paths relative to home, so a moved home keeps its history.
func (l *Log) rel(path string) string {
	r, err := filepath.Rel(l.home, path)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return path
	}
	return filepath.ToSlash(r)
}

// Abs turns a change's path back into a file path.
func (l *Log) Abs(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(l.home, filepath.FromSlash(path))
}

func hashOf(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// Write records and makes a change that leaves data in t's file. write
// does the actual writing (atomically). When the file already holds data,
// write still runs but nothing is recorded and the change is nil.
func (l *Log) Write(ctx context.Context, t Target, data []byte, write func() error) (*Change, error) {
	if data == nil {
		data = []byte{}
	}
	return l.apply(ctx, t, data, true, write)
}

// Remove records and makes a change that removes t's file; remove does it
// (moving the file to a trash folder, say).
func (l *Log) Remove(ctx context.Context, t Target, remove func() error) (*Change, error) {
	return l.apply(ctx, t, nil, false, remove)
}

// seen is a file as found on disk.
type seen struct {
	exists      bool
	hash        string
	data        []byte // nil when it was not read (unchanged since last seen)
	mtime, size int64
}

func (l *Log) apply(ctx context.Context, t Target, data []byte, exists bool, fn func() error) (*Change, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	a := ActorFrom(ctx)
	l.mu.Lock()
	defer l.mu.Unlock()
	if a.Background {
		pinned, err := l.Pinned(ctx, t.Agent, t.Kind, t.Name)
		if err != nil {
			return nil, err
		}
		if pinned {
			return nil, fmt.Errorf("%s %s is %w", t.Kind, t.Name, ErrPinned)
		}
	}
	// A hand edit since the file was last seen is recorded first, so this
	// change's "before" is what really was there.
	cur, err := l.observe(ctx, t)
	if err != nil {
		return nil, err
	}
	newHash := ""
	if exists {
		newHash = hashOf(data)
	}
	if newHash == cur.hash {
		if err := fn(); err != nil {
			return nil, err
		}
		return nil, l.track(context.WithoutCancel(ctx), nil, t, newHash)
	}
	c := &Change{
		Agent: t.Agent, Kind: t.Kind, Name: t.Name, Path: l.rel(t.Path), Old: cur.hash, New: newHash,
		By: a.By, Tool: a.Tool, Session: a.Session, Tainted: a.Tainted, Note: a.Note, Restores: a.restores,
		At: l.now().UTC().Truncate(time.Millisecond),
	}
	var blobs [][]byte
	if cur.data != nil {
		blobs = append(blobs, cur.data)
	}
	if exists {
		blobs = append(blobs, data)
	}
	if err := l.insert(ctx, c, false, blobs, nil); err != nil {
		return nil, err
	}
	if l.crash != nil && l.crash("pending") {
		return nil, errCrash
	}
	if err := fn(); err != nil {
		if _, derr := l.db.ExecContext(context.WithoutCancel(ctx), `DELETE FROM changes WHERE seq = ? AND state = 0`, c.seq); derr != nil {
			l.log.Warn("history: dropping a failed change", "id", c.ID, "err", derr)
		}
		return nil, err
	}
	if l.crash != nil && l.crash("written") {
		return nil, errCrash
	}
	if err := l.commit(context.WithoutCancel(ctx), c, t); err != nil {
		// The file changed; recovery commits the pending row later.
		l.log.Warn("history: commit failed; it is settled on the next scan", "id", c.ID, "err", err)
	}
	return c, nil
}

// insert stores blobs and a change row (pending, or committed with the
// file's new state when file is set) in one transaction.
func (l *Log) insert(ctx context.Context, c *Change, committed bool, blobs [][]byte, file *Target) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, b := range blobs {
		if err := putBlob(ctx, tx, b); err != nil {
			return err
		}
	}
	state := 0
	if committed {
		state = 1
	}
	for try := 0; ; try++ {
		c.ID = newID()
		res, err := tx.ExecContext(ctx, `INSERT INTO changes (id, state, agent, kind, name, path, old_hash, new_hash,
			actor, tool, session, tainted, note, restores, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO NOTHING`,
			c.ID, state, c.Agent, c.Kind, c.Name, c.Path, c.Old, c.New, c.By, c.Tool, c.Session, c.Tainted, c.Note, c.Restores, c.At.UnixMilli())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			if c.seq, err = res.LastInsertId(); err != nil {
				return err
			}
			break
		}
		if try == 8 {
			return errors.New("history: no free change id")
		}
	}
	if file != nil {
		if err := l.trackTx(ctx, tx, *file, c.New); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// commit marks a pending change done and remembers the file's new state.
func (l *Log) commit(ctx context.Context, c *Change, t Target) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE changes SET state = 1 WHERE seq = ?`, c.seq); err != nil {
		return err
	}
	if err := l.trackTx(ctx, tx, t, c.New); err != nil {
		return err
	}
	return tx.Commit()
}

// track remembers what t's file holds now (hash, with its current mtime
// and size), storing the content too when data is given.
func (l *Log) track(ctx context.Context, data []byte, t Target, hash string) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if data != nil {
		if err := putBlob(ctx, tx, data); err != nil {
			return err
		}
	}
	if err := l.trackTx(ctx, tx, t, hash); err != nil {
		return err
	}
	return tx.Commit()
}

func (l *Log) trackTx(ctx context.Context, tx *sql.Tx, t Target, hash string) error {
	var mtime, size int64
	if hash != "" {
		if info, err := os.Lstat(t.Path); err == nil {
			mtime, size = info.ModTime().UnixNano(), info.Size()
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO files (path, agent, kind, name, hash, mtime, size) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (path) DO UPDATE SET hash = excluded.hash, mtime = excluded.mtime, size = excluded.size`,
		l.rel(t.Path), t.Agent, t.Kind, t.Name, hash, mtime, size)
	return err
}

func putBlob(ctx context.Context, tx *sql.Tx, data []byte) error {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	stored, z := data, 0
	if buf.Len() < len(data) {
		stored, z = buf.Bytes(), 1
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO blobs (hash, size, z, data) VALUES (?, ?, ?, ?) ON CONFLICT (hash) DO NOTHING`,
		hashOf(data), len(data), z, stored)
	return err
}

// Content returns the text a hash names; nil for "" (no file).
func (l *Log) Content(ctx context.Context, hash string) ([]byte, error) {
	if hash == "" {
		return nil, nil
	}
	var z int
	var data []byte
	err := l.db.QueryRowContext(ctx, `SELECT z, data FROM blobs WHERE hash = ?`, hash).Scan(&z, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("history: content %s is gone", hash[:min(12, len(hash))])
	}
	if err != nil || z == 0 {
		return data, err
	}
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type fileRow struct {
	hash        string
	mtime, size int64
}

func (l *Log) fileRow(ctx context.Context, path string) (fileRow, bool, error) {
	var r fileRow
	err := l.db.QueryRowContext(ctx, `SELECT hash, mtime, size FROM files WHERE path = ?`, path).Scan(&r.hash, &r.mtime, &r.size)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// read looks at t's file. With a row whose mtime and size match, the file
// is taken as unchanged and not read.
func read(t Target, row fileRow, known bool) (seen, error) {
	info, err := os.Lstat(t.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return seen{}, nil
	}
	if err != nil {
		return seen{}, err
	}
	if !info.Mode().IsRegular() {
		return seen{}, fmt.Errorf("%s is not a regular file", t.Path)
	}
	s := seen{exists: true, mtime: info.ModTime().UnixNano(), size: info.Size()}
	if known && row.hash != "" && row.mtime == s.mtime && row.size == s.size {
		s.hash = row.hash
		return s, nil
	}
	data, err := os.ReadFile(t.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return seen{}, nil
	}
	if err != nil {
		return seen{}, err
	}
	s.data, s.hash = data, hashOf(data)
	return s, nil
}

// Observe compares t's file with what the log last saw and records a
// difference as a hand edit. The memory index calls it for every note it
// finds changed.
func (l *Log) Observe(ctx context.Context, t Target) error {
	if err := t.check(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.observe(ctx, t)
	return err
}

func (l *Log) observe(ctx context.Context, t Target) (seen, error) {
	path := l.rel(t.Path)
	row, known, err := l.fileRow(ctx, path)
	if err != nil {
		return seen{}, err
	}
	cur, err := read(t, row, known)
	if err != nil {
		return seen{}, err
	}
	if known && row.hash == cur.hash {
		if cur.exists && (row.mtime != cur.mtime || row.size != cur.size) {
			return cur, l.track(ctx, nil, t, cur.hash) // touched, not changed
		}
		return cur, nil
	}
	// A pending change that wrote this content: commit it once it is
	// stale (its process died after writing), or leave it to the writer.
	settled, err := l.settlePending(ctx, path, t, cur.hash)
	if err != nil || settled {
		return cur, err
	}
	if !known {
		if !cur.exists {
			return cur, nil
		}
		var base int
		if err := l.db.QueryRowContext(ctx, `SELECT count(*) FROM baselines WHERE agent = ?`, t.Agent).Scan(&base); err != nil {
			return cur, err
		}
		if base == 0 {
			return cur, l.track(ctx, cur.data, t, cur.hash) // first sight: baseline only
		}
	}
	c := &Change{
		Agent: t.Agent, Kind: t.Kind, Name: t.Name, Path: path, Old: row.hash, New: cur.hash,
		By: ByHand, At: l.now().UTC().Truncate(time.Millisecond),
	}
	switch c.Op() {
	case "created":
		c.Note = "created on disk"
	case "deleted":
		c.Note = "deleted on disk"
	default:
		c.Note = "edited on disk"
	}
	var blobs [][]byte
	if cur.data != nil {
		blobs = append(blobs, cur.data)
	}
	return cur, l.insert(ctx, c, true, blobs, &t)
}

// settlePending looks at path's pending changes. One whose new content is
// on disk is committed when stale (true: the file is accounted for) or left
// to its writer when fresh (also true). Stale ones that did not happen are
// dropped.
func (l *Log) settlePending(ctx context.Context, path string, t Target, hash string) (bool, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT seq, new_hash, at FROM changes WHERE state = 0 AND path = ? ORDER BY seq`, path)
	if err != nil {
		return false, err
	}
	type pend struct {
		seq  int64
		hash string
		at   int64
	}
	var ps []pend
	for rows.Next() {
		var p pend
		if err := rows.Scan(&p.seq, &p.hash, &p.at); err != nil {
			rows.Close()
			return false, err
		}
		ps = append(ps, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	cutoff := l.now().Add(-staleAfter).UnixMilli()
	settled := false
	for _, p := range ps {
		stale := p.at < cutoff
		switch {
		case p.hash == hash && !settled && stale:
			if err := l.commit(ctx, &Change{seq: p.seq, New: p.hash}, t); err != nil {
				return false, err
			}
			settled = true
		case p.hash == hash && !settled:
			settled = true // in flight in another process
		case stale:
			if _, err := l.db.ExecContext(ctx, `DELETE FROM changes WHERE seq = ? AND state = 0`, p.seq); err != nil {
				return false, err
			}
		}
	}
	return settled, nil
}

// recover settles stale pending changes (of one agent, or all with "").
func (l *Log) recover(ctx context.Context, agent string) error {
	q := `SELECT DISTINCT agent, kind, name, path FROM changes WHERE state = 0 AND at < ?`
	args := []any{l.now().Add(-staleAfter).UnixMilli()}
	if agent != "" {
		q += ` AND agent = ?`
		args = append(args, agent)
	}
	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	var ts []Target
	for rows.Next() {
		var t Target
		var path string
		if err := rows.Scan(&t.Agent, &t.Kind, &t.Name, &path); err != nil {
			rows.Close()
			return err
		}
		t.Path = l.Abs(path)
		ts = append(ts, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range ts {
		row, known, err := l.fileRow(ctx, l.rel(t.Path))
		if err != nil {
			return err
		}
		cur, err := read(t, row, known)
		if err != nil {
			continue
		}
		if _, err := l.settlePending(ctx, l.rel(t.Path), t, cur.hash); err != nil {
			return err
		}
	}
	return nil
}

// Targets lists the files tracked for an agent that are on disk: notes,
// USER.md, IDENTITY.md and the agent's own skills.
func (l *Log) Targets(agent string) []Target {
	ts := []Target{ProfileTarget(l.home, agent), IdentityTarget(l.home, agent)}
	dir := AgentDir(l.home, agent)
	if entries, err := os.ReadDir(filepath.Join(dir, "memory")); err == nil {
		for _, e := range entries {
			slug, ok := strings.CutSuffix(e.Name(), ".md")
			if ok && e.Type().IsRegular() && reName.MatchString(slug) {
				ts = append(ts, NoteTarget(l.home, agent, slug))
			}
		}
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "skills")); err == nil {
		for _, e := range entries {
			if e.IsDir() && reName.MatchString(e.Name()) {
				ts = append(ts, SkillTarget(l.home, agent, e.Name()))
			}
		}
	}
	return ts
}

// Scan records hand edits to every tracked file of an agent, including
// files removed since they were last seen. An agent's first scan takes a
// baseline instead.
func (l *Log) Scan(ctx context.Context, agent string) error {
	if err := validAgent(agent); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.recover(ctx, agent); err != nil {
		return err
	}
	ts := l.Targets(agent)
	have := map[string]bool{}
	for _, t := range ts {
		have[l.rel(t.Path)] = true
	}
	rows, err := l.db.QueryContext(ctx, `SELECT path, kind, name FROM files WHERE agent = ? AND hash != ''`, agent)
	if err != nil {
		return err
	}
	for rows.Next() {
		var path string
		t := Target{Agent: agent}
		if err := rows.Scan(&path, &t.Kind, &t.Name); err != nil {
			rows.Close()
			return err
		}
		if !have[path] {
			t.Path = l.Abs(path)
			ts = append(ts, t)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range ts {
		if _, err := l.observe(ctx, t); err != nil {
			l.log.Warn("history: scan", "path", t.Path, "err", err)
		}
	}
	_, err = l.db.ExecContext(ctx, `INSERT INTO baselines (agent, at) VALUES (?, ?) ON CONFLICT (agent) DO NOTHING`,
		agent, l.now().Unix())
	return err
}

// Pins

// Pin keeps background jobs from changing a note, the profile or a skill.
func (l *Log) Pin(ctx context.Context, agent, kind, name string) error {
	if err := validAgent(agent); err != nil {
		return err
	}
	_, err := l.db.ExecContext(ctx, `INSERT INTO pins (agent, kind, name, at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		agent, kind, name, l.now().Unix())
	return err
}

// Unpin lets background jobs change it again; false when it was not pinned.
func (l *Log) Unpin(ctx context.Context, agent, kind, name string) (bool, error) {
	res, err := l.db.ExecContext(ctx, `DELETE FROM pins WHERE agent = ? AND kind = ? AND name = ?`, agent, kind, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Pinned reports whether background jobs must leave a file alone.
func (l *Log) Pinned(ctx context.Context, agent, kind, name string) (bool, error) {
	var n int
	err := l.db.QueryRowContext(ctx, `SELECT count(*) FROM pins WHERE agent = ? AND kind = ? AND name = ?`, agent, kind, name).Scan(&n)
	return n > 0, err
}

// Pin is one pinned file.
type Pin struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// Pins lists an agent's pinned files.
func (l *Log) Pins(ctx context.Context, agent string) ([]Pin, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT kind, name FROM pins WHERE agent = ? ORDER BY kind, name`, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pin
	for rows.Next() {
		var p Pin
		if err := rows.Scan(&p.Kind, &p.Name); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Reading the history

// Filter selects changes; zero fields match everything.
type Filter struct {
	Agent   string
	File    string // a name (slug, USER.md, skill name), a file name or a path
	Kind    string
	Session string
	Since   time.Time
	Limit   int // default 50
}

const changeCols = `seq, id, agent, kind, name, path, old_hash, new_hash, actor, tool, session, tainted, note, restores, at`

func scanChange(sc interface{ Scan(...any) error }) (Change, error) {
	var c Change
	var at int64
	err := sc.Scan(&c.seq, &c.ID, &c.Agent, &c.Kind, &c.Name, &c.Path, &c.Old, &c.New, &c.By, &c.Tool, &c.Session,
		&c.Tainted, &c.Note, &c.Restores, &at)
	c.At = time.UnixMilli(at).UTC()
	return c, err
}

// List returns committed changes, newest first.
func (l *Log) List(ctx context.Context, f Filter) ([]Change, error) {
	q := `SELECT ` + changeCols + ` FROM changes WHERE state = 1`
	var args []any
	if f.Agent != "" {
		q += ` AND agent = ?`
		args = append(args, f.Agent)
	}
	if f.Kind != "" {
		q += ` AND kind = ?`
		args = append(args, f.Kind)
	}
	if f.Session != "" {
		q += ` AND session = ?`
		args = append(args, f.Session)
	}
	if !f.Since.IsZero() {
		q += ` AND at >= ?`
		args = append(args, f.Since.UnixMilli())
	}
	if file := strings.TrimSpace(f.File); file != "" {
		q += ` AND (name = ? OR name = ? OR path = ? OR path LIKE ? ESCAPE '\')`
		args = append(args, file, strings.TrimSuffix(file, ".md"), file, "%/"+likeEscape(file))
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	q += ` ORDER BY seq DESC LIMIT ?`
	args = append(args, limit)
	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		c, err := scanChange(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

var reID = regexp.MustCompile(`^[0-9a-f]{4,8}$`)

// Get finds a committed change by its id or a prefix of at least four
// characters.
func (l *Log) Get(ctx context.Context, id string) (Change, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if !reID.MatchString(id) {
		return Change{}, fmt.Errorf("%q is not a change id (4 to 8 hex digits, as the history lists them)", id)
	}
	rows, err := l.db.QueryContext(ctx, `SELECT `+changeCols+` FROM changes WHERE state = 1 AND id LIKE ? ORDER BY seq DESC LIMIT 2`, id+"%")
	if err != nil {
		return Change{}, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		c, err := scanChange(rows)
		if err != nil {
			return Change{}, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return Change{}, err
	}
	switch len(out) {
	case 0:
		return Change{}, fmt.Errorf("no change %s", id)
	case 1:
		return out[0], nil
	}
	return Change{}, fmt.Errorf("change id %s is ambiguous; give more of it", id)
}

// Diff is the unified diff of a change.
func (l *Log) Diff(ctx context.Context, c Change) (string, error) {
	old, err := l.Content(ctx, c.Old)
	if err != nil {
		return "", err
	}
	cur, err := l.Content(ctx, c.New)
	if err != nil {
		return "", err
	}
	a, b := "a/"+c.Path, "b/"+c.Path
	if c.Old == "" {
		a = "/dev/null"
	}
	if c.New == "" {
		b = "/dev/null"
	}
	return Unified(a, b, string(old), string(cur)), nil
}

// Version names a side of a change for Restore.
const (
	Before = "before"
	After  = "after"
)

// Restore puts back one side of a change: Before undoes it, After redoes
// it. It is recorded as a change by the context's actor, noting what it
// restored. The change is nil when the file already holds that version.
func (l *Log) Restore(ctx context.Context, id, version string) (Change, *Change, error) {
	c, err := l.Get(ctx, id)
	if err != nil {
		return c, nil, err
	}
	hash := c.Old
	switch version {
	case "", Before:
		version = Before
	case After:
		hash = c.New
	default:
		return c, nil, fmt.Errorf("version %q must be before or after", version)
	}
	data, err := l.Content(ctx, hash)
	if err != nil {
		return c, nil, err
	}
	t := Target{Agent: c.Agent, Kind: c.Kind, Name: c.Name, Path: l.Abs(c.Path)}
	a := ActorFrom(ctx)
	a.Background = false
	a.restores = c.ID
	a.Note = fmt.Sprintf("restored the version %s change %s", version, c.ID)
	if a.Tool == "" {
		a.Tool = "restore"
	}
	ctx = WithActor(ctx, a)
	var r *Change
	if hash == "" {
		r, err = l.Remove(ctx, t, func() error { return trash(t.Path, l.now()) })
	} else {
		r, err = l.Write(ctx, t, data, func() error {
			if err := os.MkdirAll(filepath.Dir(t.Path), 0o700); err != nil {
				return err
			}
			return store.WriteFileAtomic(t.Path, data, 0o600)
		})
	}
	return c, r, err
}

// trash moves a file into a .trash folder beside it, where a person can
// still find it.
func trash(path string, now time.Time) error {
	dir := filepath.Join(filepath.Dir(path), ".trash")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	base := strings.TrimSuffix(filepath.Base(path), ".md")
	return os.Rename(path, filepath.Join(dir, fmt.Sprintf("%s.%s.md", base, now.UTC().Format("20060102T150405.000000000"))))
}

// Prune deletes changes older than cutoff, except the newest change of
// every file that exists, and the content nothing refers to any more. It
// returns how many changes it deleted.
func (l *Log) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM changes WHERE at < ? AND seq NOT IN (
		SELECT max(c.seq) FROM changes c JOIN files f ON f.path = c.path
		WHERE c.state = 1 AND f.hash != '' GROUP BY c.path)`, cutoff.UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE hash = ''
		AND NOT EXISTS (SELECT 1 FROM changes c WHERE c.path = files.path)`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM blobs
		WHERE NOT EXISTS (SELECT 1 FROM changes WHERE old_hash = blobs.hash)
		AND NOT EXISTS (SELECT 1 FROM changes WHERE new_hash = blobs.hash)
		AND NOT EXISTS (SELECT 1 FROM files WHERE files.hash = blobs.hash)`); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if n > 0 {
		if _, err := l.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			return n, err
		}
	}
	return n, nil
}

// ParseSince reads "7d", "12h", "30m" or a date (2006-01-02) as the start
// of a time window ending now.
func ParseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		var n int
		if _, err := fmt.Sscanf(days, "%d", &n); err == nil && n >= 0 && fmt.Sprint(n) == days {
			return now.AddDate(0, 0, -n), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("since %q: use a span like 7d, 12h or 30m, or a date like 2026-01-31", s)
}
