package jobs

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
)

// OwnerID identifies a worker as worker:<host>:<pid>:<random>, so a restarted
// process can recognise leases held by a dead process on the same host and
// reclaim them immediately instead of waiting for expiry.
func OwnerID() string {
	host, _ := os.Hostname()
	host = strings.ReplaceAll(host, ":", "_")
	return fmt.Sprintf("worker:%s:%d:%s", host, os.Getpid(), NewID("w")[2:])
}

func parseOwner(owner string) (host string, pid int, ok bool) {
	parts := strings.Split(owner, ":")
	if len(parts) != 4 || parts[0] != "worker" {
		return "", 0, false
	}
	pid, err := strconv.Atoi(parts[2])
	return parts[1], pid, err == nil
}

func processAlive(pid int) bool { return platform.Alive(pid) }

// ReleaseDead expires the leases of running jobs whose owner process on this
// host no longer exists. Several pools call it as they start, so each lease
// is expired only while the dead owner still holds it: a job that a live
// worker has taken in the meantime is left alone.
func (q *Queue) ReleaseDead(ctx context.Context) (int, error) {
	dead, err := q.deadLeases(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, l := range dead {
		ok, err := q.expireLease(ctx, l.id, l.owner)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

type deadLease struct{ id, owner string }

// deadLeases lists live leases whose owner is a process on this host that
// has gone.
func (q *Queue) deadLeases(ctx context.Context) ([]deadLease, error) {
	host, _ := os.Hostname()
	host = strings.ReplaceAll(host, ":", "_")
	rows, err := q.db.QueryContext(ctx, `SELECT id, lease_owner FROM jobs WHERE status = 'running' AND lease_until >= ?`,
		q.now().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var dead []deadLease
	for rows.Next() {
		var id, owner string
		if err := rows.Scan(&id, &owner); err != nil {
			return nil, err
		}
		if h, pid, ok := parseOwner(owner); ok && h == host && pid != os.Getpid() && !processAlive(pid) {
			dead = append(dead, deadLease{id, owner})
		}
	}
	return dead, rows.Err()
}

// expireLease ends owner's lease on the job, and reports whether it did. It
// does nothing when the job has finished or another worker holds it now.
func (q *Queue) expireLease(ctx context.Context, id, owner string) (bool, error) {
	res, err := q.db.ExecContext(ctx, `UPDATE jobs SET lease_until = 0
		WHERE id = ? AND status = 'running' AND lease_owner = ?`, id, owner)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
