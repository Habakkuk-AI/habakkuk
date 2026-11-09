package jobs

import (
	"context"
	"time"
)

// Listed is a job with the time it last changed, for status pages.
type Listed struct {
	Job
	UpdatedAt time.Time
}

// withTail scans the job columns followed by extra columns.
type withTail struct {
	r    scanner
	tail []any
}

func (w withTail) Scan(dst ...any) error { return w.r.Scan(append(dst, w.tail...)...) }

func (q *Queue) list(ctx context.Context, where string, args ...any) ([]Listed, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT `+jobColumns+`, updated_at FROM jobs `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Listed
	for rows.Next() {
		var updated int64
		j, err := scanJob(withTail{r: rows, tail: []any{&updated}})
		if err != nil {
			return nil, err
		}
		out = append(out, Listed{Job: *j, UpdatedAt: time.UnixMilli(updated)})
	}
	return out, rows.Err()
}

// Running returns the jobs that hold a lease now, oldest first.
func (q *Queue) Running(ctx context.Context, limit int) ([]Listed, error) {
	return q.list(ctx, `WHERE status = 'running' ORDER BY updated_at LIMIT ?`, max(limit, 1))
}

// Failures returns jobs that failed for good or recorded an error on a
// retried attempt since the cutoff, newest first.
func (q *Queue) Failures(ctx context.Context, since time.Time, limit int) ([]Listed, error) {
	return q.list(ctx, `WHERE (status = 'failed' OR last_error != '') AND updated_at >= ?
		ORDER BY updated_at DESC LIMIT ?`, since.UnixMilli(), max(limit, 1))
}
