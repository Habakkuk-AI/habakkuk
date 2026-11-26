package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

const controlConfig = `version: 1
providers:
  fake:
    type: openai
    base_url: http://127.0.0.1:1
    keys_env: [FAKE_KEY]
agents:
  habakkuk:
    model: fake/big
  sales:
    description: Finds leads and answers customers
    model: fake/small
    toolsets: [core, board]
channels:
  telegram:
    type: telegram
    agent: habakkuk
    token_env: TELEGRAM_BOT_TOKEN
    routes:
      "-100": sales   # Sales group
`

// controlRig is the dashboard rig with a config.yaml the control panel can
// edit, a restart hook and a model list.
func controlRig(t *testing.T) (*dashRig, *int) {
	t.Helper()
	r := newDashRig(t)
	if err := os.WriteFile(filepath.Join(r.home, "config.yaml"), []byte(controlConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	restarts := 0
	r.s.Dash.Home = r.home
	r.s.Dash.Pending = func() []string { return []string{"agents"} }
	r.s.Dash.Restart = func() error { restarts++; return nil }
	r.s.Dash.Models = func(context.Context) []ModelList {
		return []ModelList{{Provider: "fake", Models: []string{"big", "small"}}}
	}
	return r, &restarts
}

func TestControlTeamShowsAgentsAndWhereTheyAnswer(t *testing.T) {
	r, _ := controlRig(t)
	j := r.get(t, "/api/team")
	if j["writable"] != true {
		t.Fatalf("writable = %v", j["writable"])
	}
	agents := list(j, "agents")
	if len(agents) != 2 {
		t.Fatalf("agents = %v", agents)
	}
	byName := map[string]map[string]any{}
	for _, a := range agents {
		m := a.(map[string]any)
		byName[m["name"].(string)] = m
	}
	sales := byName["sales"]
	if sales["description"] != "Finds leads and answers customers" || sales["default_toolsets"] == true {
		t.Fatalf("sales = %v", sales)
	}
	answers := list(sales, "answers")
	if len(answers) != 1 || answers[0].(map[string]any)["label"] != "Sales group" {
		t.Fatalf("sales answers = %v", answers)
	}
	if byName["habakkuk"]["default_toolsets"] != true || len(list(byName["habakkuk"], "toolsets")) != 6 {
		t.Fatalf("habakkuk should show the default toolsets: %v", byName["habakkuk"])
	}
	toolsets := list(j, "toolsets")
	if toolsets[0] != "core" || !strings.Contains(toString(toolsets), "browser") {
		t.Fatalf("toolsets = %v", toolsets)
	}
}

func toString(v []any) string {
	var b strings.Builder
	for _, x := range v {
		b.WriteString(x.(string) + " ")
	}
	return b.String()
}

func TestControlSavesAgentAndRouting(t *testing.T) {
	r, _ := controlRig(t)
	out := r.send(t, "PUT", "/api/agents/sales", `{"model":"fake/big","toolsets":["core","board","browser"],"daily":3}`, http.StatusOK)
	if out["can_restart"] != true || len(list(out, "pending")) != 1 {
		t.Fatalf("save answer = %v", out)
	}
	cfg, err := config.Load(r.home)
	if err != nil {
		t.Fatal(err)
	}
	if a := cfg.Agents["sales"]; a.Model != "fake/big" || len(a.Toolsets) != 3 || a.Budget.Daily.Dollars != 3 {
		t.Fatalf("saved agent = %+v", a)
	}
	r.send(t, "PUT", "/api/agents/ghost", `{"model":"fake/big"}`, http.StatusNotFound)
	r.send(t, "PUT", "/api/agents/sales", `{"model":"nowhere/x"}`, http.StatusBadRequest)
	r.send(t, "PUT", "/api/agents/sales", `{"daily":-1}`, http.StatusBadRequest)

	r.send(t, "PUT", "/api/routing/telegram", `{"agent":"sales","routes":[{"conversation":"-200","agent":"habakkuk","label":"Ops"}]}`, http.StatusOK)
	j := r.get(t, "/api/routing")
	ch := list(j, "channels")[0].(map[string]any)
	routes := list(ch, "routes")
	if ch["agent"] != "sales" || len(routes) != 1 || routes[0].(map[string]any)["label"] != "Ops" {
		t.Fatalf("routing after save = %v", ch)
	}
	r.send(t, "PUT", "/api/routing/telegram", `{"routes":[{"conversation":"-1","agent":"ghost"}]}`, http.StatusBadRequest)
	r.send(t, "PUT", "/api/routing/nowhere", `{"routes":[]}`, http.StatusNotFound)
}

func TestControlRestartAndModels(t *testing.T) {
	r, restarts := controlRig(t)
	if out := r.get(t, "/api/restart"); out["can_restart"] != true {
		t.Fatalf("restart state = %v", out)
	}
	r.send(t, "POST", "/api/restart", `{}`, http.StatusAccepted)
	if *restarts != 1 {
		t.Fatalf("restarts = %d", *restarts)
	}
	r.s.Dash.Restart = func() error { return context.Canceled }
	r.send(t, "POST", "/api/restart", `{}`, http.StatusConflict)
	r.s.Dash.Restart = func() error { return errors.New("config.yaml does not load") }
	r.send(t, "POST", "/api/restart", `{}`, http.StatusBadRequest)
	r.s.Dash.Restart = nil
	r.send(t, "POST", "/api/restart", `{}`, http.StatusNotImplemented)
	if p := list(r.get(t, "/api/models"), "providers"); len(p) != 1 {
		t.Fatalf("models = %v", p)
	}
	app := r.get(t, "/api/app")
	if _, ok := app["restart"].(map[string]any); !ok {
		t.Fatalf("app info has no restart state: %v", app)
	}
}

func TestControlNeedsAHomeToSave(t *testing.T) {
	r := newDashRig(t)
	r.send(t, "PUT", "/api/agents/habakkuk", `{"model":"fake/m"}`, http.StatusNotImplemented)
	r.send(t, "PUT", "/api/routing/web", `{"routes":[]}`, http.StatusNotImplemented)
	if j := r.get(t, "/api/team"); j["writable"] != false {
		t.Fatalf("writable without a home = %v", j["writable"])
	}
}

func TestControlSkillOnOff(t *testing.T) {
	r := newDashRig(t)
	r.send(t, "POST", "/api/skills-off/brainstorm", `{"off":true}`, http.StatusOK)
	if where, off := r.s.Dash.Skills.IsOff("brainstorm"); !off || where != "command" {
		t.Fatalf("brainstorm off = %v (%s)", off, where)
	}
	r.send(t, "POST", "/api/skills-off/brainstorm", `{"off":false}`, http.StatusOK)
	if _, off := r.s.Dash.Skills.IsOff("brainstorm"); off {
		t.Fatal("brainstorm still off")
	}
	r.send(t, "POST", "/api/skills-off/Bad..Name", `{"off":true}`, http.StatusBadRequest)
}

func TestControlPluginPages(t *testing.T) {
	r := newDashRig(t)
	dir := t.TempDir()
	page := filepath.Join(dir, "dashboard")
	if err := os.MkdirAll(page, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"index.html": "<h1>Report</h1>", "page.js": "1", "../secret.txt": "nope"} {
		if err := os.WriteFile(filepath.Join(page, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.s.Dash.Config.Plugins = []*config.Plugin{{Name: "report", Dir: dir, Manifest: &config.PluginManifest{Name: "report",
		Dashboard: &config.PluginDashboard{Title: "Sales report"}}}}
	ext := list(r.get(t, "/api/app"), "extensions")
	if len(ext) != 1 || ext[0].(map[string]any)["title"] != "Sales report" || ext[0].(map[string]any)["url"] != "ext/report/" {
		t.Fatalf("extensions = %v", ext)
	}
	for path, want := range map[string]int{
		"/app/ext/report/":                http.StatusOK,
		"/app/ext/report/page.js":         http.StatusOK,
		"/app/ext/report/..%2Fsecret.txt": http.StatusNotFound,
		"/app/ext/nobody/":                http.StatusNotFound,
	} {
		res, err := http.Get(r.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, res.StatusCode, want)
		}
		if want == http.StatusOK && !strings.Contains(res.Header.Get("Content-Security-Policy"), "script-src 'self'") {
			t.Errorf("GET %s has no content security policy", path)
		}
	}
}
