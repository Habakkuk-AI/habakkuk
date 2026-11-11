package cutover

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the schedule database
)

// OpenClaw, as it runs. Facts, from its public source:
//
//   - Its gateway is one Node process (`node .../dist/index.js gateway`),
//     which names itself openclaw-gateway. Cron and the WhatsApp channel
//     run inside it; there is no separate bridge process.
//   - Services: systemd user units openclaw-gateway.service (and
//     openclaw-gateway-<profile>.service, older openclaw.service,
//     openclaw-<profile>.service, clawdbot-gateway.service) and the node
//     host's openclaw-node[-<profile>].service, all Restart=always; launchd
//     agents ai.openclaw.gateway, ai.openclaw.<profile> and
//     ai.openclaw.node[.<profile>] (KeepAlive); Windows scheduled tasks
//     "OpenClaw Gateway", "OpenClaw Gateway (<profile>)" and "OpenClaw
//     Node" that run <home>\gateway.cmd, or a launcher of the same name
//     in the Startup folder.
//   - No pid file: lock files tmp/openclaw-<uid>/gateway.*.lock in the
//     home hold JSON with the gateway's "pid".
//   - Schedules: table cron_jobs in state/openclaw.sqlite (older homes:
//     cron/jobs.json). Whether a job is on is kept twice, in the enabled
//     column and in job_json's "enabled", and the gateway reads job_json.
//     It keeps them in memory while it runs, so they are changed only
//     while it is stopped.

var openclawSpec = &spec{
	services: openclawServices,
	role: func(p Process, dir, _ string) (string, bool) {
		args := p.Args()
		if len(args) == 0 {
			return "", false
		}
		switch p.Exe() {
		case "openclaw-gateway", "clawdbot-gateway":
			return RoleGateway, true
		case "openclaw":
			if hasWord(args, "gateway") && !slices.ContainsFunc(args, isShortGatewayCommand) {
				return RoleGateway, true
			}
			return "", false
		case "node", "nodejs", "bun":
			// node <install>/openclaw/dist/index.js gateway ...
			for _, a := range args[1:] {
				if strings.Contains(slashes(a), "/openclaw/") && hasWord(args, "gateway") &&
					!slices.ContainsFunc(args, isShortGatewayCommand) {
					return RoleGateway, true
				}
			}
		}
		return "", false
	},
	mayBe: func(p Process) bool {
		return strings.Contains(strings.ToLower(p.Cmdline), "openclaw") || strings.Contains(strings.ToLower(p.Cmdline), "clawdbot")
	},
	pidFiles: func(dir string) []pidFile {
		var out []pidFile
		for _, f := range globAll(filepath.Join(dir, "tmp"), filepath.Join("openclaw-*", "gateway.*.lock")) {
			out = append(out, pidFile{f, RoleGateway})
		}
		return out
	},
	jobs: openclawJobs,
	pause: func(store string, ids []string) ([]string, error) {
		if strings.HasSuffix(store, ".json") {
			return pauseJobs(store, ids, time.Now(), false)
		}
		return setOpenClawJobs(store, ids, false)
	},
	resume: func(store, id string) error {
		if strings.HasSuffix(store, ".json") {
			return resumeJob(store, id, false)
		}
		_, err := setOpenClawJobs(store, []string{id}, true)
		return err
	},
	waSessions: func(dir string) []string {
		var out []string
		for _, f := range globAll(filepath.Join(dir, "credentials", "whatsapp"), filepath.Join("*", "creds.json")) {
			out = append(out, filepath.Dir(f))
		}
		return out
	},
}

// isShortGatewayCommand: openclaw gateway status, stop and the like end
// at once; they are not the gateway.
func isShortGatewayCommand(a string) bool {
	switch a {
	case "status", "stop", "install", "uninstall", "restart", "start", "probe", "health", "call", "discover":
		return true
	}
	return false
}

func openclawServices(w where) []Service {
	defaultHome := filepath.Join(w.sys.UserHome(), ".openclaw")
	profile := ""
	if base := filepath.Base(w.dir); strings.HasPrefix(base, ".openclaw-") && under(filepath.Dir(w.dir), w.sys.UserHome()) {
		profile = strings.TrimPrefix(base, ".openclaw-")
	}
	isDefault := under(w.dir, defaultHome) && under(defaultHome, w.dir)
	// ours: a service file names this home; one that names none belongs
	// to the default home, or to the profile its name carries.
	ours := func(text, suffix string) bool {
		if mentionsDir(text, w.dir) {
			return true
		}
		if strings.Contains(text, "OPENCLAW_STATE_DIR") {
			return false
		}
		if suffix == "" {
			return isDefault
		}
		return suffix == profile
	}
	var out []Service
	switch w.sys.GOOS() {
	case "linux":
		for _, f := range globAll(w.userUnits(), "openclaw*.service", "clawdbot-gateway*.service") {
			name := strings.TrimSuffix(filepath.Base(f), ".service")
			suffix := ""
			for _, base := range []string{"openclaw-gateway", "openclaw-node", "clawdbot-gateway", "openclaw"} {
				if rest, ok := strings.CutPrefix(name, base); ok {
					suffix = strings.TrimPrefix(rest, "-")
					break
				}
			}
			if ours(readSmall(f), suffix) {
				out = append(out, Service{Manager: Systemd, Name: filepath.Base(f), File: f, Role: openclawRole(name)})
			}
		}
	case "darwin":
		for _, f := range globAll(filepath.Join(w.sys.UserHome(), "Library", "LaunchAgents"), "ai.openclaw.*.plist") {
			label := strings.TrimSuffix(filepath.Base(f), ".plist")
			rest := strings.TrimPrefix(label, "ai.openclaw.")
			if strings.HasPrefix(rest, "update.") {
				continue // a one-off update job, not the agent
			}
			suffix := ""
			switch {
			case rest == "gateway", rest == "node":
			case strings.HasPrefix(rest, "node."):
				suffix = strings.TrimPrefix(rest, "node.")
			default:
				suffix = rest
			}
			if ours(readSmall(f), suffix) {
				out = append(out, Service{Manager: Launchd, Name: label, File: f, Role: openclawRole(rest)})
			}
		}
	case "windows":
		if !fileExists(filepath.Join(w.dir, "gateway.cmd")) && !fileExists(filepath.Join(w.dir, "gateway.vbs")) {
			break
		}
		names := []string{"OpenClaw Gateway", "OpenClaw Node"}
		if profile != "" {
			names = []string{"OpenClaw Gateway (" + profile + ")", "OpenClaw Node (" + profile + ")"}
		}
		for _, n := range names {
			out = append(out, Service{Manager: Schtasks, Name: n, Role: openclawRole(n)})
			for _, ext := range []string{".cmd", ".vbs"} {
				if f := filepath.Join(w.startupDir(), n+ext); fileExists(f) {
					out = append(out, Service{Manager: Startup, Name: n + ext, File: f, Role: openclawRole(n)})
				}
			}
		}
	}
	return out
}

func openclawRole(name string) string {
	if strings.Contains(strings.ToLower(name), "node") {
		return "node host"
	}
	return RoleGateway
}

// openclawJobs lists the schedules that are on: the database's, else the
// older jobs file's.
func openclawJobs(dir string) ([]Job, error) {
	db := filepath.Join(dir, "state", "openclaw.sqlite")
	if fileExists(db) {
		return readOpenClawJobs(db)
	}
	return runnableJobs(filepath.Join(dir, "cron", "jobs.json"), "")
}

func openOpenClawDB(path string, write bool) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	if !write {
		q.Add("mode", "ro")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func readOpenClawJobs(path string) ([]Job, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := openOpenClawDB(path, false)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'cron_jobs'`).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT job_id, COALESCE(name, ''), COALESCE(agent_id, ''),
		enabled, COALESCE(json_extract(job_json, '$.enabled'), 1) FROM cron_jobs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var id, name, agent string
		var col, inJSON sql.NullInt64
		if err := rows.Scan(&id, &name, &agent, &col, &inJSON); err != nil {
			return out, err
		}
		// On when either says on: that is the one the gateway may act on.
		if col.Int64 == 0 && inJSON.Int64 == 0 {
			continue
		}
		if name == "" {
			name = id
		}
		out = append(out, Job{ID: id, Name: name, Agent: agent, Store: path})
	}
	return out, rows.Err()
}

// setOpenClawJobs turns jobs on or off in both places OpenClaw keeps it.
// It returns the ids it changed.
func setOpenClawJobs(path string, ids []string, on bool) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !fileExists(path) {
		return nil, fmt.Errorf("%s: %w", path, os.ErrNotExist)
	}
	db, err := openOpenClawDB(path, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	val, want := 0, "false"
	if on {
		val, want = 1, "true"
	}
	var done []string
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `UPDATE cron_jobs SET enabled = ?, job_json = json_set(job_json, '$.enabled', json(?))
			WHERE job_id = ? AND (enabled != ? OR COALESCE(json_extract(job_json, '$.enabled'), 1) != ?)`, val, want, id, val, val)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			done = append(done, id)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if on && len(done) == 0 && len(ids) > 0 {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM cron_jobs WHERE job_id = ?`, ids[0]).Scan(&n); err == nil && n == 0 {
			return nil, errors.New("it is no longer there")
		}
	}
	return done, nil
}
