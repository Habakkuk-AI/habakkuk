package cutover

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/app"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/migrate"
	"github.com/habakkuk-ai/habakkuk/internal/work/schedule"
)

func noEnv(string) string { return "" }

// fixture copies a source home from ../testdata into a fresh user home
// under its default folder name, the way it sits on a real computer.
func fixture(t *testing.T, from string) (userHome, dir string) {
	t.Helper()
	userHome = t.TempDir()
	dir = filepath.Join(userHome, "."+from)
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("..", "testdata", from))); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == "dotenv" {
			return os.Rename(p, filepath.Join(filepath.Dir(p), ".env"))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return userHome, dir
}

// imported runs hk migrate's import into a fresh Habakkuk home, so its
// schedules are there, paused, under the names the import gives them.
func imported(t *testing.T, from, userHome string) (home string, found *migrate.Found) {
	t.Helper()
	home = t.TempDir()
	o := migrate.Options{From: from, Getenv: noEnv, UserHome: userHome, History: migrate.HistorySkip, Secrets: "copy"}
	f, err := migrate.Read(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	plan, err := migrate.NewPlan(f, home, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Apply(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	return home, f
}

func habakkukSchedules(t *testing.T, home string) map[string]bool {
	t.Helper()
	ctx := context.Background()
	db, err := app.OpenControl(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	list, err := schedule.NewStore(db, nil).List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, s := range list {
		out[s.Agent+"/"+s.Name] = s.Enabled
	}
	return out
}

// hermesJobs reads the fixture's jobs file: id -> the job.
func hermesJobs(t *testing.T, dir string) map[string]map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "cron", "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ Jobs []map[string]any }
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, j := range doc.Jobs {
		out[j["id"].(string)] = j
	}
	return out
}

// A Hermes Agent install like the one that ran twice: a systemd gateway,
// a board worker that outlived it, the WhatsApp bridge, and the jobs.
func hermesRunning(t *testing.T) (*fakeSystem, string, string) {
	t.Helper()
	userHome, dir := fixture(t, migrate.Hermes)
	sys := newFake(userHome)
	unit := filepath.Join(userHome, ".config", "systemd", "user", "hermes-gateway.service")
	if err := os.MkdirAll(filepath.Dir(unit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("[Service]\nExecStart=/opt/hermes/venv/bin/python -m hermes_cli.main gateway run\nRestart=always\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sys.service(Systemd, "hermes-gateway.service", true, true,
		Process{PID: 100, PPID: 1, Cmdline: "/opt/hermes/venv/bin/python -m hermes_cli.main gateway run"},
		Process{PID: 300, PPID: 100, Cmdline: "node /opt/hermes/scripts/whatsapp-bridge/bridge.js --port 3000 --session " + filepath.Join(dir, "platforms", "whatsapp", "session")})
	sys.run(Process{PID: 200, PPID: 1, Cmdline: "/opt/hermes/venv/bin/python -m hermes_cli.kanban_worker_client"})
	// Not the agent: a short command, an editor on its files, a shell.
	sys.run(Process{PID: 400, PPID: 1, Cmdline: "/usr/local/bin/hermes gateway status"})
	sys.run(Process{PID: 500, PPID: 1, Cmdline: "vim " + filepath.Join(dir, "SOUL.md")})
	sys.run(Process{PID: 600, PPID: 1, Cmdline: "-zsh"})
	return sys, userHome, dir
}

func TestDetectHermesOnlyReads(t *testing.T) {
	sys, _, dir := hermesRunning(t)
	before, _ := os.ReadFile(filepath.Join(dir, "cron", "jobs.json"))
	st, err := Detect(context.Background(), sys, Options{From: migrate.Hermes, Getenv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if st.Dir != dir {
		t.Errorf("dir = %s, want %s", st.Dir, dir)
	}
	// The fixture has four jobs on (one is a one-off long past, which
	// Hermes Agent keeps on) and one turned off.
	want := "Hermes Agent is still running: gateway (systemd hermes-gateway), 1 worker, 1 channel bridge, 4 active schedules"
	if got := st.Summary(); got != want {
		t.Errorf("summary:\n got %s\nwant %s", got, want)
	}
	var pids []int
	for _, p := range st.Processes {
		pids = append(pids, p.PID)
	}
	if !slices.Equal(pids, []int{100, 200, 300}) {
		t.Errorf("processes = %v, want the gateway, worker and bridge only", pids)
	}
	if !st.SchedulerRunning() {
		t.Error("the gateway runs the scheduler")
	}
	if c := sys.changed(); len(c) > 0 {
		t.Errorf("detect changed things: %v", c)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "cron", "jobs.json"))
	if string(before) != string(after) {
		t.Error("detect changed jobs.json")
	}
}

func TestFinishThenRollbackHermes(t *testing.T) {
	ctx := context.Background()
	sys, userHome, dir := hermesRunning(t)
	home, found := imported(t, migrate.Hermes, userHome)
	jobsBefore := hermesJobs(t, dir)
	// A schedule of your own, paused, that the old agent never had.
	db, err := app.OpenControl(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	st := schedule.NewStore(db, nil)
	mine, err := st.Create(ctx, schedule.Schedule{Agent: "habakkuk", Name: "My own", Spec: "@daily", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetEnabled(ctx, mine.ID, false); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before := habakkukSchedules(t, home)
	for name, on := range before {
		if on {
			t.Fatalf("%s is on before finish", name)
		}
	}

	var said []string
	res, err := Finish(ctx, sys, FinishOptions{Options: Options{From: migrate.Hermes, Getenv: noEnv, Now: fixedNow},
		Home: home, Found: found, Say: func(f string, a ...any) { said = append(said, f) }})
	if err != nil {
		t.Fatalf("finish: %v (left %v)", err, res.Left)
	}
	if len(res.Left) > 0 {
		t.Errorf("left undone: %v", res.Left)
	}
	if s := sys.services["hermes-gateway.service"]; s.active || s.enabled {
		t.Errorf("the gateway unit is still active=%v enabled=%v", s.active, s.enabled)
	}
	if len(sys.procs) != 3 { // the status command, the editor and the shell
		t.Errorf("still running: %v", sys.procs)
	}
	if !slices.Contains(sys.stopped, 200) {
		t.Errorf("the worker was not stopped: %v", sys.stopped)
	}
	jobs := hermesJobs(t, dir)
	for id, j := range jobs {
		if j["enabled"] != false {
			t.Errorf("job %s is still on: %v", id, j)
		}
	}
	if jobs["a1b2c3d4e5f6"]["paused_reason"] != pauseReason || jobs["a1b2c3d4e5f6"]["state"] != "paused" {
		t.Errorf("job not marked paused the way the old agent marks it: %v", jobs["a1b2c3d4e5f6"])
	}
	if _, ok := jobs["d4e5f6a1b2c3"]["paused_reason"]; ok {
		t.Error("a job that was already off was touched")
	}
	if !fileExists(filepath.Join(dir, "cron", "jobs.json.before-habakkuk")) {
		t.Error("no copy of jobs.json before the change")
	}
	for _, h := range []string{dir, filepath.Join(dir, "profiles", "coder")} {
		if !fileExists(filepath.Join(h, "ESTOP")) {
			t.Errorf("the old agent's pause switch is not set in %s", h)
		}
	}
	if fileExists(filepath.Join(dir, "profiles", "stray", "ESTOP")) {
		t.Error("a folder that is not a profile got a pause switch")
	}
	after := habakkukSchedules(t, home)
	for _, name := range []string{"habakkuk/Morning brief", "habakkuk/Inbox sweep", "habakkuk/Digest script"} {
		if !after[name] {
			t.Errorf("%s was not resumed: %v", name, after)
		}
	}
	if after["habakkuk/My own"] {
		t.Error("a schedule that is not from the old agent was resumed")
	}
	rec, err := LoadRecord(home, migrate.Hermes)
	if err != nil || rec == nil || rec.Finished.IsZero() {
		t.Fatalf("record = %+v, %v", rec, err)
	}

	// Finish again: nothing left to do, nothing changes.
	n := len(rec.Changes)
	if _, err := Finish(ctx, sys, FinishOptions{Options: Options{From: migrate.Hermes, Getenv: noEnv}, Home: home, Found: found}); err != nil {
		t.Fatal(err)
	}
	if rec, _ = LoadRecord(home, migrate.Hermes); len(rec.Changes) != n {
		t.Errorf("a second finish recorded %d more changes", len(rec.Changes)-n)
	}

	rb, err := Rollback(ctx, sys, home, migrate.Hermes, fixedNow)
	if err != nil {
		t.Fatalf("rollback: %v (left %v)", err, rb.Left)
	}
	if s := sys.services["hermes-gateway.service"]; !s.active || !s.enabled {
		t.Errorf("the gateway unit is not back: active=%v enabled=%v", s.active, s.enabled)
	}
	if !slices.ContainsFunc(sys.procs, func(p Process) bool { return p.PID == 100 }) {
		t.Error("the gateway did not come back with its unit")
	}
	if len(rb.Left) > 0 {
		t.Errorf("rollback left steps: %v", rb.Left)
	}
	if !slices.ContainsFunc(rb.Gone, func(l string) bool { return strings.Contains(l, "kanban_worker_client") }) {
		t.Errorf("rollback should name the worker it can't start again: %v", rb.Gone)
	}
	// Each job exactly as it was before finish: no field added.
	if got := hermesJobs(t, dir); !reflect.DeepEqual(got, jobsBefore) {
		t.Errorf("jobs after rollback differ from before:\n got %v\nwant %v", got, jobsBefore)
	}
	for _, h := range []string{dir, filepath.Join(dir, "profiles", "coder")} {
		if fileExists(filepath.Join(h, "ESTOP")) {
			t.Errorf("the pause switch in %s is still set", h)
		}
	}
	for name, on := range habakkukSchedules(t, home) {
		if on {
			t.Errorf("%s is still on in Habakkuk after rollback", name)
		}
	}
	// Nothing left: the record is retired, and a second rollback has
	// nothing to do and starts nothing again.
	if rb.Retired == "" || fileExists(RecordPath(home, migrate.Hermes)) {
		t.Errorf("record not retired: %q", rb.Retired)
	}
	starts := countRan(sys, "systemctl --user start hermes-gateway.service")
	if _, err := Rollback(ctx, sys, home, migrate.Hermes, fixedNow); err == nil {
		t.Error("a second rollback found something to do")
	}
	if n := countRan(sys, "systemctl --user start hermes-gateway.service"); n != 1 || starts != 1 {
		t.Errorf("the unit was started %d times", n)
	}
}

func countRan(sys *fakeSystem, line string) int {
	n := 0
	for _, l := range sys.ran {
		if l == line {
			n++
		}
	}
	return n
}

func fixedNow() time.Time { return time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC) }

// A supervisor the cutover cannot stop keeps the old gateway (and its
// scheduler) running: finish must not resume the imported schedules.
func TestFinishRefusesWhileOldSchedulerRuns(t *testing.T) {
	ctx := context.Background()
	userHome, dir := fixture(t, migrate.Hermes)
	sys := newFake(userHome)
	sys.run(Process{PID: 100, PPID: 1, Cmdline: "hermes -p default gateway run --replace"})
	sys.respawn[100] = true
	home, found := imported(t, migrate.Hermes, userHome)
	o := FinishOptions{Options: Options{From: migrate.Hermes, Getenv: noEnv}, Home: home, Found: found}

	res, err := Finish(ctx, sys, o)
	if !errors.Is(err, ErrSchedulerRunning) {
		t.Fatalf("err = %v, want ErrSchedulerRunning", err)
	}
	if !res.Status.SchedulerRunning() {
		t.Error("the status after the stop should show the scheduler")
	}
	for name, on := range habakkukSchedules(t, home) {
		if on {
			t.Errorf("%s was resumed while the old scheduler runs", name)
		}
	}
	if j := hermesJobs(t, dir)["a1b2c3d4e5f6"]; j["enabled"] != true {
		t.Error("its jobs were edited while its scheduler runs")
	}
	if !fileExists(filepath.Join(dir, "ESTOP")) {
		t.Error("the pause switch should be set even when the stop fails")
	}

	// --force resumes them anyway, and still leaves the old agent's own
	// schedules alone while its scheduler runs.
	o.Force = true
	res, err = Finish(ctx, sys, o)
	if err != nil {
		t.Fatalf("finish --force: %v", err)
	}
	if on := habakkukSchedules(t, home); !on["habakkuk/Morning brief"] || !on["habakkuk/Inbox sweep"] {
		t.Errorf("--force did not resume them: %v", on)
	}
	if j := hermesJobs(t, dir)["a1b2c3d4e5f6"]; j["enabled"] != true {
		t.Error("--force edited the jobs of a scheduler that runs")
	}
	if !slices.ContainsFunc(res.Left, func(l string) bool { return strings.Contains(l, "stay on") }) {
		t.Errorf("--force should say the old schedules stay on: %v", res.Left)
	}
}

// Once the scheduler is stopped by other means, finishing again completes.
func TestFinishAfterRefusalCompletes(t *testing.T) {
	ctx := context.Background()
	userHome, _ := fixture(t, migrate.Hermes)
	sys := newFake(userHome)
	sys.run(Process{PID: 100, PPID: 1, Cmdline: "hermes gateway run"})
	sys.respawn[100] = true
	home, found := imported(t, migrate.Hermes, userHome)
	o := FinishOptions{Options: Options{From: migrate.Hermes, Getenv: noEnv}, Home: home, Found: found}
	if _, err := Finish(ctx, sys, o); !errors.Is(err, ErrSchedulerRunning) {
		t.Fatalf("err = %v", err)
	}
	delete(sys.respawn, 100)
	if _, err := Finish(ctx, sys, o); err != nil {
		t.Fatal(err)
	}
	if on := habakkukSchedules(t, home); !on["habakkuk/Morning brief"] {
		t.Errorf("not resumed after the second finish: %v", on)
	}
}

func TestSystemUnitNeedsAnAdministrator(t *testing.T) {
	ctx := context.Background()
	sys := newFake(t.TempDir())
	svc := Service{Manager: SystemdSystem, Name: "hermes-gateway.service"}
	if !svc.NeedsRoot() || !strings.HasPrefix(svc.StopCommand(sys.UID()), "sudo systemctl disable --now") {
		t.Errorf("stop command = %q", svc.StopCommand(sys.UID()))
	}
	if err := stopService(ctx, sys, svc); err == nil {
		t.Error("stopping a system unit should need an administrator")
	}
	if len(sys.ran) > 0 {
		t.Errorf("ran %v", sys.ran)
	}
}

// A worker that ignores the polite stop is forced.
func TestStopForcesAProcessThatIgnoresTerm(t *testing.T) {
	sys := newFake(t.TempDir())
	sys.run(Process{PID: 42, Cmdline: "python -m hermes_cli.kanban_worker_client"})
	sys.ignoreTerm[42] = true
	if err := stopProcess(sys, 42, time.Second); err != nil {
		t.Fatal(err)
	}
	if sys.Alive(42) || !slices.Equal(sys.stopped, []int{42, 42}) {
		t.Errorf("stopped %v, alive %v", sys.stopped, sys.Alive(42))
	}
	if err := stopProcess(sys, sys.Self(), time.Second); err == nil {
		t.Error("it must never stop itself")
	}
}

func TestJobFieldsMoveOnlyFromWhatTheyHold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")
	if err := os.WriteFile(path, []byte(`{"jobs":[{"id":"x","enabled":true,"state":"scheduled","repeat":{"times":1000000}},{"id":"y"}],"updated_at":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	js := hermesSpec.store(path)
	prior, err := js.read([]string{"x", "y"})
	if err != nil {
		t.Fatal(err)
	}
	set := js.paused(fixedNow())
	done, err := js.write(map[string][2]fields{"x": {prior["x"], set}, "y": {prior["y"], set}})
	if err != nil || len(done) != 2 {
		t.Fatalf("paused %v, %v", done, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"times": 1000000`) || !strings.Contains(string(data), `"updated_at": "t"`) {
		t.Errorf("other fields changed:\n%s", data)
	}
	// You pause y yourself in the old agent afterwards.
	f, _ := readJobsFile(path)
	f.jobs[1]["paused_reason"] = "mine"
	if err := f.write(); err != nil {
		t.Fatal(err)
	}
	back, err := js.write(map[string][2]fields{"x": {set, prior["x"]}, "y": {set, prior["y"]}})
	if err != nil || !slices.Equal(back, []string{"x"}) {
		t.Fatalf("put back %v, %v; want only x", back, err)
	}
	jobs := map[string]map[string]any{}
	f, _ = readJobsFile(path)
	for _, j := range f.jobs {
		jobs[jobID(j)] = j
	}
	// x exactly as it was: no field added, none left behind.
	if x := jobs["x"]; x["enabled"] != true || x["state"] != "scheduled" || len(x) != 4 {
		t.Errorf("x = %v", x)
	}
	if jobs["y"]["paused_reason"] != "mine" {
		t.Error("a job paused since by someone else was changed")
	}
	// Twice changes nothing.
	if again, _ := js.write(map[string][2]fields{"x": {set, prior["x"]}}); len(again) != 0 {
		t.Errorf("put back twice: %v", again)
	}
}

func TestLaunchdAndWindowsServices(t *testing.T) {
	ctx := context.Background()
	t.Run("launchd", func(t *testing.T) {
		userHome, dir := fixture(t, migrate.Hermes)
		sys := newFake(userHome)
		sys.goos = "darwin"
		plist := filepath.Join(userHome, "Library", "LaunchAgents", "ai.hermes.gateway.plist")
		other := filepath.Join(userHome, "Library", "LaunchAgents", "ai.hermes.gateway-elsewhere.plist")
		_ = os.MkdirAll(filepath.Dir(plist), 0o700)
		_ = os.WriteFile(plist, []byte("<plist><string>gateway</string></plist>"), 0o600)
		// Another home's agent names its own HERMES_HOME.
		_ = os.WriteFile(other, []byte("<key>HERMES_HOME</key><string>/srv/other-home</string>"), 0o600)
		sys.service(Launchd, "ai.hermes.gateway", true, true, Process{PID: 7, Cmdline: "hermes gateway run --external-supervisor"})
		sys.service(Launchd, "ai.hermes.gateway-elsewhere", true, true)
		st, err := Detect(ctx, sys, Options{From: migrate.Hermes, Source: dir, Getenv: noEnv})
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Services) != 1 || st.Services[0].Name != "ai.hermes.gateway" || !st.Services[0].Active {
			t.Fatalf("services = %+v", st.Services)
		}
		if err := stopService(ctx, sys, st.Services[0].Service); err != nil {
			t.Fatal(err)
		}
		if s := sys.services["ai.hermes.gateway"]; s.active || s.enabled || sys.Alive(7) {
			t.Errorf("still running or enabled: %+v", s)
		}
		if err := restoreService(ctx, sys, st.Services[0].Service, st.Services[0].ServiceState); err != nil {
			t.Fatal(err)
		}
		if s := sys.services["ai.hermes.gateway"]; !s.active || !s.enabled {
			t.Errorf("not restored: %+v", s)
		}
	})
	t.Run("windows", func(t *testing.T) {
		userHome, dir := fixture(t, migrate.Hermes)
		sys := newFake(userHome)
		sys.goos = "windows"
		appdata := filepath.Join(userHome, "AppData", "Roaming")
		launcher := filepath.Join(dir, "gateway-service", "Hermes_Gateway.vbs")
		startup := filepath.Join(appdata, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "Hermes_Gateway.cmd")
		for _, f := range []string{launcher, startup} {
			_ = os.MkdirAll(filepath.Dir(f), 0o700)
			_ = os.WriteFile(f, []byte("rem"), 0o600)
		}
		sys.service(Schtasks, "Hermes_Gateway", true, true, Process{PID: 9, Cmdline: `C:\hermes\venv\Scripts\python.exe -m hermes_cli.main gateway run`})
		getenv := func(k string) string {
			if k == "APPDATA" {
				return appdata
			}
			return ""
		}
		st, err := Detect(ctx, sys, Options{From: migrate.Hermes, Source: dir, Getenv: getenv})
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Services) != 2 {
			t.Fatalf("services = %+v", st.Services)
		}
		for _, sv := range st.Services {
			if err := stopService(ctx, sys, sv.Service); err != nil {
				t.Fatal(err)
			}
		}
		if fileExists(startup) || !fileExists(startup+startupOff) {
			t.Error("the Startup launcher was not set aside")
		}
		if s := sys.services["Hermes_Gateway"]; s.active || s.enabled {
			t.Errorf("task still on: %+v", s)
		}
		for _, sv := range st.Services {
			if err := restoreService(ctx, sys, sv.Service, sv.ServiceState); err != nil {
				t.Fatal(err)
			}
		}
		if !fileExists(startup) || !sys.services["Hermes_Gateway"].active {
			t.Error("not restored")
		}
	})
}

func TestOpenClawFinishAndRollback(t *testing.T) {
	t.Run("jobs file", func(t *testing.T) { openclawFinishAndRollback(t, false) })
	t.Run("database", func(t *testing.T) { openclawFinishAndRollback(t, true) })
}

// openclawDB puts a cron_jobs table in the home, shaped like OpenClaw's,
// with one job on and one off.
func openclawDB(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "state", "openclaw.sqlite")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := openOpenClawDB(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE cron_jobs (store_key TEXT NOT NULL, job_id TEXT NOT NULL, name TEXT NOT NULL, enabled INTEGER NOT NULL,
		agent_id TEXT, payload_kind TEXT NOT NULL, job_json TEXT NOT NULL, sort_order INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL, PRIMARY KEY (store_key, job_id))`); err != nil {
		t.Fatal(err)
	}
	for _, j := range []struct {
		id, name string
		on       bool
	}{{"j9", "Evening wrap", true}, {"j10", "Off one", false}} {
		js, _ := json.Marshal(map[string]any{"id": j.id, "name": j.name, "enabled": j.on,
			"schedule": map[string]any{"kind": "cron", "expr": "0 21 * * *"},
			"payload":  map[string]any{"kind": "agentTurn", "message": "Wrap up the day."}})
		on := 0
		if j.on {
			on = 1
		}
		if _, err := db.Exec(`INSERT INTO cron_jobs VALUES (?, ?, ?, ?, 'work', 'agentTurn', ?, 0, 1)`,
			filepath.Join(dir, "cron", "jobs.json"), j.id, j.name, on, string(js)); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func openclawFinishAndRollback(t *testing.T, withDB bool) {
	ctx := context.Background()
	userHome, dir := fixture(t, migrate.OpenClaw)
	if withDB {
		openclawDB(t, dir)
	}
	sys := newFake(userHome)
	sys.goos = "darwin"
	plist := filepath.Join(userHome, "Library", "LaunchAgents", "ai.openclaw.gateway.plist")
	update := filepath.Join(userHome, "Library", "LaunchAgents", "ai.openclaw.update.1.plist")
	_ = os.MkdirAll(filepath.Dir(plist), 0o700)
	_ = os.WriteFile(plist, []byte("<plist/>"), 0o600)
	_ = os.WriteFile(update, []byte("<plist/>"), 0o600)
	sys.service(Launchd, "ai.openclaw.gateway", true, true, Process{PID: 50, Cmdline: "openclaw-gateway"})
	sys.run(Process{PID: 51, Cmdline: "openclaw gateway status"})
	lock := filepath.Join(dir, "tmp", "openclaw-501", "gateway.abcd1234.lock")
	_ = os.MkdirAll(filepath.Dir(lock), 0o700)
	_ = os.WriteFile(lock, []byte(`{"pid": 50, "createdAt": "x"}`), 0o600)

	jobsBefore, err := openclawJobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobsBefore) == 0 {
		t.Fatal("the fixture has no schedules on")
	}
	home, found := imported(t, migrate.OpenClaw, userHome)
	st, err := Detect(ctx, sys, Options{From: migrate.OpenClaw, Getenv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Services) != 1 || len(st.Processes) != 1 || st.Processes[0].PID != 50 {
		t.Fatalf("services %+v, processes %+v", st.Services, st.Processes)
	}
	if _, err := Finish(ctx, sys, FinishOptions{Options: Options{From: migrate.OpenClaw, Getenv: noEnv}, Home: home, Found: found}); err != nil {
		t.Fatal(err)
	}
	if left, _ := openclawJobs(dir); len(left) != 0 {
		t.Errorf("still on in OpenClaw: %v", left)
	}
	if withDB {
		assertOpenClawBoth(t, jobsBefore[0].Store, "j9", 0)
		assertOpenClawBoth(t, jobsBefore[0].Store, "j10", 0)
	}
	on := habakkukSchedules(t, home)
	resumed := 0
	for _, v := range on {
		if v {
			resumed++
		}
	}
	if resumed == 0 {
		t.Errorf("no imported schedule resumed: %v", on)
	}
	if _, err := Rollback(ctx, sys, home, migrate.OpenClaw, fixedNow); err != nil {
		t.Fatal(err)
	}
	if again, _ := openclawJobs(dir); len(again) != len(jobsBefore) {
		t.Errorf("after rollback %d on, want %d", len(again), len(jobsBefore))
	}
	if withDB {
		assertOpenClawBoth(t, jobsBefore[0].Store, "j9", 1)
		assertOpenClawBoth(t, jobsBefore[0].Store, "j10", 0)
	}
	if !sys.services["ai.openclaw.gateway"].active {
		t.Error("the gateway agent is not back")
	}
	if fileExists(RecordPath(home, migrate.OpenClaw)) {
		t.Error("the record should be retired after a full rollback")
	}
}

func assertOpenClawBoth(t *testing.T, path, id string, want int) {
	t.Helper()
	db, err := openOpenClawDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var col, inJSON int
	if err := db.QueryRow(`SELECT enabled, json_extract(job_json, '$.enabled') FROM cron_jobs WHERE job_id = ?`, id).Scan(&col, &inJSON); err != nil {
		t.Fatal(err)
	}
	if col != want || inJSON != want {
		t.Errorf("job %s: column %d, job_json %d, want both %d", id, col, inJSON, want)
	}
}

func loadConfig(t *testing.T, home string) *config.Config {
	t.Helper()
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestSharedChannels(t *testing.T) {
	userHome, dir := fixture(t, migrate.Hermes)
	home, found := imported(t, migrate.Hermes, userHome)
	session := filepath.Join(dir, "platforms", "whatsapp", "session")
	_ = os.MkdirAll(session, 0o700)
	_ = os.WriteFile(filepath.Join(session, "creds.json"), []byte("{}"), 0o600)
	cfg := loadConfig(t, home)
	env, _ := migrate.ReadDotEnv(filepath.Join(home, ".env"))
	shared := SharedChannels(found, cfg, func(k string) string { return env[k] })
	var got []string
	for _, s := range shared {
		got = append(got, s.Type)
		for _, v := range found.Secrets {
			if strings.Contains(s.String(), v) {
				t.Errorf("a token is shown: %s", s)
			}
		}
	}
	if !slices.Contains(got, "telegram") || !slices.Contains(got, "whatsapp") {
		t.Errorf("shared = %v", shared)
	}
	// A different token is not shared.
	none := SharedChannels(found, cfg, func(string) string { return "another-token" })
	if slices.ContainsFunc(none, func(s Shared) bool { return s.Type == "telegram" }) {
		t.Errorf("a different token was taken for the same: %v", none)
	}
}

func TestParseProcesses(t *testing.T) {
	other := func(owner string) bool { return owner != "501" }
	ps := " 100     1  501 /usr/bin/python3 -m hermes_cli.main gateway run\n  200   100 502 node bridge.js --session /a  b\n junk\n"
	got := parseProcesses(ps, "", other)
	if len(got) != 2 || got[0].PID != 100 || got[0].Other || got[1].PPID != 100 || !got[1].Other || got[1].Cmdline != "node bridge.js --session /a  b" {
		t.Errorf("%+v", got)
	}
	win := "4321\t12\t1\t\"C:\\Program Files\\nodejs\\node.exe\" C:\\x\\openclaw\\dist\\index.js gateway\r\n8\t4\t0\t\r\n"
	got = parseProcesses(win, "\t", func(f string) bool { return f != "1" })
	if len(got) != 1 || got[0].PID != 4321 || got[0].Other {
		t.Errorf("%+v", got)
	}
	if r, ok := openclawSpec.role(got[0], "", ""); !ok || r != RoleGateway {
		t.Errorf("openclaw gateway on Windows: %q %v", r, ok)
	}
}

func TestReadPIDAndLaunchdDisabled(t *testing.T) {
	dir := t.TempDir()
	for name, text := range map[string]string{
		"plain": "123\n", "json": `{"pid": 123, "kind": "hermes-gateway"}`, "lines": "123\n1700000000\n3000\n",
	} {
		p := filepath.Join(dir, name)
		_ = os.WriteFile(p, []byte(text), 0o600)
		if n, ok := readPID(p); !ok || n != 123 {
			t.Errorf("%s: %d %v", name, n, ok)
		}
	}
	out := "disabled services = {\n\t\"ai.hermes.gateway\" => disabled\n\t\"com.other\" => enabled\n}"
	if !launchdDisabled(out, "ai.hermes.gateway") || launchdDisabled(out, "com.other") || launchdDisabled(out, "x") {
		t.Error("launchd disabled list misread")
	}
	if !mentionsDir("Environment=HERMES_HOME=/srv/h\n", "/srv/h") || mentionsDir("HERMES_HOME=/srv/h2", "/srv/h") {
		t.Error("mentionsDir")
	}
}

func TestHermesRoles(t *testing.T) {
	for cmd, want := range map[string]string{
		"/home/u/.local/bin/hermes gateway run":                                      RoleGateway,
		"/usr/bin/python3 -m hermes_cli.main -p work gateway run --replace":          RoleGateway,
		"hermes --profile work gateway":                                              RoleGateway,
		"python -m cron.scheduler --external-worker-file /h/cron/external-workers/x": RoleWorker,
		"python -m hermes_cli.kanban_worker_client":                                  RoleWorker,
		"hermes kanban daemon --pidfile /tmp/k.pid":                                  RoleWorker,
		"hermes dashboard":       RoleDashboard,
		"hermes gateway stop":    "",
		"hermes chat":            "",
		"grep hermes":            "",
		"python3 -m http.server": "",
	} {
		got, _ := hermesSpec.role(Process{Cmdline: cmd}, "/h", "")
		if got != want {
			t.Errorf("%q: role %q, want %q", cmd, got, want)
		}
	}
}
