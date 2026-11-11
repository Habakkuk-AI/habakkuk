package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/app"
	"github.com/habakkuk-ai/habakkuk/internal/migrate/cutover"
	"github.com/habakkuk-ai/habakkuk/internal/work/schedule"
)

// Every test in this package sees a computer with nothing running, so no
// test reads, let alone stops, a real process or service.
func TestMain(m *testing.M) {
	newCutoverSystem = func() cutover.System { return &fakeComputer{home: os.TempDir()} }
	os.Exit(m.Run())
}

// fakeComputer is a process table and nothing else: no service manager
// knows anything, and stopping a process removes it unless it respawns.
type fakeComputer struct {
	mu      sync.Mutex
	home    string
	procs   []cutover.Process
	respawn map[int]bool
	stopped []int
}

func (f *fakeComputer) GOOS() string       { return "linux" }
func (f *fakeComputer) UserHome() string   { return f.home }
func (f *fakeComputer) Self() int          { return 1 }
func (f *fakeComputer) UID() int           { return 501 }
func (f *fakeComputer) Wait(time.Duration) {}
func (f *fakeComputer) Processes(context.Context) ([]cutover.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// A real list is never empty: the system's own processes are there.
	return append(slices.Clone(f.procs), cutover.Process{PID: 1, Cmdline: "/sbin/init"}), nil
}

func (f *fakeComputer) Alive(pid int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.procs, func(p cutover.Process) bool { return p.PID == pid })
}

func (f *fakeComputer) Stop(pid int, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, pid)
	if !f.respawn[pid] {
		f.procs = slices.DeleteFunc(f.procs, func(p cutover.Process) bool { return p.PID == pid })
	}
	return nil
}

// Inspect: every process's environment is readable and names no home,
// so it uses the default home.
func (f *fakeComputer) Inspect(context.Context, int) cutover.ProcInfo {
	return cutover.ProcInfo{Env: map[string]string{}}
}

func (f *fakeComputer) Run(context.Context, string, ...string) (string, error) {
	return "", errors.New("not installed")
}

// useComputer makes the commands in one test see f.
func useComputer(t *testing.T, f *fakeComputer) {
	old := newCutoverSystem
	newCutoverSystem = func() cutover.System { return f }
	t.Cleanup(func() { newCutoverSystem = old })
}

func schedulesOn(t *testing.T, home string) map[string]bool {
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

func migrateRun(t *testing.T, home, user, stdin string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runMigrate(context.Background(), home, args, strings.NewReader(stdin), &out, noEnv, user)
	return out.String(), err
}

// The plan names the old agent still running, and the report points at
// hk migrate finish.
func TestMigratePlanShowsTheOldAgentRunning(t *testing.T) {
	user, _ := sourceHome(t, "hermes")
	pc := &fakeComputer{home: user, procs: []cutover.Process{
		{PID: 100, PPID: 1, Cmdline: "/opt/hermes/venv/bin/python -m hermes_cli.main gateway run"},
		{PID: 200, PPID: 1, Cmdline: "/opt/hermes/venv/bin/python -m hermes_cli.kanban_worker_client"},
	}}
	useComputer(t, pc)
	home := t.TempDir()
	out, err := migrateRun(t, home, user, "", "--from", "hermes", "-y", "--secrets", "copy")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"Hermes Agent is still running: gateway, 1 worker, 4 active schedules",
		"hk migrate finish stops it",
		"hk migrate finish --from hermes stops Hermes Agent",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(pc.stopped) > 0 {
		t.Errorf("the import stopped processes: %v", pc.stopped)
	}
	// After the import the Telegram bot is in both.
	out, err = migrateRun(t, home, user, "", "--from", "hermes", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Both would use telegram (telegram): the same Telegram bot token as Hermes Agent's telegram channel") {
		t.Errorf("the shared channel is not named:\n%s", out)
	}
	assertNoSecrets(t, out)
}

func TestMigrateFinishAndRollbackCommands(t *testing.T) {
	user, dir := sourceHome(t, "hermes")
	pc := &fakeComputer{home: user, procs: []cutover.Process{
		{PID: 100, PPID: 1, Cmdline: "hermes gateway run"},
		{PID: 200, PPID: 1, Cmdline: "python -m hermes_cli.kanban_worker_client"},
	}}
	useComputer(t, pc)
	home := t.TempDir()
	if out, err := migrateRun(t, home, user, "", "--from", "hermes", "-y", "--secrets", "copy"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	jobs, _ := os.ReadFile(dir + "/cron/jobs.json")

	// --dry-run and a "no" change nothing.
	for _, run := range []struct{ stdin string }{{""}, {"n\n"}} {
		args := []string{"finish"}
		if run.stdin == "" {
			args = append(args, "--dry-run")
		}
		out, err := migrateRun(t, home, user, run.stdin, args...)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if !strings.Contains(out, "Hermes Agent is still running: gateway, 1 worker, 4 active schedules") {
			t.Errorf("finish does not say what runs:\n%s", out)
		}
		if len(pc.stopped) > 0 {
			t.Fatalf("stopped %v", pc.stopped)
		}
		if now, _ := os.ReadFile(dir + "/cron/jobs.json"); string(now) != string(jobs) {
			t.Fatal("jobs.json changed")
		}
		for n, on := range schedulesOn(t, home) {
			if on {
				t.Fatalf("%s was resumed", n)
			}
		}
	}

	out, err := migrateRun(t, home, user, "", "finish", "-y")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"Stopped the gateway (pid 100)", "Stopped the worker (pid 200)",
		"Paused 4 of its schedules in Hermes Agent", "Resumed 3 imported schedules", "Only Habakkuk runs now"} {
		if !strings.Contains(out, want) {
			t.Errorf("finish output lacks %q:\n%s", want, out)
		}
	}
	if on := schedulesOn(t, home); !on["habakkuk/Morning brief"] || !on["habakkuk/Inbox sweep"] || !on["habakkuk/Digest script"] {
		t.Errorf("not resumed: %v", on)
	}
	out, err = migrateRun(t, home, user, "", "finish", "-y")
	if err != nil || !strings.Contains(out, "already finished") {
		t.Errorf("second finish: %v\n%s", err, out)
	}

	// Rollback can't start the processes that ran outside a service: it
	// names them, and still finishes.
	out, err = migrateRun(t, home, user, "", "rollback", "-y")
	if err != nil {
		t.Errorf("rollback: %v\n%s", err, out)
	}
	for _, want := range []string{"Paused 3 imported schedules in Habakkuk again", "Turned 4 of its schedules back on",
		"Start the ones you need the way you did before:", "the gateway (pid 100): hermes gateway run", "Hermes Agent runs again"} {
		if !strings.Contains(out, want) {
			t.Errorf("rollback output lacks %q:\n%s", want, out)
		}
	}
	for n, on := range schedulesOn(t, home) {
		if on {
			t.Errorf("%s is still on after rollback", n)
		}
	}
	// Again, with and without --from: nothing to do, and that's no error.
	for _, args := range [][]string{{"rollback", "-y"}, {"rollback", "--from", "hermes", "-y"}} {
		out, err = migrateRun(t, home, user, "", args...)
		if err != nil || !strings.Contains(out, "Nothing to roll back: the move from Hermes Agent was already rolled back") {
			t.Errorf("%v: %v\n%s", args, err, out)
		}
	}
}

func TestMigrateFinishRefusesWhileTheOldSchedulerRuns(t *testing.T) {
	user, _ := sourceHome(t, "hermes")
	pc := &fakeComputer{home: user, procs: []cutover.Process{{PID: 100, PPID: 1, Cmdline: "hermes gateway run"}},
		respawn: map[int]bool{100: true}}
	useComputer(t, pc)
	home := t.TempDir()
	if out, err := migrateRun(t, home, user, "", "--from", "hermes", "-y"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err := migrateRun(t, home, user, "", "finish", "-y")
	if err == nil {
		t.Fatalf("finish went ahead:\n%s", out)
	}
	if !strings.Contains(out, "scheduler is still running") || !strings.Contains(out, "hk migrate finish --force") {
		t.Errorf("output:\n%s", out)
	}
	for n, on := range schedulesOn(t, home) {
		if on {
			t.Errorf("%s was resumed while the old scheduler runs", n)
		}
	}
	// --force resumes them, and says plainly that the old agent still
	// runs and what is left.
	out, _ = migrateRun(t, home, user, "", "finish", "-y", "--force")
	if on := schedulesOn(t, home); !on["habakkuk/Morning brief"] {
		t.Errorf("--force did not resume: %v", on)
	}
	if !strings.Contains(out, "still runs, so a schedule that is on in both runs twice") || strings.Contains(out, "Only Habakkuk runs now") {
		t.Errorf("--force output:\n%s", out)
	}
}

func TestMigrateFinishFromCodingToolSaysNothingToStop(t *testing.T) {
	_, err := migrateRun(t, t.TempDir(), t.TempDir(), "", "finish", "--from", "codex")
	if err == nil || !strings.Contains(err.Error(), "doesn't run in the background") {
		t.Errorf("err = %v", err)
	}
	_, err = migrateRun(t, t.TempDir(), t.TempDir(), "", "rollback")
	if err == nil || !strings.Contains(err.Error(), "no finished move") {
		t.Errorf("rollback with no record: %v", err)
	}
}

func TestDoctorWarnsWhenTheOldAgentStillRuns(t *testing.T) {
	for _, k := range []string{"HERMES_HOME", "OPENCLAW_STATE_DIR", "OPENCLAW_PROFILE", "TELEGRAM_BOT_TOKEN"} {
		t.Setenv(k, "")
	}
	user, _ := sourceHome(t, "hermes")
	pc := &fakeComputer{home: user}
	useComputer(t, pc)
	home := t.TempDir()
	if out, err := migrateRun(t, home, user, "", "--from", "hermes", "-y", "--secrets", "copy"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ctx := context.Background()
	var out bytes.Buffer
	// Nothing of the old agent runs: no warning.
	if n := cutoverDoctor(ctx, home, &out); n != 0 {
		t.Fatalf("warned with nothing running: %s", out.String())
	}
	// Its gateway runs, and you resumed one imported schedule by hand.
	pc.procs = []cutover.Process{{PID: 100, PPID: 1, Cmdline: "hermes gateway run"}}
	db, err := app.OpenControl(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	st := schedule.NewStore(db, nil)
	list, _ := st.List(ctx, "")
	for _, s := range list {
		if s.Name == "Morning brief" {
			if err := st.SetEnabled(ctx, s.ID, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	db.Close()
	out.Reset()
	n := cutoverDoctor(ctx, home, &out)
	got := out.String()
	if n != 2 {
		t.Errorf("problems = %d, want 2:\n%s", n, got)
	}
	for _, want := range []string{
		"migrate: Hermes Agent is still running: gateway, 4 active schedules; 1 imported schedule is on in both, so they run twice once hk serve starts (habakkuk/Morning brief); fix: hk migrate finish --from hermes",
		"migrate: Hermes Agent is running and uses the same channel as Habakkuk (telegram); fix: hk migrate finish --from hermes",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("doctor lacks:\n%s\ngot:\n%s", want, got)
		}
	}
	assertNoSecrets(t, got)
}

// Its dashboard alone ticks schedules but polls no channel: doctor warns
// about the schedules only.
func TestDoctorDashboardAloneIsNoChannelConflict(t *testing.T) {
	for _, k := range []string{"HERMES_HOME", "OPENCLAW_STATE_DIR", "OPENCLAW_PROFILE", "TELEGRAM_BOT_TOKEN"} {
		t.Setenv(k, "")
	}
	user, _ := sourceHome(t, "hermes")
	pc := &fakeComputer{home: user}
	useComputer(t, pc)
	home := t.TempDir()
	if out, err := migrateRun(t, home, user, "", "--from", "hermes", "-y", "--secrets", "copy"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	pc.procs = []cutover.Process{{PID: 100, PPID: 1, Cmdline: "hermes dashboard"}}
	var out bytes.Buffer
	if n := cutoverDoctor(context.Background(), home, &out); n != 0 {
		t.Errorf("no schedule is on in both, and the dashboard polls no channel, yet %d problems:\n%s", n, out.String())
	}
	pc.procs = append(pc.procs, cutover.Process{PID: 101, PPID: 1, Cmdline: "hermes gateway run"})
	out.Reset()
	if n := cutoverDoctor(context.Background(), home, &out); n != 1 || !strings.Contains(out.String(), "uses the same channel") {
		t.Errorf("with its gateway running: %d problems:\n%s", n, out.String())
	}
}
