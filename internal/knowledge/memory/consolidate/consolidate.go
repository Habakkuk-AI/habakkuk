// Package consolidate keeps agents' memory tidy in the background. After a
// conversation goes quiet, a review job reads what is new in it and has a
// model propose memory changes: new notes, updates, merges of duplicates and
// user profile edits. Once a night a second job merges near-duplicate notes
// and archives stale ones.
//
// Both are durable jobs with ids derived from what they cover, so a job is
// queued once however often the scheduler looks. A job first stores its
// planned changes in memory.db, then applies them one at a time, recording
// progress after each; a job resumed after a crash reuses the stored plan
// (no second model call) and continues at the first change not yet
// recorded. Every change is safe to apply twice, which covers a crash
// between applying one and recording it.
package consolidate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// Job kinds.
const (
	KindReview  = "memory_review"
	KindNightly = "memory_nightly"
)

// Sender is the model call. *llm.Router implements it.
type Sender interface {
	Send(ctx context.Context, chain []llm.Target, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error)
}

// Consolidator queues and runs memory review and nightly jobs.
type Consolidator struct {
	Mem      *memory.Store
	Sessions *session.Store
	Queue    *jobs.Queue
	Models   Sender
	// Chains maps each agent whose memory is reviewed to the models its
	// reviews use, cheapest first.
	Chains map[string][]llm.Target
	// Idle is how long a session must be quiet before it is reviewed.
	Idle time.Duration
	// Lookback bounds how old a quiet session may be and still get its
	// first review (default 3 days), so a fresh memory.db does not review
	// all history.
	Lookback time.Duration
	// NightlyAt is the nightly pass's offset from local midnight; Nightly
	// false turns the pass off.
	Nightly   bool
	NightlyAt time.Duration
	// StaleDays archives notes neither updated nor read for that long;
	// zero or less never does.
	StaleDays int
	Location  *time.Location
	// Untrusted reports sessions whose messages come from senders nobody
	// vetted (an email inbox): everything learned there is marked
	// untrusted and the profile is left alone.
	Untrusted func(ctx context.Context, sessionID string) bool
	Wake      func()
	Log       *slog.Logger
	Now       func() time.Time

	// crashAfter, in tests, stops a job after applying that many changes
	// as if the process died.
	crashAfter func(applied int) bool
}

var errCrash = errors.New("simulated crash")

func (c *Consolidator) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Consolidator) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.New(slog.DiscardHandler)
}

// SerialKey keeps one agent's memory jobs from running at the same time.
func SerialKey(agent string) string { return "memory:" + agent }

type reviewPayload struct {
	Session string `json:"session"`
	Through int    `json:"through"` // review messages before this seq
}

type nightlyPayload struct {
	Day string `json:"day"` // YYYY-MM-DD, local
}

// Tick queues a review for every quiet session with messages not yet
// reviewed, and tonight's pass for each agent once its time has come. Job
// ids make repeated ticks harmless.
func (c *Consolidator) Tick(ctx context.Context) error {
	now := c.now()
	lookback := c.Lookback
	if lookback <= 0 {
		lookback = 72 * time.Hour
	}
	quiet, err := c.Sessions.Quiet(ctx, now.Add(-lookback), now.Add(-c.Idle), 500)
	if err != nil {
		return err
	}
	queued := 0
	for _, a := range quiet {
		if _, ok := c.Chains[a.Agent]; !ok {
			continue
		}
		through, err := c.reviewed(ctx, a.ID)
		if err != nil {
			return err
		}
		if a.Messages <= through {
			continue
		}
		if _, err := c.Queue.Enqueue(ctx, jobs.NewJob{
			ID:   fmt.Sprintf("memrev_%s_%d", a.ID, a.Messages),
			Kind: KindReview, Agent: a.Agent, SerialKey: SerialKey(a.Agent),
			Payload: reviewPayload{Session: a.ID, Through: a.Messages},
		}); err != nil {
			return err
		}
		queued++
	}
	if c.Nightly {
		loc := c.Location
		if loc == nil {
			loc = time.Local
		}
		local := now.In(loc)
		midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
		if local.Sub(midnight) >= c.NightlyAt {
			day := local.Format("2006-01-02")
			for agent := range c.Chains {
				if _, err := c.Queue.Enqueue(ctx, jobs.NewJob{
					ID:   "memnight_" + agent + "_" + strings.ReplaceAll(day, "-", ""),
					Kind: KindNightly, Agent: agent, SerialKey: SerialKey(agent),
					Payload: nightlyPayload{Day: day},
				}); err != nil {
					return err
				}
				queued++
			}
		}
	}
	if queued > 0 && c.Wake != nil {
		c.Wake()
	}
	return nil
}

// reviewed is how far a session has been reviewed.
func (c *Consolidator) reviewed(ctx context.Context, sessionID string) (int, error) {
	var through int
	err := c.Mem.DB().QueryRowContext(ctx, `SELECT through FROM reviews WHERE session = ?`, sessionID).Scan(&through)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return through, err
}

// Op is one planned memory change, stored as JSON in review_runs.
type Op struct {
	Op        string   `json:"op"` // save | merge | profile | archive
	Title     string   `json:"title,omitempty"`
	Content   string   `json:"content,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	Slugs     []string `json:"slugs,omitempty"` // merge: the notes folded in; archive: the note
	Section   string   `json:"section,omitempty"`
	Mode      string   `json:"mode,omitempty"`
	Untrusted bool     `json:"untrusted,omitempty"`
	Session   string   `json:"session,omitempty"`
}

type run struct {
	Ops     []Op
	Next    int
	Done    bool
	Summary string
}

func (c *Consolidator) loadRun(ctx context.Context, job string) (*run, error) {
	var ops string
	var r run
	err := c.Mem.DB().QueryRowContext(ctx, `SELECT ops, next, done, summary FROM review_runs WHERE job = ?`, job).
		Scan(&ops, &r.Next, &r.Done, &r.Summary)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(ops), &r.Ops); err != nil {
		return nil, err
	}
	return &r, nil
}

// HandleReview is the jobs.Handler for KindReview.
func (c *Consolidator) HandleReview(ctx context.Context, j *jobs.Job, owner string) (string, error) {
	var p reviewPayload
	if err := j.Decode(&p); err != nil {
		return "", jobs.Permanent{Err: err}
	}
	return c.handle(ctx, j, owner, func() ([]Op, error) { return c.planReview(ctx, j.Agent, p) },
		func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO reviews (session, agent, through, at) VALUES (?, ?, ?, ?)
				ON CONFLICT (session) DO UPDATE SET through = max(through, excluded.through), at = excluded.at`,
				p.Session, j.Agent, p.Through, c.now().Unix())
			return err
		}, "session "+p.Session)
}

// HandleNightly is the jobs.Handler for KindNightly.
func (c *Consolidator) HandleNightly(ctx context.Context, j *jobs.Job, owner string) (string, error) {
	var p nightlyPayload
	if err := j.Decode(&p); err != nil {
		return "", jobs.Permanent{Err: err}
	}
	return c.handle(ctx, j, owner, func() ([]Op, error) { return c.planNightly(ctx, j.Agent) },
		func(tx *sql.Tx) error { return c.prune(ctx, tx) }, "nightly "+p.Day)
}

// handle plans (once), applies and finishes a job.
func (c *Consolidator) handle(ctx context.Context, j *jobs.Job, owner string, plan func() ([]Op, error),
	finish func(*sql.Tx) error, what string) (string, error) {
	if _, ok := c.Chains[j.Agent]; !ok {
		return "", jobs.Permanent{Err: fmt.Errorf("memory review is not on for agent %q", j.Agent)}
	}
	ctx = llm.WithCaller(ctx, llm.Caller{Agent: j.Agent, Session: "memory-review"})
	// Changes are recorded as the review's; pinned files are refused.
	ctx = history.WithActor(ctx, history.Actor{By: history.ByReview, Tool: j.Kind, Background: true, Note: "job " + j.ID})
	r, err := c.loadRun(ctx, j.ID)
	if err != nil {
		return "", err
	}
	if r != nil && r.Done {
		return r.Summary, nil
	}
	if r == nil {
		ops, err := plan()
		if be := new(llm.BudgetError); errors.As(err, &be) {
			// Out of budget: skip this review; the session is reviewed
			// again when it next goes idle.
			c.log().Info("memory review skipped by a spending limit", "job", j.ID, "agent", j.Agent, "limit", be.Key)
			return "skipped: " + be.Message, nil
		}
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(ops)
		if err != nil {
			return "", err
		}
		if err := c.Queue.Owns(ctx, j.ID, owner); err != nil {
			return "", err
		}
		if _, err := c.Mem.DB().ExecContext(ctx, `INSERT INTO review_runs (job, agent, kind, ops, at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (job) DO NOTHING`, j.ID, j.Agent, j.Kind, string(data), c.now().Unix()); err != nil {
			return "", err
		}
		if r, err = c.loadRun(ctx, j.ID); err != nil {
			return "", err
		}
	}
	for i := r.Next; i < len(r.Ops); i++ {
		if c.crashAfter != nil && c.crashAfter(i) {
			return "", errCrash
		}
		if err := c.Queue.Owns(ctx, j.ID, owner); err != nil {
			return "", err
		}
		if err := c.apply(ctx, j.Agent, r.Ops[i]); err != nil {
			if ctx.Err() != nil {
				return "", err
			}
			// A change the store refuses (a secret, an over-long profile)
			// will not succeed on a retry either: skip it.
			c.log().Warn("memory change skipped", "job", j.ID, "op", r.Ops[i].Op, "title", r.Ops[i].Title, "err", err)
		}
		if _, err := c.Mem.DB().ExecContext(ctx, `UPDATE review_runs SET next = ? WHERE job = ?`, i+1, j.ID); err != nil {
			return "", err
		}
	}
	summary := describe(r.Ops)
	if err := c.Queue.Owns(ctx, j.ID, owner); err != nil {
		return "", err
	}
	if err := c.writeLog(j.Agent, j.ID, what+": "+summary); err != nil {
		c.log().Warn("memory review log failed", "agent", j.Agent, "err", err)
	}
	tx, err := c.Mem.DB().BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE review_runs SET done = 1, summary = ? WHERE job = ?`, summary, j.ID); err != nil {
		return "", err
	}
	if err := finish(tx); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	c.log().Info("memory consolidated", "agent", j.Agent, "job", j.ID, "changes", len(r.Ops), "summary", summary)
	return summary, nil
}

// apply makes one change. Each is idempotent: saving replaces by title,
// forgetting a note that is gone is a no-op, and appending to the profile
// skips text the section already holds.
func (c *Consolidator) apply(ctx context.Context, agent string, op Op) error {
	if what := c.pinnedBy(ctx, agent, op); what != "" {
		c.log().Info("memory change skipped: pinned", "agent", agent, "op", op.Op, "pinned", what)
		return nil
	}
	a := history.ActorFrom(ctx)
	a.Session, a.Tainted = op.Session, op.Untrusted
	ctx = history.WithActor(ctx, a)
	switch op.Op {
	case "save", "merge":
		m, _, err := c.Mem.Put(ctx, agent, memory.Note{
			Title: op.Title, Content: op.Content, Tags: op.Tags,
			Source: memory.SourceReview, Session: op.Session, Untrusted: op.Untrusted,
		})
		if err != nil {
			return err
		}
		for _, slug := range op.Slugs {
			if slug != m.Slug {
				if err := c.forget(ctx, agent, slug); err != nil {
					return err
				}
			}
		}
		return nil
	case "profile":
		_, err := c.Mem.UpdateProfile(ctx, agent, op.Section, op.Content, op.Mode)
		return err
	case "archive":
		for _, slug := range op.Slugs {
			if err := c.forget(ctx, agent, slug); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("unknown change %q", op.Op)
}

// pinnedBy names the pinned note or profile a change would touch, or "".
// A merge touching any pinned note is skipped whole, so a pinned note is
// neither rewritten nor folded into another.
func (c *Consolidator) pinnedBy(ctx context.Context, agent string, op Op) string {
	switch op.Op {
	case "profile":
		if c.Mem.ProfilePinned(ctx, agent) {
			return "USER.md"
		}
		return ""
	case "save", "merge":
		if slug := memory.Slugify(op.Title); c.Mem.Pinned(ctx, agent, slug) {
			return slug
		}
	}
	for _, slug := range op.Slugs {
		if c.Mem.Pinned(ctx, agent, slug) {
			return slug
		}
	}
	return ""
}

func (c *Consolidator) forget(ctx context.Context, agent, slug string) error {
	if _, err := os.Lstat(filepath.Join(c.Mem.Dir(agent), slug+".md")); errors.Is(err, os.ErrNotExist) {
		return nil // already gone: applied before a crash, or removed by hand
	}
	_, err := c.Mem.Forget(ctx, agent, slug)
	return err
}

// describe summarizes a job's changes in one line for the log.
func describe(ops []Op) string {
	var parts []string
	for _, op := range ops {
		switch op.Op {
		case "save":
			parts = append(parts, fmt.Sprintf("saved %q", op.Title))
		case "merge":
			parts = append(parts, fmt.Sprintf("merged %s into %q", strings.Join(op.Slugs, ", "), op.Title))
		case "profile":
			parts = append(parts, fmt.Sprintf("profile %q (%s)", op.Section, op.Mode))
		case "archive":
			parts = append(parts, "archived "+strings.Join(op.Slugs, ", "))
		}
	}
	if len(parts) == 0 {
		return "nothing to change"
	}
	return strings.Join(parts, "; ")
}

const maxLog = 256 << 10

// writeLog appends one line per job to agents/<agent>/memory/.review.log,
// once: a job that finishes again after a crash finds its line there.
func (c *Consolidator) writeLog(agent, job, line string) error {
	path := filepath.Join(c.Mem.Dir(agent), ".review.log")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if strings.Contains(string(data), " "+job+" ") {
		return nil
	}
	if len(data) > maxLog {
		if err := os.Rename(path, path+".1"); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "%s %s %s\n", c.now().UTC().Format(time.RFC3339), job, strings.Join(strings.Fields(line), " ")); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// prune drops bookkeeping nobody needs any more: finished runs and review
// positions older than 30 days (their sessions are past the lookback), and
// cached vectors no note uses.
func (c *Consolidator) prune(ctx context.Context, tx *sql.Tx) error {
	cutoff := c.now().Add(-30 * 24 * time.Hour).Unix()
	if _, err := tx.ExecContext(ctx, `DELETE FROM review_runs WHERE done = 1 AND at < ?`, cutoff); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM reviews WHERE at < ?`, cutoff); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM embeddings WHERE used < ?
		AND NOT EXISTS (SELECT 1 FROM memories m WHERE m.chash = embeddings.hash)`, cutoff)
	return err
}
