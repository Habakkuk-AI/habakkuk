package cutover

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/migrate"
)

func cgroupOf(unit string) string {
	return "0::/user.slice/user-501.slice/user@501.service/app.slice/" + unit
}

// relaunch is the dashboard as its own Python relaunches it: python -I -c
// with the program's path and command line inside the code.
func relaunch(dir string) string {
	src := filepath.Join(dir, "hermes-agent")
	return "/opt/py/bin/python3 -I -c import sys, runpy; sys.path.insert(0, '" + src + "'); sys.argv = ['" +
		filepath.Join(src, "hermes_cli", "main.py") + "', 'dashboard', '--host', '127.0.0.1', '--port', '9119', '--no-open']; " +
		"runpy.run_module('hermes_cli.main', run_name='__main__', alter_sys=True)"
}

func writeUnit(t *testing.T, userHome, name, text string) {
	t.Helper()
	dir := filepath.Join(userHome, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The dashboard installed as its own unit and running as a relaunch is
// found by its unit, stopped with it, and put back by rollback. Another
// home's dashboard unit is left alone.
func TestDashboardUnitAndRelaunch(t *testing.T) {
	ctx := context.Background()
	userHome, dir := fixture(t, migrate.Hermes)
	sys := newFake(userHome)
	writeUnit(t, userHome, "hermes-dashboard.service", "[Service]\nEnvironmentFile=%h/.hermes/.env\n"+
		"ExecStart=%h/.hermes/hermes-agent/venv/bin/python -m hermes_cli.main dashboard --port 9119 --no-open\nRestart=on-failure\n")
	sys.service(Systemd, "hermes-dashboard.service", true, true, Process{PID: 120, PPID: 1, Cmdline: relaunch(dir)})
	writeUnit(t, userHome, "hermes-dashboard-x1.service", "[Service]\nEnvironment=HERMES_HOME=/srv/other\nExecStart=/usr/bin/hermes dashboard\n")
	sys.service(Systemd, "hermes-dashboard-x1.service", true, true, Process{PID: 121, PPID: 1, Cmdline: "/usr/bin/hermes dashboard"})
	sys.info[121] = ProcInfo{Env: map[string]string{"HERMES_HOME": "/srv/other"}, Cgroup: cgroupOf("hermes-dashboard-x1.service")}

	st, err := Detect(ctx, sys, Options{From: migrate.Hermes, Getenv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Services) != 1 || st.Services[0].Name != "hermes-dashboard.service" || st.Services[0].Role != RoleDashboard {
		t.Fatalf("services = %+v, want only hermes-dashboard.service as the dashboard", st.Services)
	}
	if got := pids(st.Processes); !slices.Equal(got, []int{120}) || st.Processes[0].Role != RoleDashboard {
		t.Fatalf("ours = %+v, want the relaunched dashboard", st.Processes)
	}
	if !strings.Contains(st.Processes[0].Why, "hermes-dashboard.service") {
		t.Errorf("why = %q, want its unit", st.Processes[0].Why)
	}
	if want := "Hermes Agent is still running: 1 dashboard (systemd hermes-dashboard)"; !strings.HasPrefix(st.Summary(), want) {
		t.Errorf("summary = %q, want it to start %q", st.Summary(), want)
	}

	home, found := imported(t, migrate.Hermes, userHome)
	if res, err := Finish(ctx, sys, FinishOptions{Options: Options{From: migrate.Hermes, Getenv: noEnv, Now: fixedNow}, Home: home, Found: found}); err != nil {
		t.Fatalf("finish: %v (left %v)", err, res.Left)
	}
	if s := sys.services["hermes-dashboard.service"]; s.active || s.enabled {
		t.Errorf("the dashboard unit is still active=%v enabled=%v", s.active, s.enabled)
	}
	if !sys.Alive(121) || !sys.services["hermes-dashboard-x1.service"].enabled {
		t.Error("another home's dashboard was stopped")
	}
	if rb, err := Rollback(ctx, sys, home, migrate.Hermes, fixedNow); err != nil || len(rb.Left) > 0 {
		t.Fatalf("rollback: %v %v", err, rb.Left)
	}
	if s := sys.services["hermes-dashboard.service"]; !s.active || !s.enabled {
		t.Errorf("rollback left the dashboard unit active=%v enabled=%v", s.active, s.enabled)
	}
}

// A relaunch names the program's own folder in its code, which says
// nothing about the home it uses: alone, with nothing readable, it is
// only one that may be the old agent's.
func TestRelaunchAloneIsOnlyListed(t *testing.T) {
	userHome, dir := fixture(t, migrate.Hermes)
	sys := newFake(userHome)
	sys.run(Process{PID: 130, PPID: 1, Cmdline: relaunch(dir)})
	sys.info[130] = ProcInfo{}
	st, err := Detect(context.Background(), sys, Options{From: migrate.Hermes, Getenv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Processes) > 0 {
		t.Errorf("tied to the home by its program's path: %+v", st.Processes)
	}
	if got := pids(st.Maybe); !slices.Equal(got, []int{130}) || st.Maybe[0].Role != RoleDashboard {
		t.Errorf("maybe = %+v, want the dashboard", st.Maybe)
	}
}

// A worker that ended leaves what it started in its scope. The scope is
// tied to the home by what is in it and the command it started, and
// stopped; another home's scope is left alone, and one nothing ties to
// any home is listed without holding the imported schedules back.
func TestWorkerScopeLeftovers(t *testing.T) {
	ctx := context.Background()
	userHome, dir := fixture(t, migrate.Hermes)
	sys := newFake(userHome)

	mine := "hermes-worker-kanban-t_1-run-3.scope"
	sys.scopes[mine] = []int{90}
	sys.scopeDesc[mine] = `/opt/py/bin/python3 -m hermes_cli.main -p coder --cli --toolsets web,file chat -q "work kanban task t_1"`
	sys.run(Process{PID: 90, PPID: 1, Cmdline: "node scripts/fixture-server.mjs 9100"})
	sys.info[90] = ProcInfo{Env: map[string]string{"HERMES_HOME": filepath.Join(dir, "profiles", "coder")}, Cgroup: cgroupOf(mine)}

	other := "hermes-worker-kanban-t_2-run-1.scope"
	sys.scopes[other] = []int{91}
	sys.run(Process{PID: 91, PPID: 1, Cmdline: "sleep 600"})
	sys.info[91] = ProcInfo{Env: map[string]string{"HERMES_HOME": "/srv/other"}, Cgroup: cgroupOf(other)}

	// The command says this home, what runs in it says another: another wins.
	mixed := "hermes-worker-kanban-t_3-run-1.scope"
	sys.scopes[mixed] = []int{93}
	sys.scopeDesc[mixed] = "/usr/bin/hermes -p coder chat -q work kanban task t_3"
	sys.run(Process{PID: 93, PPID: 1, Cmdline: "sleep 601"})
	sys.info[93] = ProcInfo{Env: map[string]string{"HERMES_HOME": "/srv/other"}, Cgroup: cgroupOf(mixed)}

	unsure := "hermes-worker-proc_x.scope"
	sys.scopes[unsure] = []int{92}
	sys.run(Process{PID: 92, PPID: 1, Cmdline: "bash ./long-job.sh"})
	sys.info[92] = ProcInfo{Cgroup: cgroupOf(unsure)} // its environment can't be read

	st, err := Detect(ctx, sys, Options{From: migrate.Hermes, Getenv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(st.Scopes, []string{mine}) {
		t.Errorf("scopes = %v, want %s", st.Scopes, mine)
	}
	if got := pids(st.Processes); !slices.Equal(got, []int{90}) || st.Processes[0].Role != RoleLeftover {
		t.Errorf("ours = %+v, want what the worker left", st.Processes)
	}
	if got := pids(st.Maybe); !slices.Equal(got, []int{92}) {
		t.Errorf("maybe = %v, want 92", got)
	}
	if !st.Live() || !strings.Contains(st.Summary(), "1 process left by a worker") {
		t.Errorf("live %v, summary %q", st.Live(), st.Summary())
	}

	home, found := imported(t, migrate.Hermes, userHome)
	res, err := Finish(ctx, sys, FinishOptions{Options: Options{From: migrate.Hermes, Getenv: noEnv, Now: fixedNow}, Home: home, Found: found})
	if err != nil {
		t.Fatalf("finish: %v (left %v)", err, res.Left)
	}
	if !slices.Contains(sys.ran, "systemctl --user stop "+mine) || sys.Alive(90) {
		t.Errorf("the scope was not stopped: %v", sys.ran)
	}
	for _, s := range []string{other, mixed, unsure} {
		if slices.Contains(sys.ran, "systemctl --user stop "+s) {
			t.Errorf("%s was stopped", s)
		}
	}
	if !sys.Alive(91) || !sys.Alive(92) || !sys.Alive(93) {
		t.Error("a process outside this home's scope was stopped")
	}
	if !slices.ContainsFunc(res.Left, func(l string) bool { return strings.Contains(l, "pid 92") }) {
		t.Errorf("the unsure one is not named: %v", res.Left)
	}
	if on := habakkukSchedules(t, home); !slices.Contains(mapValues(on), true) {
		t.Errorf("no imported schedule was resumed: %v", on)
	}
}

// The scope of a worker still running is found through the worker, as
// before, and what it started is stopped with it.
func TestWorkerScopeWithItsWorker(t *testing.T) {
	userHome, dir := fixture(t, migrate.Hermes)
	sys := newFake(userHome)
	scope := "hermes-worker-kanban-t_4-run-1.scope"
	sys.scopes[scope] = []int{95, 96}
	sys.run(Process{PID: 95, PPID: 1, Cmdline: "/opt/py/bin/python3 -m hermes_cli.main -p coder --toolsets web chat -q work kanban task t_4"})
	sys.info[95] = ProcInfo{Env: map[string]string{"HERMES_HOME": dir}, Cgroup: cgroupOf(scope)}
	sys.run(Process{PID: 96, PPID: 95, Cmdline: "node server.mjs"})
	sys.info[96] = ProcInfo{Cgroup: cgroupOf(scope)}
	st, err := Detect(context.Background(), sys, Options{From: migrate.Hermes, Getenv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(st.Scopes, []string{scope}) {
		t.Errorf("scopes = %v", st.Scopes)
	}
	if got := pids(st.Processes); !slices.Equal(got, []int{95, 96}) || st.Processes[0].Role != RoleWorker || st.Processes[1].Role != RoleLeftover {
		t.Errorf("ours = %+v, want the worker and what it started", st.Processes)
	}
}

func TestHermesRelaunchAndFlagRoles(t *testing.T) {
	for cmd, want := range map[string]string{
		relaunch("/h"): RoleDashboard,
		"python3 -I -c import runpy; runpy.run_module('hermes_cli.kanban_worker_client', run_name='__main__')":          RoleWorker,
		"/usr/bin/python3 -m hermes_cli.main -p coder --cli --accept-hooks --toolsets a,b chat -q work kanban task t_1": RoleWorker,
		"hermes --toolsets web,file gateway run":                      RoleGateway,
		"hermes -m some-model --provider p gateway":                   RoleGateway,
		"hermes -c gateway run":                                       RoleGateway,
		"hermes serve --port 9000":                                    RoleDashboard,
		"hermes -c my-session chat":                                   "",
		"hermes --toolsets web chat":                                  "",
		"python3 -c print(1)":                                         "",
		"python3 -I -c import runpy; runpy.run_module('http.server')": "",
	} {
		got, _ := hermesSpec.role(Process{Cmdline: cmd}, "/h", "")
		if got != want {
			t.Errorf("%q: role %q, want %q", cmd, got, want)
		}
	}
}

func TestHermesUnitNames(t *testing.T) {
	for name, want := range map[string][2]string{
		"hermes.service":               {RoleGateway, ""},
		"hermes-gateway.service":       {RoleGateway, ""},
		"hermes-gateway-coder.service": {RoleGateway, "coder"},
		"hermes-dashboard.service":     {RoleDashboard, ""},
		"hermes-serve-coder.service":   {RoleDashboard, "coder"},
		"hermes-server.service":        {"", ""},
		"hermes-dashboards.service":    {"", ""},
	} {
		role, suffix, _ := hermesUnit(name)
		if role != want[0] || suffix != want[1] {
			t.Errorf("%s: %q %q, want %q %q", name, role, suffix, want[0], want[1])
		}
	}
}

func TestPythonCodeParts(t *testing.T) {
	code := `import sys, runpy; sys.argv = ['/x/main.py', 'chat', '-q', 'a, b \'c\'']; runpy.run_module("hermes_cli.main")`
	if got := codeArgv(code); !slices.Equal(got, []string{"/x/main.py", "chat", "-q", "a, b 'c'"}) {
		t.Errorf("argv = %q", got)
	}
	if got := codeModule(code); got != "hermes_cli.main" {
		t.Errorf("module = %q", got)
	}
	if got := codeArgv("sys.argv = ['/x', unclosed"); !slices.Equal(got, []string{"/x"}) {
		t.Errorf("a list cut short = %q", got)
	}
}

func mapValues[K comparable, V any](m map[K]V) []V {
	var out []V
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
