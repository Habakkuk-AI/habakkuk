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
// host no longer exists.
func (q *Queue) ReleaseDead(ctx context.Context) (int, error) {
	host, _ := os.Hostname()
	host = strings.ReplaceAll(host, ":", "_")
	rows, err := q.db.QueryContext(ctx, `SELECT id, lease_owner FROM jobs WHERE status = 'running' AND lease_until >= ?`,
		q.now().UnixMilli())
	if err != nil {
		return 0, err
	}
	var dead []string
	for rows.Next() {
		var id, owner string
		if err := rows.Scan(&id, &owner); err != nil {
			rows.Close()
			return 0, err
		}
		if h, pid, ok := parseOwner(owner); ok && h == host && pid != os.Getpid() && !processAlive(pid) {
			dead = append(dead, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range dead {
		if _, err := q.db.ExecContext(ctx, `UPDATE jobs SET lease_until = 0 WHERE id = ? AND status = 'running'`, id); err != nil {
			return 0, err
		}
	}
	return len(dead), nil
}
