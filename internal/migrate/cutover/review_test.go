package cutover

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/migrate"
)

func pids(rs []Running) []int {
	var out []int
	for _, r := range rs {
		out = append(out, r.PID)
	}
	return out
}

// Finish stops only processes it can tie to the home being moved: never
// another user's, another home's, or an unrelated program's, and one it
// cannot tie is listed for you, not stopped.
func TestOnlyThisHomesProcessesAreStopped(t *testing.T) {
	ctx := context.Background()
	userHome, dir := fixture(t, migrate.Hermes)
	sys := newFake(userHome)
	other := "/srv/other-home/venv/bin/python -m hermes_cli.main gateway run"
	sys.run(Process{PID: 10, PPID: 1, Cmdline: other})
	sys.info[10] = ProcInfo{Env: map[string]string{"HERMES_HOME": "/srv/other-home"}}
	sys.run(Process{PID: 11, PPID: 1, Cmdline: other}) // its environment can't be read
	sys.info[11] = ProcInfo{}
	sys.run(Process{PID: 12, PPID: 1, Cmdline: "/usr/bin/python3 -m cron.scheduler"})
	sys.run(Process{PID: 13, PPID: 1, Cmdline: "hermes gateway run", Other: true}) // sudo -u bob hermes gateway run
	sys.run(Process{PID: 14, PPID: 1, Cmdline: "sudo -u bob hermes gateway run", Other: true})
	worker := "/usr/bin/python3 -u -X utf8 -m cron.scheduler --external-worker-file " + filepath.Join(dir, "cron", "external-workers", "x.json")
	sys.run(Process{PID: 20, PPID: 1, Cmdline: worker})
	sys.info[20] = ProcInfo{} // tied by the path it names, not its environment
	sys.run(Process{PID: 21, PPID: 1, Cmdline: "python -m hermes_cli.kanban_worker_client"})
	sys.info[21] = ProcInfo{Env: map[string]string{"HERMES_HOME": dir}}
	sys.run(Process{PID: 22, PPID: 21, Cmdline: "python -m hermes_cli.kanban_worker_client"})
	sys.info[22] = ProcInfo{} // its parent is ours

	st, err := Detect(ctx, sys, Options{From: migrate.Hermes, Getenv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if got := pids(st.Processes); !slices.Equal(got, []int{20, 21, 22}) {
		t.Errorf("ours = %v, want 20 21 22", got)
	}
	if got := pids(st.Maybe); !slices.Equal(got, []int{11}) {
		t.Errorf("maybe = %v, want 11", got)
	}

	home, found := imported(t, migrate.Hermes, userHome)
	res, err := Finish(ctx, sys, FinishOptions{Options: Options{From: migrate.Hermes, Getenv: noEnv}, Home: home, Found: found})
	if !errors.Is(err, ErrSchedulerRunning) {
		t.Fatalf("err = %v: a gateway that may be the old agent's must keep the imported schedules paused", err)
	}
	slices.Sort(sys.stopped)
	if !slices.Equal(sys.stopped, []int{20, 21, 22}) {
		t.Errorf("stopped %v, want only 20 21 22", sys.stopped)
	}
	if !slices.ContainsFunc(res.Left, func(l string) bool { return strings.Contains(l, "pid 11) may be Hermes Agent's") }) {
		t.Errorf("the unproven gateway is not named: %v", res.Left)
	}
	for n, on := range habakkukSchedules(t, home) {
		if on {
			t.Errorf("%s resumed", n)
		}
	}
}

// When the process list can't be read, or the platform isn't one it
// knows, finish can't tell whether the old scheduler runs: it resumes
// nothing unless --force.
func TestFinishWithoutAProcessList(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		set  func(*fakeSystem)
	}{
		{"ps fails", func(f *fakeSystem) { f.procErr = errors.New("ps: not found") }},
		{"unknown system", func(f *fakeSystem) { f.goos = "plan9" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			userHome, dir := fixture(t, migrate.Hermes)
			sys := newFake(userHome)
			c.set(sys)
			home, found := imported(t, migrate.Hermes, userHome)
			o := FinishOptions{Options: Options{From: migrate.Hermes, Getenv: noEnv}, Home: home, Found: found}
			res, err := Finish(ctx, sys, o)
			if !errors.Is(err, ErrUnchecked) {
				t.Fatalf("err = %v, want ErrUnchecked", err)
			}
			if !slices.ContainsFunc(res.Left, func(l string) bool { return strings.HasPrefix(l, "could not check") }) {
				t.Errorf("left %v", res.Left)
			}
			for n, on := range habakkukSchedules(t, home) {
				if on {
					t.Errorf("%s resumed", n)
				}
			}
			if hermesJobs(t, dir)["a1b2c3d4e5f6"]["enabled"] != true {
				t.Error("its jobs were edited without knowing it had stopped")
			}
			o.Force = true
			if _, err := Finish(ctx, sys, o); err != nil {
				t.Fatalf("--force: %v", err)
			}
			if !habakkukSchedules(t, home)["habakkuk/Morning brief"] {
				t.Error("--force did not resume")
			}
		})
	}
}

// A record that can't be written stops finish before the change it would
// have recorded.
func TestRecordFailsBeforeAnyChange(t *testing.T) {
	sys, userHome, dir := hermesRunning(t)
	home, found := imported(t, migrate.Hermes, userHome)
	saveHook = func(*Record) error { return errors.New("read-only file system") }
	t.Cleanup(func() { saveHook = nil })
	_, err := Finish(context.Background(), sys, FinishOptions{Options: Options{From: migrate.Hermes, Getenv: noEnv}, Home: home, Found: found})
	if !errors.Is(err, ErrRecord) {
		t.Fatalf("err = %v, want ErrRecord", err)
	}
	if fileExists(filepath.Join(dir, "ESTOP")) || len(sys.changed()) > 0 {
		t.Errorf("changed without a record: ESTOP %v, %v", fileExists(filepath.Join(dir, "ESTOP")), sys.changed())
	}
}

// A finish that stops between making a change and marking it done (the
// record became unwritable, or the machine went down) leaves a pending
// entry, and rollback undoes it.
func TestRecordFailsPartwayAndRollbackRecovers(t *testing.T) {
	ctx := context.Background()
	sys, userHome, dir := hermesRunning(t)
	home, found := imported(t, migrate.Hermes, userHome)
	saveHook = func(r *Record) error {
		if last := r.Changes[len(r.Changes)-1]; last.Kind == ChangeService && last.State == Done {
			return errors.New("disk full")
		}
		return nil
	}
	_, err := Finish(ctx, sys, FinishOptions{Options: Options{From: migrate.Hermes, Getenv: noEnv}, Home: home, Found: found})
	saveHook = nil
	if !errors.Is(err, ErrRecord) {
		t.Fatalf("err = %v, want ErrRecord", err)
	}
	if s := sys.services["hermes-gateway.service"]; s.active || s.enabled {
		t.Fatal("the unit should have been stopped before the record failed")
	}
	if slices.Contains(sys.stopped, 200) {
		t.Error("finish went on after the record failed")
	}
	rec, _ := LoadRecord(home, migrate.Hermes)
	if i := slices.IndexFunc(rec.Changes, func(c Change) bool { return c.Kind == ChangeService }); i < 0 || rec.Changes[i].State != Pending {
		t.Fatalf("the unit's entry should be pending: %+v", rec.Changes)
	}
	rb, err := Rollback(ctx, sys, home, migrate.Hermes, fixedNow)
	if err != nil || len(rb.Left) > 0 {
		t.Fatalf("rollback: %v, left %v", err, rb.Left)
	}
	if s := sys.services["hermes-gateway.service"]; !s.active || !s.enabled {
		t.Error("the pending unit stop was not undone")
	}
	if fileExists(filepath.Join(dir, "ESTOP")) {
		t.Error("the pause switch is still set")
	}
}

// Workers that run in systemd scopes are stopped through their scope, and
// a bridge is found however its path and session are written.
func TestScopesAndBridges(t *testing.T) {
	ctx := context.Background()
	userHome, dir := fixture(t, migrate.Hermes)
	sys := newFake(userHome)
	scope := "hermes-worker-cron-a1b2-exec-9.scope"
	sys.run(Process{PID: 70, PPID: 1, Cmdline: "/usr/bin/python3 -m cron.scheduler --external-worker-file /tmp/x.json"})
	sys.info[70] = ProcInfo{Env: map[string]string{"HERMES_HOME": dir}, Cgroup: "0::/user.slice/user@501.service/app.slice/" + scope}
	sys.scopes[scope] = []int{70}
	sys.scopes["hermes-worker-kanban-other-run-1.scope"] = nil // another home's, nothing of ours in it
	sys.run(Process{PID: 80, PPID: 1, Cmdline: "node /usr/lib/hermes/bridge/bridge.js --port 3000 --session ~/.hermes/platforms/whatsapp/session"})
	sys.info[80] = ProcInfo{}
	sys.run(Process{PID: 81, PPID: 1, Cmdline: "node /srv/elsewhere/whatsapp-bridge/bridge.js --session /srv/elsewhere/session"})
	sys.info[81] = ProcInfo{Env: map[string]string{"HERMES_HOME": "/srv/elsewhere"}}

	st, err := Detect(ctx, sys, Options{From: migrate.Hermes, Getenv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(st.Scopes, []string{scope}) {
		t.Errorf("scopes = %v", st.Scopes)
	}
	if got := pids(st.Processes); !slices.Equal(got, []int{70, 80}) {
		t.Errorf("ours = %v, want 70 80", got)
	}
	home, found := imported(t, migrate.Hermes, userHome)
	if _, err := Finish(ctx, sys, FinishOptions{Options: Options{From: migrate.Hermes, Getenv: noEnv}, Home: home, Found: found}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(sys.ran, "systemctl --user stop "+scope) {
		t.Errorf("the scope was not stopped: %v", sys.ran)
	}
	if !sys.Alive(81) {
		t.Error("another home's bridge was stopped")
	}
	rb, err := Rollback(ctx, sys, home, migrate.Hermes, fixedNow)
	if err != nil || len(rb.Left) > 0 {
		t.Fatalf("rollback: %v %v", err, rb.Left)
	}
	if !slices.ContainsFunc(rb.Gone, func(g string) bool { return strings.Contains(g, scope) }) {
		t.Errorf("the scope is not named as gone: %v", rb.Gone)
	}
}

// Rollback puts a launchd agent back as it was: one you had disabled stays
// disabled, though it was running.
func TestLaunchdRollbackEnablesOnlyWhatWasEnabled(t *testing.T) {
	ctx := context.Background()
	userHome, _ := fixture(t, migrate.Hermes)
	sys := newFake(userHome)
	sys.goos = "darwin"
	plist := filepath.Join(userHome, "Library", "LaunchAgents", "ai.hermes.gateway.plist")
	_ = os.MkdirAll(filepath.Dir(plist), 0o700)
	_ = os.WriteFile(plist, []byte("<plist/>"), 0o600)
	sys.service(Launchd, "ai.hermes.gateway", true, false, Process{PID: 7, Cmdline: "hermes gateway run"})
	home, found := imported(t, migrate.Hermes, userHome)
	if _, err := Finish(ctx, sys, FinishOptions{Options: Options{From: migrate.Hermes, Getenv: noEnv}, Home: home, Found: found}); err != nil {
		t.Fatal(err)
	}
	rec, _ := LoadRecord(home, migrate.Hermes)
	i := slices.IndexFunc(rec.Changes, func(c Change) bool { return c.Kind == ChangeService })
	if i < 0 || rec.Changes[i].Was.Enabled || !rec.Changes[i].Was.Active {
		t.Fatalf("prior state not recorded: %+v", rec.Changes)
	}
	sys.ran = nil
	if _, err := Rollback(ctx, sys, home, migrate.Hermes, fixedNow); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(sys.ran, func(l string) bool { return strings.HasPrefix(l, "launchctl enable") }) {
		t.Errorf("rollback enabled an agent that was disabled: %v", sys.ran)
	}
	if s := sys.services["ai.hermes.gateway"]; !s.active || s.enabled {
		t.Errorf("want running and still disabled, got %+v", s)
	}
}

// A rollback that can't do every step keeps the record, and the next run
// does only what is left: a service already put back isn't started again.
func TestRollbackAgainDoesOnlyWhatIsLeft(t *testing.T) {
	ctx := context.Background()
	sys, userHome, _ := hermesRunning(t)
	unit := filepath.Join(userHome, ".config", "systemd", "user", "hermes-gateway-coder.service")
	_ = os.WriteFile(unit, []byte("[Service]\nExecStart=hermes -p coder gateway run\n"), 0o600)
	sys.service(Systemd, "hermes-gateway-coder.service", true, true, Process{PID: 110, Cmdline: "hermes -p coder gateway run"})
	home, found := imported(t, migrate.Hermes, userHome)
	if _, err := Finish(ctx, sys, FinishOptions{Options: Options{From: migrate.Hermes, Getenv: noEnv}, Home: home, Found: found}); err != nil {
		t.Fatal(err)
	}
	sys.fail["systemctl --user enable hermes-gateway-coder.service"] = errors.New("unit masked")
	rb, err := Rollback(ctx, sys, home, migrate.Hermes, fixedNow)
	if err != nil || len(rb.Left) != 1 || rb.Retired != "" {
		t.Fatalf("first rollback: %v, left %v, retired %q", err, rb.Left, rb.Retired)
	}
	rb, err = Rollback(ctx, sys, home, migrate.Hermes, fixedNow)
	if err != nil || len(rb.Left) != 0 || rb.Retired == "" {
		t.Fatalf("second rollback: %v, left %v", err, rb.Left)
	}
	if !slices.Equal(rb.Services, []string{"systemd hermes-gateway-coder"}) || rb.Resumed != 0 || len(rb.Paused) != 0 {
		t.Errorf("the second rollback redid finished steps: %+v", rb)
	}
	if n := countRan(sys, "systemctl --user start hermes-gateway.service"); n != 1 {
		t.Errorf("the main unit was started %d times", n)
	}
}

// OpenClaw's jobs come back exactly as they were: a job_json with no
// "enabled" gets none back, and a job you turned off since stays off.
func TestOpenClawRollbackRestoresExactPriorState(t *testing.T) {
	ctx := context.Background()
	userHome, dir := fixture(t, migrate.OpenClaw)
	path := openclawDB(t, dir)
	db, err := openOpenClawDB(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cron_jobs VALUES (?, 'j11', 'No flag', 1, 'work', 'agentTurn', '{"id":"j11","name":"No flag"}', 0, 1)`, filepath.Join(dir, "cron", "jobs.json")); err != nil {
		t.Fatal(err)
	}
	db.Close()
	sys := newFake(userHome)
	home, found := imported(t, migrate.OpenClaw, userHome)
	if _, err := Finish(ctx, sys, FinishOptions{Options: Options{From: migrate.OpenClaw, Getenv: noEnv}, Home: home, Found: found}); err != nil {
		t.Fatal(err)
	}
	before, _ := readOpenClawFields(path, []string{"j11"})
	if canon(before["j11"]["json"]) != "false" {
		t.Fatalf("j11 not paused in job_json: %s", before["j11"]["json"])
	}
	// You turn j9 back on yourself, then off again with your own value.
	if _, err := writeOpenClawFields(path, map[string][2]fields{"j9": {{"column": rawOf(0), "json": rawOf(false)}, {"column": rawOf(0)}}}); err != nil {
		t.Fatal(err)
	}
	rb, err := Rollback(ctx, sys, home, migrate.OpenClaw, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := readOpenClawFields(path, []string{"j9", "j10", "j11"})
	if _, has := after["j11"]["json"]; has || canon(after["j11"]["column"]) != "1" {
		t.Errorf("j11 = %v, want the column on and no enabled in job_json", after["j11"])
	}
	if canon(after["j10"]["column"]) != "0" {
		t.Errorf("j10 was off before and should stay off: %v", after["j10"])
	}
	if !reflect.DeepEqual(after["j9"], fields{"column": rawOf(0)}) || len(rb.Kept) != 1 {
		t.Errorf("j9 changed since and should be kept as you set it: %v, kept %v", after["j9"], rb.Kept)
	}
}

func TestOpenClawJSONJobWithoutAFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	_ = os.WriteFile(path, []byte(`{"version":1,"jobs":[{"id":"a"},{"id":"b","enabled":true}]}`), 0o600)
	js := openclawSpec.store(path)
	prior, _ := js.read([]string{"a", "b"})
	set := js.paused(fixedNow())
	if done, err := js.write(map[string][2]fields{"a": {prior["a"], set}, "b": {prior["b"], set}}); err != nil || len(done) != 2 {
		t.Fatal(done, err)
	}
	if done, err := js.write(map[string][2]fields{"a": {set, prior["a"]}, "b": {set, prior["b"]}}); err != nil || len(done) != 2 {
		t.Fatal(done, err)
	}
	data, _ := os.ReadFile(path)
	var doc struct{ Jobs []map[string]any }
	_ = json.Unmarshal(data, &doc)
	if _, has := doc.Jobs[0]["enabled"]; has || doc.Jobs[1]["enabled"] != true {
		t.Errorf("jobs = %v", doc.Jobs)
	}
}

// The dashboard ticks the schedules but polls no channel.
func TestDashboardFiresSchedulesButPollsNoChannel(t *testing.T) {
	st := &Status{Processes: []Running{{Role: RoleDashboard}}}
	if !st.SchedulerRunning() || st.UsesChannels() {
		t.Errorf("dashboard: scheduler %v, channels %v", st.SchedulerRunning(), st.UsesChannels())
	}
	st.Processes = append(st.Processes, Running{Role: RoleBridge})
	if !st.UsesChannels() {
		t.Error("a bridge polls a channel")
	}
}

func TestPythonFlagsBeforeTheModule(t *testing.T) {
	p := Process{Cmdline: "/usr/bin/python3 -u -X utf8 -m cron.scheduler --external-worker-file /h/cron/external-workers/1.json --ack-file /h/a"}
	if r, ok := hermesRole(p, "/h", ""); !ok || r != RoleWorker {
		t.Errorf("role %q %v", r, ok)
	}
	if _, ok := hermesRole(Process{Cmdline: "/usr/bin/python3 -u -m cron.scheduler"}, "/h", ""); ok {
		t.Error("another project's cron.scheduler module was taken for Hermes Agent's")
	}
	if _, ok := hermesRole(Process{Cmdline: "vim /home/u/bin/hermes"}, "/h", ""); ok {
		t.Error("an editor on a file named hermes was taken for it")
	}
	if r, ok := hermesRole(Process{Cmdline: "/usr/bin/python3 -X utf8 /home/u/.local/bin/hermes gateway run"}, "/h", ""); !ok || r != RoleGateway {
		t.Errorf("python running the hermes command: %q %v", r, ok)
	}
}
