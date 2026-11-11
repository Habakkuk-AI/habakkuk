package cutover

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	home:     openclawHomeOf,
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
	store: func(path string) jobStore {
		if strings.HasSuffix(path, ".json") {
			return jsonStore(path, []string{"enabled"}, func(time.Time) fields { return fields{"enabled": rawOf(false)} })
		}
		return jobStore{keys: []string{"column", "json"},
			paused: func(time.Time) fields { return fields{"column": rawOf(0), "json": rawOf(false)} },
			read:   func(ids []string) (map[string]fields, error) { return readOpenClawFields(path, ids) },
			write:  func(c map[string][2]fields) ([]string, error) { return writeOpenClawFields(path, c) },
		}
	},
	waSessions: func(dir string) []string {
		var out []string
		for _, f := range globAll(filepath.Join(dir, "credentials", "whatsapp"), filepath.Join("*", "creds.json")) {
			out = append(out, filepath.Dir(f))
		}
		return out
	},
}

// openclawHomeOf: OPENCLAW_STATE_DIR names the home; OPENCLAW_PROFILE or
// --profile NAME (--dev is the dev profile) names ~/.openclaw-NAME;
// neither means the default home.
func openclawHomeOf(w where, p Process, env map[string]string) (verdict, string) {
	uh := w.sys.UserHome()
	if d, ok := env["OPENCLAW_STATE_DIR"]; ok && d != "" {
		if under(expandTilde(d, uh), w.dir) {
			return ours, "its OPENCLAW_STATE_DIR is " + d
		}
		return theirs, "its OPENCLAW_STATE_DIR is " + d
	}
	prof := env["OPENCLAW_PROFILE"]
	if f := wordAfter(p.Args(), "--profile"); f != "" {
		prof = f
	} else if hasWord(p.Args(), "--dev") {
		prof = "dev"
	}
	home := filepath.Join(uh, ".openclaw")
	if prof != "" && prof != "default" {
		home += "-" + prof
	}
	switch {
	case prof == "" && env == nil:
		return unknown, ""
	case isDefaultHome(w.dir, home):
		return ours, "it uses " + home
	}
	return theirs, "it uses " + home
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

// readOpenClawFields reads where each job keeps whether it is on: the
// enabled column ("column") and job_json's "enabled" ("json", missing
// when job_json has none).
func readOpenClawFields(path string, ids []string) (map[string]fields, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := openOpenClawDB(path, false)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	out := map[string]fields{}
	for _, id := range ids {
		f, err := openClawFields(ctx, db, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[id] = f
	}
	return out, nil
}

type queryer interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func openClawFields(ctx context.Context, db queryer, id string) (fields, error) {
	var col int64
	var typ sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT enabled, json_type(job_json, '$.enabled') FROM cron_jobs WHERE job_id = ? LIMIT 1`, id).Scan(&col, &typ); err != nil {
		return nil, err
	}
	f := fields{"column": rawOf(col)}
	switch typ.String {
	case "true":
		f["json"] = rawOf(true)
	case "false":
		f["json"] = rawOf(false)
	case "":
	default:
		var v sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT json_extract(job_json, '$.enabled') FROM cron_jobs WHERE job_id = ? LIMIT 1`, id).Scan(&v); err != nil {
			return nil, err
		}
		f["json"] = rawOf(v.String)
	}
	return f, nil
}

// writeOpenClawFields moves each job from one state to another in both
// places, only where it still holds the first.
func writeOpenClawFields(path string, changes map[string][2]fields) ([]string, error) {
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
	keys := []string{"column", "json"}
	var done []string
	for _, id := range slices.Sorted(maps.Keys(changes)) {
		c := changes[id]
		have, err := openClawFields(ctx, tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !same(have, c[0], keys) {
			continue
		}
		var col int64
		if err := json.Unmarshal(c[1]["column"], &col); err != nil {
			return nil, fmt.Errorf("job %s: %w", id, err)
		}
		q := `UPDATE cron_jobs SET enabled = ?, job_json = json_remove(job_json, '$.enabled') WHERE job_id = ?`
		args := []any{col, id}
		if v, ok := c[1]["json"]; ok {
			q = `UPDATE cron_jobs SET enabled = ?, job_json = json_set(job_json, '$.enabled', json(?)) WHERE job_id = ?`
			args = []any{col, string(v), id}
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return nil, err
		}
		done = append(done, id)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return done, nil
}
