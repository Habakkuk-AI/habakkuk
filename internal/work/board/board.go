// Package board is a task board shared by agents. One agent (or the user)
// puts a card on the board for another; the dispatcher turns each ready card
// into a job that runs the assignee, and the result travels back to whoever
// asked. Every state change is a compare-and-swap inside one write
// transaction, so several processes can share the board and a crash at any
// point leaves it consistent.
package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// Migrations for control.db; they follow the schedule migrations.
var Migrations = []store.Migration{{Version: 4, SQL: `
CREATE TABLE cards (
	id              TEXT PRIMARY KEY,
	title           TEXT NOT NULL,
	body            TEXT NOT NULL DEFAULT '',
	assignee        TEXT NOT NULL DEFAULT '',
	created_by      TEXT NOT NULL DEFAULT '',
	creator_session TEXT NOT NULL DEFAULT '',
	status          TEXT NOT NULL CHECK (status IN ('todo','doing','blocked','done','cancelled')),
	priority        INTEGER NOT NULL DEFAULT 0,
	result          TEXT NOT NULL DEFAULT '',
	parent          TEXT NOT NULL DEFAULT '',
	reply_to        TEXT NOT NULL DEFAULT '',
	attempt         INTEGER NOT NULL DEFAULT 0,
	job             TEXT NOT NULL DEFAULT '',
	session         TEXT NOT NULL DEFAULT '',
	rev             INTEGER NOT NULL DEFAULT 0,
	notify          INTEGER NOT NULL DEFAULT 0,
	created_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL
);
CREATE INDEX cards_status ON cards (status, assignee);
CREATE INDEX cards_notify ON cards (notify) WHERE notify = 1;
CREATE TABLE card_deps (
	card       TEXT NOT NULL,
	depends_on TEXT NOT NULL,
	PRIMARY KEY (card, depends_on)
);
CREATE INDEX card_deps_on ON card_deps (depends_on);
CREATE TABLE card_comments (
	id     INTEGER PRIMARY KEY,
	card   TEXT NOT NULL,
	author TEXT NOT NULL,
	text   TEXT NOT NULL,
	at     INTEGER NOT NULL
);
CREATE INDEX card_comments_card ON card_comments (card, id);
CREATE TABLE card_events (
	id     INTEGER PRIMARY KEY,
	card   TEXT NOT NULL,
	actor  TEXT NOT NULL,
	kind   TEXT NOT NULL,
	detail TEXT NOT NULL DEFAULT '',
	at     INTEGER NOT NULL
);
CREATE INDEX card_events_card ON card_events (card, id);
`}}

// Status is where a card is in its life.
type Status string

const (
	Todo      Status = "todo"
	Doing     Status = "doing"
	Blocked   Status = "blocked"
	Done      Status = "done"
	Cancelled Status = "cancelled"
)

// User is the actor name for changes made from the CLI.
const User = "user"

// Card is one piece of work on the board.
type Card struct {
	ID        string
	Title     string
	Body      string
	Assignee  string
	CreatedBy string // agent name, or User
	// CreatorSession is the session the card was created from; the result
	// is sent back there as a new turn.
	CreatorSession string
	Status         Status
	Priority       int // higher is dispatched first
	Result         string
	Parent         string
	DependsOn      []string
	ReplyTo        json.RawMessage // a chat to tell when the card ends, or empty
	Attempt        int             // how many times it has been dispatched
	Job            string          // the job of the latest attempt
	Session        string          // the assignee's session for the latest attempt
	Rev            int             // bumped on every status change
	CreatedAt      time.Time
	UpdatedAt      time.Time

	Comments []Comment // filled by Get
	Events   []Event   // filled by Get

	notify bool
}

// Comment is a note on a card from an agent or the user.
type Comment struct {
	Author string
	Text   string
	At     time.Time
}

// Event is one line of a card's history.
type Event struct {
	Actor  string
	Kind   string
	Detail string
	At     time.Time
}

// Open reports whether the card can still change hands or be worked on.
func (c *Card) Open() bool { return c.Status != Done && c.Status != Cancelled }

// Store keeps the board in control.db.
type Store struct {
	db  *sql.DB
	q   *jobs.Queue
	now func() time.Time
}

// NewStore returns a board over db. The queue is used to stop a card's
// running job when the card is cancelled or handed to someone else.
func NewStore(db *sql.DB, q *jobs.Queue) *Store {
	return &Store{db: db, q: q, now: time.Now}
}

const columns = `id, title, body, assignee, created_by, creator_session, status, priority, result, parent,
	reply_to, attempt, job, session, rev, notify, created_at, updated_at`

type scanner interface{ Scan(...any) error }

func scan(r scanner) (*Card, error) {
	var c Card
	var to string
	var created, updated int64
	if err := r.Scan(&c.ID, &c.Title, &c.Body, &c.Assignee, &c.CreatedBy, &c.CreatorSession, &c.Status,
		&c.Priority, &c.Result, &c.Parent, &to, &c.Attempt, &c.Job, &c.Session, &c.Rev, &c.notify,
		&created, &updated); err != nil {
		return nil, err
	}
	if to != "" {
		c.ReplyTo = json.RawMessage(to)
	}
	c.CreatedAt, c.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return &c, nil
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Store) query(ctx context.Context, q querier, where string, args ...any) ([]Card, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+columns+` FROM cards c `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Card
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].DependsOn, err = deps(ctx, q, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func deps(ctx context.Context, q querier, id string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT depends_on FROM card_deps WHERE card = ? ORDER BY depends_on`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func load(ctx context.Context, q querier, id string) (*Card, error) {
	c, err := scan(q.QueryRowContext(ctx, `SELECT `+columns+` FROM cards WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("no card %s", id)
	}
	if err != nil {
		return nil, err
	}
	c.DependsOn, err = deps(ctx, q, id)
	return c, err
}

// Create puts a new card on the board. Dependencies and the parent must
// already exist, so dependency cycles cannot form.
func (s *Store) Create(ctx context.Context, c Card) (*Card, error) {
	c.Title = strings.TrimSpace(c.Title)
	if c.Title == "" {
		return nil, errors.New("a card needs a title")
	}
	if c.CreatedBy == "" {
		c.CreatedBy = User
	}
	now := s.now()
	c.ID, c.Status, c.CreatedAt, c.UpdatedAt = jobs.NewID("crd"), Todo, now, now
	c.Attempt, c.Job, c.Session, c.Rev, c.Result = 0, "", "", 0, ""
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if c.Parent != "" {
		if _, err := load(ctx, tx, c.Parent); err != nil {
			return nil, fmt.Errorf("parent: %w", err)
		}
	}
	seen := map[string]bool{}
	var ds []string
	for _, d := range c.DependsOn {
		if d = strings.TrimSpace(d); d == "" || seen[d] {
			continue
		}
		seen[d] = true
		if _, err := load(ctx, tx, d); err != nil {
			return nil, fmt.Errorf("depends_on: %w", err)
		}
		ds = append(ds, d)
	}
	c.DependsOn = ds
	if _, err := tx.ExecContext(ctx, `INSERT INTO cards (id, title, body, assignee, created_by, creator_session,
		status, priority, parent, reply_to, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Title, c.Body, c.Assignee, c.CreatedBy, c.CreatorSession, c.Status, c.Priority, c.Parent,
		string(c.ReplyTo), now.UnixMilli(), now.UnixMilli()); err != nil {
		return nil, err
	}
	for _, d := range ds {
		if _, err := tx.ExecContext(ctx, `INSERT INTO card_deps (card, depends_on) VALUES (?, ?)`, c.ID, d); err != nil {
			return nil, err
		}
	}
	detail := "for " + c.Assignee
	if c.Assignee == "" {
		detail = "unassigned"
	}
	if err := addEvent(ctx, tx, c.ID, c.CreatedBy, "created", detail, now); err != nil {
		return nil, err
	}
	return &c, tx.Commit()
}

func addEvent(ctx context.Context, tx *sql.Tx, card, actor, kind, detail string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO card_events (card, actor, kind, detail, at) VALUES (?, ?, ?, ?, ?)`,
		card, actor, kind, detail, at.UnixMilli())
	return err
}

// Get returns a card with its comments and history.
func (s *Store) Get(ctx context.Context, id string) (*Card, error) {
	c, err := load(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT author, text, at FROM card_comments WHERE card = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cm Comment
		var at int64
		if err := rows.Scan(&cm.Author, &cm.Text, &at); err != nil {
			return nil, err
		}
		cm.At = time.UnixMilli(at)
		c.Comments = append(c.Comments, cm)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ev, err := s.db.QueryContext(ctx, `SELECT actor, kind, detail, at FROM card_events WHERE card = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer ev.Close()
	for ev.Next() {
		var e Event
		var at int64
		if err := ev.Scan(&e.Actor, &e.Kind, &e.Detail, &at); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at)
		c.Events = append(c.Events, e)
	}
	return c, ev.Err()
}

// Filter narrows List. Zero values match everything; Status "" means open
// cards (todo, doing, blocked) and "all" means every card.
type Filter struct {
	Status    string
	Assignee  string
	CreatedBy string
	Limit     int // default 50
}

// List returns cards, highest priority and oldest first.
func (s *Store) List(ctx context.Context, f Filter) ([]Card, error) {
	var where []string
	var args []any
	switch f.Status {
	case "":
		where = append(where, `status IN ('todo','doing','blocked')`)
	case "all":
	default:
		where = append(where, `status = ?`)
		args = append(args, f.Status)
	}
	if f.Assignee != "" {
		where = append(where, `assignee = ?`)
		args = append(args, f.Assignee)
	}
	if f.CreatedBy != "" {
		where = append(where, `created_by = ?`)
		args = append(args, f.CreatedBy)
	}
	q := ""
	if len(where) > 0 {
		q = "WHERE " + strings.Join(where, " AND ")
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	args = append(args, f.Limit)
	return s.query(ctx, s.db, q+` ORDER BY priority DESC, created_at LIMIT ?`, args...)
}

// Ready returns cards that can be dispatched now: todo, assigned, and every
// dependency done.
func (s *Store) Ready(ctx context.Context) ([]Card, error) {
	return s.query(ctx, s.db, `WHERE c.status = 'todo' AND c.assignee != ''
		AND NOT EXISTS (SELECT 1 FROM card_deps d JOIN cards x ON x.id = d.depends_on
			WHERE d.card = c.id AND x.status != 'done')
		ORDER BY c.priority DESC, c.created_at`)
}

// change collects what a mutation adds besides the card's own columns.
type change struct {
	events   []Event
	comments []Comment
	cancel   string // a job to stop after the change commits
}

func (ch *change) event(actor, kind, detail string) {
	ch.events = append(ch.events, Event{Actor: actor, Kind: kind, Detail: detail})
}

func (ch *change) comment(author, text string) {
	ch.comments = append(ch.comments, Comment{Author: author, Text: text})
}

// errNoChange makes mutate roll back quietly and report no change.
var errNoChange = errors.New("no change")

// mutate reads a card, lets fn change it, and writes it back, all in one
// write transaction, so concurrent changes from several processes apply one
// at a time. A status change bumps rev, and a card that ends up done or
// blocked owes a notification. It returns the card after the change.
func (s *Store) mutate(ctx context.Context, id string, fn func(c *Card, ch *change) error) (*Card, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	c, err := load(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	before := c.Status
	ch := &change{}
	if err := fn(c, ch); errors.Is(err, errNoChange) {
		return c, nil
	} else if err != nil {
		return nil, err
	}
	now := s.now()
	if c.Status != before {
		c.Rev++
		c.notify = c.Status == Done || c.Status == Blocked
		ch.event(actorOf(ch), string(c.Status), "")
	}
	c.UpdatedAt = now
	if _, err := tx.ExecContext(ctx, `UPDATE cards SET title = ?, body = ?, assignee = ?, status = ?, priority = ?,
		result = ?, attempt = ?, job = ?, session = ?, rev = ?, notify = ?, updated_at = ? WHERE id = ?`,
		c.Title, c.Body, c.Assignee, c.Status, c.Priority, c.Result, c.Attempt, c.Job, c.Session, c.Rev,
		c.notify, now.UnixMilli(), c.ID); err != nil {
		return nil, err
	}
	for _, cm := range ch.comments {
		if _, err := tx.ExecContext(ctx, `INSERT INTO card_comments (card, author, text, at) VALUES (?, ?, ?, ?)`,
			c.ID, cm.Author, cm.Text, now.UnixMilli()); err != nil {
			return nil, err
		}
	}
	for _, e := range ch.events {
		if err := addEvent(ctx, tx, c.ID, e.Actor, e.Kind, e.Detail, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if ch.cancel != "" && s.q != nil {
		// The job's worker sees the cancel on its next heartbeat and stops;
		// anything it tries to write to the card after that fails the
		// status checks.
		if err := s.q.Cancel(ctx, ch.cancel); err != nil {
			return c, err
		}
	}
	return c, nil
}

// actorOf names who made a status change: the first actor recorded in the
// change, or the board itself.
func actorOf(ch *change) string {
	if len(ch.events) > 0 {
		return ch.events[0].Actor
	}
	if len(ch.comments) > 0 {
		return ch.comments[0].Author
	}
	return "board"
}

// Comment adds a note to a card.
func (s *Store) Comment(ctx context.Context, id, author, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("empty comment")
	}
	_, err := s.mutate(ctx, id, func(c *Card, ch *change) error {
		ch.comment(author, text)
		return nil
	})
	return err
}

// Complete marks a card done with its result. Agents may only complete
// cards assigned to them; the CLI (actor User) may complete any card.
func (s *Store) Complete(ctx context.Context, id, actor, result string) (*Card, error) {
	result = strings.TrimSpace(result)
	if result == "" {
		return nil, errors.New("a result is required: say what was done and give proof (paths, links, output)")
	}
	return s.mutate(ctx, id, func(c *Card, ch *change) error {
		if err := mayWork(c, actor); err != nil {
			return err
		}
		ch.event(actor, "result", firstLine(result))
		c.Status, c.Result = Done, result
		return nil
	})
}

// Block marks a card blocked with the reason as a comment. Like Complete,
// only the assignee (or the user) may do it.
func (s *Store) Block(ctx context.Context, id, actor, reason string) (*Card, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, errors.New("say why the card is blocked")
	}
	return s.mutate(ctx, id, func(c *Card, ch *change) error {
		if err := mayWork(c, actor); err != nil {
			return err
		}
		if c.Status == Blocked {
			return fmt.Errorf("card %s is already blocked", c.ID)
		}
		ch.comment(actor, "Blocked: "+reason)
		c.Status = Blocked
		return nil
	})
}

func mayWork(c *Card, actor string) error {
	if !c.Open() {
		return fmt.Errorf("card %s is %s", c.ID, c.Status)
	}
	if actor != User && c.Assignee != actor {
		return fmt.Errorf("card %s is assigned to %q, not you", c.ID, c.Assignee)
	}
	return nil
}

// Update changes who a card is for, its priority, or its status. Nil
// fields stay as they are.
type Update struct {
	Assignee *string
	Priority *int
	// Status may be Cancelled, or Todo to reopen a blocked card for another
	// attempt.
	Status Status
	Note   string // optional comment explaining the change
}

// Update applies u. Agents may only update cards they created or that are
// assigned to them. Reassigning a card that is blocked or in progress puts
// it back to todo (stopping the running attempt) so the new assignee gets it.
func (s *Store) Update(ctx context.Context, id, actor string, u Update) (*Card, error) {
	return s.mutate(ctx, id, func(c *Card, ch *change) error {
		was := c.Status
		if actor != User && c.CreatedBy != actor && c.Assignee != actor {
			return fmt.Errorf("card %s was neither created by nor assigned to you", c.ID)
		}
		if !c.Open() {
			return fmt.Errorf("card %s is %s", c.ID, c.Status)
		}
		if u.Note != "" {
			ch.comment(actor, u.Note)
		}
		if u.Priority != nil && *u.Priority != c.Priority {
			ch.event(actor, "priority", fmt.Sprintf("%d -> %d", c.Priority, *u.Priority))
			c.Priority = *u.Priority
		}
		if u.Assignee != nil && *u.Assignee != c.Assignee {
			ch.event(actor, "assigned", fmt.Sprintf("%s -> %s", orNone(c.Assignee), orNone(*u.Assignee)))
			c.Assignee = *u.Assignee
			if c.Status != Todo {
				c.Status = Todo
			}
		}
		switch u.Status {
		case "":
		case Cancelled:
			c.Status = Cancelled
		case Todo:
			if c.Status == Doing {
				return fmt.Errorf("card %s is in progress; cancel or reassign it instead", c.ID)
			}
			c.Status = Todo
		default:
			return fmt.Errorf("status %q: use board_complete or board_block for that, or cancelled/todo here", u.Status)
		}
		if was == Doing && c.Status != Doing {
			ch.cancel = c.Job
		}
		if len(ch.events) == 0 && len(ch.comments) == 0 && u.Status == "" {
			return errNoChange
		}
		return nil
	})
}

// Cancel takes a card off the board, stopping its run if one is going.
func (s *Store) Cancel(ctx context.Context, id, actor string) (*Card, error) {
	return s.Update(ctx, id, actor, Update{Status: Cancelled})
}

func orNone(s string) string {
	if s == "" {
		return "(nobody)"
	}
	return s
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}
