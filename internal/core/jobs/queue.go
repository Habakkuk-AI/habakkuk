// Package jobs is Habakkuk's durable work queue. Every unit of work (a model
// turn, a cron run, a delegation) is a job that a worker claims with a lease
// and heartbeats while it runs. If the process dies, the lease expires and
// another worker picks the job up.
package jobs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// Migrations for the jobs table in control.db.
var Migrations = []store.Migration{{Version: 1, SQL: `
CREATE TABLE jobs (
	id           TEXT PRIMARY KEY,
	kind         TEXT NOT NULL,
	agent        TEXT NOT NULL,
	serial_key   TEXT NOT NULL DEFAULT '',
	payload      TEXT NOT NULL DEFAULT '{}',
	status       TEXT NOT NULL CHECK (status IN ('pending','running','done','failed','canceled')),
	priority     INTEGER NOT NULL DEFAULT 0,
	attempts     INTEGER NOT NULL DEFAULT 0,
	max_attempts INTEGER NOT NULL DEFAULT 3,
	run_at       INTEGER NOT NULL,
	lease_owner  TEXT NOT NULL DEFAULT '',
	lease_until  INTEGER NOT NULL DEFAULT 0,
	last_error   TEXT NOT NULL DEFAULT '',
	result       TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL
);
CREATE INDEX jobs_ready ON jobs (status, run_at);
CREATE INDEX jobs_serial ON jobs (serial_key, status);
`}}

type Status string

const (
	Pending  Status = "pending"
	Running  Status = "running"
	Done     Status = "done"
	Failed   Status = "failed"
	Canceled Status = "canceled"
)

// ErrLeaseLost means another worker now owns the job; the caller must stop
// writing on its behalf.
var ErrLeaseLost = errors.New("job lease lost")

type Job struct {
	ID          string
	Kind        string
	Agent       string
	SerialKey   string
	Payload     json.RawMessage
	Status      Status
	Priority    int
	Attempts    int
	MaxAttempts int
	RunAt       time.Time
	LeaseOwner  string
	LeaseUntil  time.Time
	LastError   string
	Result      string
	CreatedAt   time.Time
}

type NewJob struct {
	ID          string // optional; a duplicate ID is ignored, which makes enqueueing idempotent
	Kind        string
	Agent       string
	SerialKey   string // jobs sharing a key never run concurrently
	Payload     any
	Priority    int // higher runs first; interactive turns outrank background work
	MaxAttempts int
	RunAt       time.Time
}

type Queue struct {
	db  *sql.DB
	now func() time.Time
}

func NewQueue(db *sql.DB) *Queue {
	return &Queue{db: db, now: time.Now}
}

func NewID(prefix string) string {
	var b [8]byte
	rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

func (q *Queue) Enqueue(ctx context.Context, n NewJob) (string, error) {
	payload, err := json.Marshal(n.Payload)
	if err != nil {
		return "", err
	}
	if n.MaxAttempts <= 0 {
		n.MaxAttempts = 3
	}
	now := q.now()
	if n.RunAt.IsZero() {
		n.RunAt = now
	}
	id := n.ID
	if id == "" {
		id = NewID("job")
	}
	_, err = q.db.ExecContext(ctx, `INSERT INTO jobs
		(id, kind, agent, serial_key, payload, status, priority, max_attempts, run_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		id, n.Kind, n.Agent, n.SerialKey, string(payload), n.Priority, n.MaxAttempts,
		n.RunAt.UnixMilli(), now.UnixMilli(), now.UnixMilli())
	return id, err
}

// Claim atomically takes the next runnable job of the given kinds: a pending
// job whose time has come, or a running job whose lease expired (its worker
// died). A job is skipped while another job with the same serial key holds a
// live lease. Returns (nil, nil) when nothing is runnable.
func (q *Queue) Claim(ctx context.Context, owner string, lease time.Duration, kinds ...string) (*Job, error) {
	if len(kinds) == 0 {
		return nil, errors.New("claim: no kinds")
	}
	now := q.now().UnixMilli()
	if err := q.failExhausted(ctx, now); err != nil {
		return nil, err
	}
	args := []any{owner, now + lease.Milliseconds(), now}
	kindIn := ""
	for i, k := range kinds {
		if i > 0 {
			kindIn += ","
		}
		kindIn += "?"
		args = append(args, k)
	}
	args = append(args, now, now, now)
	row := q.db.QueryRowContext(ctx, `
		UPDATE jobs SET status = 'running', lease_owner = ?, lease_until = ?,
			attempts = attempts + 1, updated_at = ?
		WHERE id = (
			SELECT j.id FROM jobs j
			WHERE j.kind IN (`+kindIn+`)
			  AND ((j.status = 'pending' AND j.run_at <= ?) OR (j.status = 'running' AND j.lease_until < ?))
			  AND j.attempts < j.max_attempts
			  AND (j.serial_key = '' OR NOT EXISTS (
				SELECT 1 FROM jobs o WHERE o.serial_key = j.serial_key AND o.id != j.id
				  AND o.status = 'running' AND o.lease_until >= ?))
			ORDER BY j.priority DESC, j.run_at, j.created_at
			LIMIT 1)
		RETURNING `+jobColumns, args...)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// failExhausted marks jobs that expired on their final attempt as failed so
// they stop blocking their serial key.
func (q *Queue) failExhausted(ctx context.Context, now int64) error {
	_, err := q.db.ExecContext(ctx, `UPDATE jobs SET status = 'failed',
		last_error = CASE WHEN last_error = '' THEN 'lease expired on final attempt' ELSE last_error END,
		lease_owner = '', updated_at = ?
		WHERE status = 'running' AND lease_until < ? AND attempts >= max_attempts`, now, now)
	return err
}

// Heartbeat extends the lease. It fails with ErrLeaseLost if the job was
// reclaimed by another worker or finished.
func (q *Queue) Heartbeat(ctx context.Context, id, owner string, lease time.Duration) error {
	now := q.now().UnixMilli()
	res, err := q.db.ExecContext(ctx, `UPDATE jobs SET lease_until = ?, updated_at = ?
		WHERE id = ? AND lease_owner = ? AND status = 'running'`, now+lease.Milliseconds(), now, id, owner)
	return leaseResult(res, err)
}

// Owns reports whether owner still holds a live lease on the job.
func (q *Queue) Owns(ctx context.Context, id, owner string) error {
	var n int
	err := q.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id = ? AND lease_owner = ?
		AND status = 'running' AND lease_until >= ?`, id, owner, q.now().UnixMilli()).Scan(&n)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLeaseLost
	}
	return nil
}

func (q *Queue) Complete(ctx context.Context, id, owner, result string) error {
	res, err := q.db.ExecContext(ctx, `UPDATE jobs SET status = 'done', result = ?, lease_owner = '',
		updated_at = ? WHERE id = ? AND lease_owner = ? AND status = 'running'`,
		result, q.now().UnixMilli(), id, owner)
	return leaseResult(res, err)
}

// Fail records an error. If attempts remain and retry is true the job goes
// back to pending after backoff; otherwise it is failed permanently.
func (q *Queue) Fail(ctx context.Context, id, owner string, cause error, retry bool, backoff time.Duration) error {
	now := q.now()
	res, err := q.db.ExecContext(ctx, `UPDATE jobs SET
		status = CASE WHEN ? AND attempts < max_attempts THEN 'pending' ELSE 'failed' END,
		run_at = ?, last_error = ?, lease_owner = '', lease_until = 0, updated_at = ?
		WHERE id = ? AND lease_owner = ? AND status = 'running'`,
		retry, now.Add(backoff).UnixMilli(), cause.Error(), now.UnixMilli(), id, owner)
	return leaseResult(res, err)
}

// Release hands a running job back without holding it against the job: it
// is pending again at its old place in line, and the run that was cut short
// does not count as an attempt. A worker calls it when the process is asked
// to stop (SIGTERM, a deploy), so the job resumes as soon as a worker starts
// and a planned restart never uses up its retries. A crash never gets here:
// that lease expires, and the next claim counts as another attempt.
func (q *Queue) Release(ctx context.Context, id, owner string) error {
	res, err := q.db.ExecContext(ctx, `UPDATE jobs SET status = 'pending', lease_owner = '', lease_until = 0,
		attempts = MAX(attempts - 1, 0), updated_at = ?
		WHERE id = ? AND lease_owner = ? AND status = 'running'`, q.now().UnixMilli(), id, owner)
	return leaseResult(res, err)
}

func (q *Queue) Cancel(ctx context.Context, id string) error {
	_, err := q.db.ExecContext(ctx, `UPDATE jobs SET status = 'canceled', lease_owner = '', updated_at = ?
		WHERE id = ? AND status IN ('pending','running')`, q.now().UnixMilli(), id)
	return err
}

// CancelSerial cancels every pending or running job with the serial key. A
// running job's worker notices on its next heartbeat and stops.
func (q *Queue) CancelSerial(ctx context.Context, key string) (int64, error) {
	res, err := q.db.ExecContext(ctx, `UPDATE jobs SET status = 'canceled', lease_owner = '', updated_at = ?
		WHERE serial_key = ? AND status IN ('pending','running')`, q.now().UnixMilli(), key)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Busy reports whether a job with the serial key, other than except, is
// pending or running.
func (q *Queue) Busy(ctx context.Context, key, except string) (bool, error) {
	var n int
	err := q.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE serial_key = ? AND id != ?
		AND status IN ('pending','running')`, key, except).Scan(&n)
	return n > 0, err
}

// Active lists the pending and running jobs of a kind, oldest first.
func (q *Queue) Active(ctx context.Context, kind string) ([]*Job, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE kind = ?
		AND status IN ('pending','running') ORDER BY created_at, id`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (q *Queue) Get(ctx context.Context, id string) (*Job, error) {
	return scanJob(q.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id))
}

// Depth counts jobs waiting to run and jobs running now.
func (q *Queue) Depth(ctx context.Context) (pending, running int, err error) {
	err = q.db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE status = 'pending'),
		count(*) FILTER (WHERE status = 'running') FROM jobs WHERE status IN ('pending','running')`).Scan(&pending, &running)
	return pending, running, err
}

// Prune deletes finished jobs older than the cutoff.
func (q *Queue) Prune(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := q.db.ExecContext(ctx, `DELETE FROM jobs WHERE status IN ('done','failed','canceled')
		AND updated_at < ?`, olderThan.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func leaseResult(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLeaseLost
	}
	return nil
}

const jobColumns = `id, kind, agent, serial_key, payload, status, priority, attempts, max_attempts,
	run_at, lease_owner, lease_until, last_error, result, created_at`

type scanner interface{ Scan(...any) error }

func scanJob(r scanner) (*Job, error) {
	var j Job
	var payload string
	var runAt, leaseUntil, created int64
	err := r.Scan(&j.ID, &j.Kind, &j.Agent, &j.SerialKey, &payload, &j.Status, &j.Priority, &j.Attempts,
		&j.MaxAttempts, &runAt, &j.LeaseOwner, &leaseUntil, &j.LastError, &j.Result, &created)
	if err != nil {
		return nil, err
	}
	j.Payload = json.RawMessage(payload)
	j.RunAt = time.UnixMilli(runAt)
	j.LeaseUntil = time.UnixMilli(leaseUntil)
	j.CreatedAt = time.UnixMilli(created)
	return &j, nil
}

func (j *Job) Decode(v any) error {
	if err := json.Unmarshal(j.Payload, v); err != nil {
		return fmt.Errorf("job %s payload: %w", j.ID, err)
	}
	return nil
}
