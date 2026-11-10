// Package schedule runs prompts and scripts on a timetable. Each due slot
// becomes a job whose ID is fixed by the schedule and the slot time, so a
// slot runs at most once however many processes are polling and however
// often they crash.
package schedule

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// Migrations for control.db; they follow the channel migrations.
var Migrations = []store.Migration{{Version: 3, SQL: `
CREATE TABLE schedules (
	id          TEXT PRIMARY KEY,
	agent       TEXT NOT NULL,
	name        TEXT NOT NULL,
	spec        TEXT NOT NULL,
	prompt      TEXT NOT NULL,
	deliver_to  TEXT NOT NULL DEFAULT '',
	enabled     INTEGER NOT NULL DEFAULT 1,
	next_run    INTEGER NOT NULL,
	last_run    INTEGER NOT NULL DEFAULT 0,
	last_job    TEXT NOT NULL DEFAULT '',
	last_status TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL
);
CREATE INDEX schedules_due ON schedules (enabled, next_run);
`}, {Version: 5, SQL: `
ALTER TABLE schedules ADD COLUMN command     TEXT NOT NULL DEFAULT '';
ALTER TABLE schedules ADD COLUMN timeout_s   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE schedules ADD COLUMN workdir     TEXT NOT NULL DEFAULT '';
ALTER TABLE schedules ADD COLUMN on_output   TEXT NOT NULL DEFAULT '';
ALTER TABLE schedules ADD COLUMN last_output TEXT NOT NULL DEFAULT '';
ALTER TABLE schedules ADD COLUMN fail_streak INTEGER NOT NULL DEFAULT 0;
`}}

// KindScheduled is the job that starts one scheduled run.
const KindScheduled = "scheduled"

// What a script schedule does with its output.
const (
	OutputDeliver = "deliver" // send stdout to the chat
	OutputAgent   = "agent"   // hand stdout to the agent with the prompt; send its reply
	OutputSilent  = "silent"  // keep stdout in last_output only
)

// Schedule is one recurring (or one-off) prompt or script.
type Schedule struct {
	ID        string
	Agent     string
	Name      string
	Spec      string          // cron ("0 9 * * 1-5"), "@every 2h", "@daily", or "at 2026-11-08 09:00"
	Prompt    string          // what the agent is asked each run
	DeliverTo json.RawMessage // reply address (a channel conversation), or empty
	// Command, when set, makes this a script schedule: the command runs
	// with /bin/sh and no model is involved unless OnOutput is "agent".
	// Only the CLI sets it; the model-facing tools cannot.
	Command    string
	Timeout    time.Duration // default 120s
	Workdir    string        // default the agent's workspace; relative paths are inside it
	OnOutput   string        // OutputDeliver (default), OutputAgent or OutputSilent
	Enabled    bool
	NextRun    time.Time
	LastRun    time.Time
	LastJob    string
	LastStatus string
	LastOutput string // a script's last output, capped
	FailStreak int    // consecutive failed script runs
	CreatedAt  time.Time
}

type Store struct {
	db  *sql.DB
	loc *time.Location
	now func() time.Time
}

func NewStore(db *sql.DB, loc *time.Location) *Store {
	if loc == nil {
		loc = time.Local
	}
	return &Store{db: db, loc: loc, now: time.Now}
}

var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Next returns the first run of spec strictly after t, or zero for a
// one-off that has passed.
func (s *Store) Next(spec string, t time.Time) (time.Time, error) {
	if at, ok := strings.CutPrefix(spec, "at "); ok {
		when, err := s.parseAt(at)
		if err != nil {
			return time.Time{}, err
		}
		if !when.After(t) {
			return time.Time{}, nil
		}
		return when, nil
	}
	if !strings.HasPrefix(spec, "CRON_TZ=") && !strings.HasPrefix(spec, "TZ=") && !strings.HasPrefix(spec, "@every") {
		spec = "CRON_TZ=" + s.loc.String() + " " + spec
	}
	sched, err := parser.Parse(spec)
	if err != nil {
		return time.Time{}, fmt.Errorf("schedule %q: %w", spec, err)
	}
	return sched.Next(t), nil
}

func (s *Store) parseAt(at string) (time.Time, error) {
	at = strings.TrimSpace(at)
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, at, s.loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("one-off time %q: use YYYY-MM-DD HH:MM", at)
}

func (s *Store) Create(ctx context.Context, sc Schedule) (*Schedule, error) {
	sc.Spec = strings.TrimSpace(sc.Spec)
	sc.Command = strings.TrimSpace(sc.Command)
	if err := validate(&sc); err != nil {
		return nil, err
	}
	next, err := s.Next(sc.Spec, s.now())
	if err != nil {
		return nil, err
	}
	if next.IsZero() {
		return nil, errors.New("that time has already passed")
	}
	sc.ID, sc.NextRun, sc.Enabled, sc.CreatedAt = jobs.NewID("sch"), next, true, s.now()
	_, err = s.db.ExecContext(ctx, `INSERT INTO schedules (id, agent, name, spec, prompt, deliver_to,
		command, timeout_s, workdir, on_output, next_run, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, sc.ID, sc.Agent, sc.Name, sc.Spec, sc.Prompt, string(sc.DeliverTo),
		sc.Command, int64(sc.Timeout/time.Second), sc.Workdir, sc.OnOutput, next.UnixMilli(), sc.CreatedAt.UnixMilli())
	return &sc, err
}

func validate(sc *Schedule) error {
	if sc.Name == "" || sc.Agent == "" {
		return errors.New("schedule needs an agent and a name")
	}
	if sc.Command == "" {
		if sc.Prompt == "" {
			return errors.New("schedule needs a prompt")
		}
		if sc.OnOutput != "" || sc.Workdir != "" || sc.Timeout != 0 {
			return errors.New("output mode, workdir and timeout only apply to a schedule that runs a command")
		}
		return nil
	}
	switch sc.OnOutput {
	case "":
		sc.OnOutput = OutputDeliver
	case OutputDeliver, OutputSilent:
	case OutputAgent:
		if sc.Prompt == "" {
			return errors.New(`on-output "agent" needs a prompt saying what to do with the output`)
		}
	default:
		return fmt.Errorf("on-output %q: want deliver, agent or silent", sc.OnOutput)
	}
	if sc.Timeout < 0 || (sc.Timeout > 0 && sc.Timeout < time.Second) {
		return errors.New("timeout must be at least 1s")
	}
	return nil
}

const columns = `id, agent, name, spec, prompt, deliver_to, command, timeout_s, workdir, on_output,
	enabled, next_run, last_run, last_job, last_status, last_output, fail_streak, created_at`

func scan(r interface{ Scan(...any) error }) (*Schedule, error) {
	var sc Schedule
	var to string
	var next, last, created, timeout int64
	if err := r.Scan(&sc.ID, &sc.Agent, &sc.Name, &sc.Spec, &sc.Prompt, &to, &sc.Command, &timeout, &sc.Workdir,
		&sc.OnOutput, &sc.Enabled, &next, &last, &sc.LastJob, &sc.LastStatus, &sc.LastOutput, &sc.FailStreak,
		&created); err != nil {
		return nil, err
	}
	if to != "" {
		sc.DeliverTo = json.RawMessage(to)
	}
	sc.Timeout = time.Duration(timeout) * time.Second
	sc.NextRun, sc.CreatedAt = time.UnixMilli(next), time.UnixMilli(created)
	if last > 0 {
		sc.LastRun = time.UnixMilli(last)
	}
	return &sc, nil
}

func (s *Store) Get(ctx context.Context, id string) (*Schedule, error) {
	sc, err := scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM schedules WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("no schedule %s", id)
	}
	return sc, err
}

// List returns schedules, all agents when agent is empty.
func (s *Store) List(ctx context.Context, agentName string) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM schedules
		WHERE ? = '' OR agent = ? ORDER BY next_run`, agentName, agentName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		sc, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sc)
	}
	return out, rows.Err()
}

func (s *Store) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no schedule %s", id)
	}
	return nil
}

func (s *Store) SetEnabled(ctx context.Context, id string, on bool) error {
	sc, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	next := sc.NextRun
	if on {
		// Resume from now rather than replaying slots missed while paused.
		if next, err = s.Next(sc.Spec, s.now()); err != nil {
			return err
		}
		if next.IsZero() {
			return errors.New("that one-off time has passed")
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE schedules SET enabled = ?, next_run = ? WHERE id = ?`, on, next.UnixMilli(), id)
	return err
}

// advance moves a schedule past slot, but only if no one else has already
// (compare-and-swap on next_run). A one-off is disabled after its slot.
func (s *Store) advance(ctx context.Context, sc *Schedule, slot, from time.Time, jobID, status string) (bool, error) {
	next, err := s.Next(sc.Spec, from)
	if err != nil {
		return false, err
	}
	enabled := !next.IsZero()
	if !enabled {
		next = from
	}
	res, err := s.db.ExecContext(ctx, `UPDATE schedules SET next_run = ?, enabled = ?, last_run = ?,
		last_job = CASE WHEN ? = '' THEN last_job ELSE ? END, last_status = ?
		WHERE id = ? AND next_run = ?`,
		next.UnixMilli(), enabled, s.now().UnixMilli(), jobID, jobID, status, sc.ID, slot.UnixMilli())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Scheduler turns due slots into jobs and runs them.
type Scheduler struct {
	Store  *Store
	Queue  *jobs.Queue
	Runner *agent.Runner
	Log    *slog.Logger
	Wake   func()
	Grace  time.Duration // a slot later than this is skipped, not run late; default 1h
	Every  time.Duration // poll interval; default 15s
	// Deliver queues text for a reply address; id makes it idempotent. Nil
	// (no channels) keeps script output in last_output only.
	Deliver func(ctx context.Context, id string, to json.RawMessage, text string) error
	// Secrets returns extra environment for scripts ($HABAKKUK_HOME/.env);
	// the process environment wins over it.
	Secrets func() map[string]string
}

type payload struct {
	Schedule string `json:"schedule"`
	Slot     int64  `json:"slot"` // unix ms
}

// Run polls for due schedules until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	every := s.Every
	if every == 0 {
		every = 15 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.Log.Error("schedule tick failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick records how finished runs ended, then enqueues every due slot once
// and returns how many it started.
func (s *Scheduler) Tick(ctx context.Context) (int, error) {
	if err := s.Settle(ctx); err != nil {
		return 0, err
	}
	grace := s.Grace
	if grace == 0 {
		grace = time.Hour
	}
	now := s.Store.now()
	rows, err := s.Store.db.QueryContext(ctx, `SELECT `+columns+` FROM schedules
		WHERE enabled = 1 AND next_run <= ?`, now.UnixMilli())
	if err != nil {
		return 0, err
	}
	var due []*Schedule
	for rows.Next() {
		sc, err := scan(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		due = append(due, sc)
	}
	rows.Close()

	started := 0
	for _, sc := range due {
		slot := sc.NextRun
		if now.Sub(slot) > grace {
			// Habakkuk was down past the slot: skip it rather than act late.
			if _, err := s.Store.advance(ctx, sc, slot, latestSlot(s.Store, sc.Spec, slot, now), "", "missed "+slot.Format(time.RFC3339)); err != nil {
				return started, err
			}
			s.Log.Warn("skipped a late schedule slot", "schedule", sc.ID, "slot", slot)
			continue
		}
		id := fmt.Sprintf("sch_%s_%d", strings.TrimPrefix(sc.ID, "sch_"), slot.UnixMilli())
		if sc.Command != "" {
			// Another poller may already have queued this slot; only an
			// earlier run counts as an overlap.
			busy, err := s.Queue.Busy(ctx, serialKey(sc.ID), id)
			if err != nil {
				return started, err
			}
			if busy {
				// Never two copies of a script: drop this slot, not queue it.
				if _, err := s.Store.advance(ctx, sc, slot, slot, "", "skipped: still running"); err != nil {
					return started, err
				}
				continue
			}
		}
		if err := s.enqueue(ctx, sc, id, slot); err != nil {
			return started, err
		}
		won, err := s.Store.advance(ctx, sc, slot, slot, id, statusStarted)
		if err != nil {
			return started, err
		}
		if won {
			started++
		}
	}
	if started > 0 && s.Wake != nil {
		s.Wake()
	}
	return started, nil
}

// statusStarted is a run's status from when its slot is queued until Settle
// records how it ended.
const statusStarted = "started"

// Settle records how started runs ended. A prompt run's job only queues a
// turn, so its outcome is the turn's; a script records its own, but a slot
// advanced after the script already finished is caught here too. Tick
// calls it; hk schedules list calls it before listing.
func (s *Scheduler) Settle(ctx context.Context) error {
	rows, err := s.Store.db.QueryContext(ctx, `SELECT id, last_job FROM schedules
		WHERE last_status = ? AND last_job != ''`, statusStarted)
	if err != nil {
		return err
	}
	type run struct{ id, job string }
	var runs []run
	for rows.Next() {
		var r run
		if err := rows.Scan(&r.id, &r.job); err != nil {
			rows.Close()
			return err
		}
		runs = append(runs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range runs {
		status, err := s.outcome(ctx, r.job)
		if err != nil || status == "" {
			continue // still running, or unreadable for now
		}
		if _, err := s.Store.db.ExecContext(ctx, `UPDATE schedules SET last_status = ?
			WHERE id = ? AND last_job = ? AND last_status = ?`, status, r.id, r.job, statusStarted); err != nil {
			return err
		}
	}
	return nil
}

// outcome says how a run's job ended, or "" while it is still going.
func (s *Scheduler) outcome(ctx context.Context, jobID string) (string, error) {
	j, err := s.Queue.Get(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return "finished", nil // pruned long ago
	}
	if err != nil {
		return "", err
	}
	switch j.Status {
	case jobs.Pending, jobs.Running:
		return "", nil
	case jobs.Failed:
		return "failed: " + oneLine(j.LastError, 200), nil
	case jobs.Canceled:
		return "canceled", nil
	}
	if turnID := j.Result; j.Kind == KindScheduled && strings.HasPrefix(turnID, "turn_") {
		return s.outcome(ctx, turnID)
	}
	if j.Kind != KindScheduled || j.Result == "" {
		return "done", nil // a turn: its result is the reply, not a status
	}
	return oneLine(j.Result, 200), nil // a script's own status, or a skip
}

// enqueue queues one run. Script runs share a serial key so two never
// overlap, and get one attempt: a script with side effects is not rerun
// after a crash.
func (s *Scheduler) enqueue(ctx context.Context, sc *Schedule, id string, slot time.Time) error {
	j := jobs.NewJob{ID: id, Kind: KindScheduled, Agent: sc.Agent, Priority: 5,
		Payload: payload{Schedule: sc.ID, Slot: slot.UnixMilli()}}
	if sc.Command != "" {
		j.SerialKey, j.MaxAttempts = serialKey(sc.ID), 1
	}
	_, err := s.Queue.Enqueue(ctx, j)
	return err
}

func serialKey(scheduleID string) string { return "sch:" + scheduleID }

// RunNow queues an extra run of a schedule outside its timetable and returns
// the job ID. The serving process runs it.
func (s *Scheduler) RunNow(ctx context.Context, id string) (string, error) {
	sc, err := s.Store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if sc.Command != "" {
		busy, err := s.Queue.Busy(ctx, serialKey(sc.ID), "")
		if err != nil {
			return "", err
		}
		if busy {
			return "", fmt.Errorf("schedule %s is still running", id)
		}
	}
	now := s.Store.now()
	jobID := fmt.Sprintf("sch_%s_now_%d", strings.TrimPrefix(sc.ID, "sch_"), now.UnixMilli())
	if err := s.enqueue(ctx, sc, jobID, now); err != nil {
		return "", err
	}
	if s.Wake != nil {
		s.Wake()
	}
	return jobID, nil
}

// latestSlot finds the last slot at or before now, so a skip lands on the
// next future slot instead of walking through every missed one.
func latestSlot(st *Store, spec string, from, now time.Time) time.Time {
	slot := from
	for i := 0; i < 100000; i++ {
		next, err := st.Next(spec, slot)
		if err != nil || next.IsZero() || next.After(now) {
			return slot
		}
		slot = next
	}
	return slot
}

// Handle is the jobs.Handler for KindScheduled: it opens a fresh session and
// queues the run as an ordinary turn, replying to the schedule's address.
func (s *Scheduler) Handle(ctx context.Context, j *jobs.Job, _ string) (string, error) {
	var p payload
	if err := j.Decode(&p); err != nil {
		return "", jobs.Permanent{Err: err}
	}
	sc, err := s.Store.Get(ctx, p.Schedule)
	if err != nil {
		return "", jobs.Permanent{Err: err} // deleted since
	}
	s.Runner.Fire(ctx, "schedule_run", sc.Name, map[string]any{"event": "schedule_run", "schedule": sc.ID,
		"name": sc.Name, "agent": sc.Agent, "spec": sc.Spec, "script": sc.Command != "", "job": j.ID})
	// What the run delivers itself (script output, notices) is labelled
	// for the agent that answers the chat it lands in.
	ctx = agent.WithPoster(ctx, agent.Poster{Agent: sc.Agent, What: fmt.Sprintf("scheduled task %q", sc.Name)})
	if sc.Command != "" {
		return s.handleScript(ctx, j, sc)
	}
	if be := s.Runner.BudgetBlocked(sc.Agent); be != nil {
		return s.skipForBudget(ctx, j, sc, be), nil
	}
	text := fmt.Sprintf("[Scheduled task %q (%s). Do it now and report the result; the user is not watching live.]\n%s",
		sc.Name, sc.Spec, sc.Prompt)
	return s.submit(ctx, j, sc, text)
}

// skipForBudget skips a run whose agent has used up a spending limit,
// without opening a session. The schedule's chat hears about it once per
// limit and period; later skips are only logged.
func (s *Scheduler) skipForBudget(ctx context.Context, j *jobs.Job, sc *Schedule, be *llm.BudgetError) string {
	msg := fmt.Sprintf("Scheduled task %q skipped. %s", sc.Name, be.Message)
	s.Log.Info("scheduled run skipped by a spending limit", "schedule", sc.ID, "agent", sc.Agent, "limit", be.Key)
	if s.Runner.BudgetNotice(be) && s.Deliver != nil && len(sc.DeliverTo) > 0 {
		if err := s.Deliver(ctx, "budget_"+j.ID, sc.DeliverTo, msg); err != nil {
			s.Log.Warn("budget notice not delivered", "schedule", sc.ID, "err", err)
		}
	}
	return "skipped: " + be.Message
}

// submit runs text as a fresh session's first turn, replying to the
// schedule's address. The turn ID derives from the job, so a rerun after a
// crash finds the turn it already queued.
func (s *Scheduler) submit(ctx context.Context, j *jobs.Job, sc *Schedule, text string) (string, error) {
	turnID := "turn_" + j.ID
	if existing, err := s.Queue.Get(ctx, turnID); err == nil && existing != nil {
		return turnID, nil // already queued before a crash
	}
	sess, err := s.Runner.NewSession(ctx, sc.Agent)
	if err != nil {
		return "", err
	}
	if _, err := s.Runner.SubmitTurn(ctx, agent.Turn{ID: turnID, Session: sess.ID, Text: text, Origin: sc.DeliverTo,
		Background: true, Source: agent.SourceSchedule, Task: fmt.Sprintf("scheduled task %q", sc.Name)}); err != nil {
		return "", err
	}
	if s.Wake != nil {
		s.Wake()
	}
	return turnID, nil
}
