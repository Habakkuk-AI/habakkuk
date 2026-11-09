package jobs

import (
	"context"
	"time"
)

// KindDepth is how many jobs of one kind wait or run.
type KindDepth struct {
	Kind    string
	Pending int
	Running int
}

// DepthByKind counts pending and running jobs per kind, sorted by kind.
func (q *Queue) DepthByKind(ctx context.Context) ([]KindDepth, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT kind, count(*) FILTER (WHERE status = 'pending'),
		count(*) FILTER (WHERE status = 'running') FROM jobs WHERE status IN ('pending','running')
		GROUP BY kind ORDER BY kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KindDepth
	for rows.Next() {
		var d KindDepth
		if err := rows.Scan(&d.Kind, &d.Pending, &d.Running); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Failure is a job whose last run failed, and when.
type Failure struct {
	*Job
	At time.Time
}

// RecentFailures returns jobs that failed for good, or are waiting to retry after
// an error, since the given time: newest first, at most limit.
func (q *Queue) RecentFailures(ctx context.Context, since time.Time, limit int) ([]Failure, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT `+jobColumns+`, updated_at FROM jobs
		WHERE last_error != '' AND status IN ('failed','pending') AND updated_at >= ?
		ORDER BY updated_at DESC LIMIT ?`, since.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Failure
	for rows.Next() {
		var updated int64
		j, err := scanJob(scanFunc(func(dest ...any) error { return rows.Scan(append(dest, &updated)...) }))
		if err != nil {
			return nil, err
		}
		out = append(out, Failure{Job: j, At: time.UnixMilli(updated)})
	}
	return out, rows.Err()
}

type scanFunc func(...any) error

func (f scanFunc) Scan(dest ...any) error { return f(dest...) }
