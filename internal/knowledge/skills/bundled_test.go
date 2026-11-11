package skills

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/habakkuk-ai/habakkuk/internal/core/frontmatter"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/hub"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// Every skill shipped in the binary is well formed: its folder is a valid
// name, the frontmatter parses and names the folder, the description is one
// short line and there are instructions.
func TestBundledSkillsAreValid(t *testing.T) {
	fsys := Bundled()
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(entries); n < 20 || n > 40 {
		t.Fatalf("%d bundled skills; the set is meant to stay between 20 and 40", n)
	}
	listing := 0
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			t.Errorf("%s is not a folder", name)
			continue
		}
		if err := ValidName(name); err != nil {
			t.Error(err)
		}
		data, err := fs.ReadFile(fsys, name+"/"+FileName)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var m meta
		body, err := frontmatter.Parse(data, &m)
		switch {
		case err != nil:
			t.Errorf("%s: frontmatter: %v", name, err)
		case m.Name != name:
			t.Errorf("%s: frontmatter name is %q", name, m.Name)
		case m.Description == "" || strings.Contains(m.Description, "\n") || len(m.Description) > 160:
			t.Errorf("%s: the description must be one line of at most 160 characters, got %d", name, len(m.Description))
		case !strings.Contains(m.Description, "Use "):
			t.Errorf("%s: the description must say when to use the skill", name)
		case len(body) < 400 || len(body) > MaxBody:
			t.Errorf("%s: body is %d bytes", name, len(body))
		}
		if strings.ContainsAny(string(data), "—–") {
			t.Errorf("%s: no em or en dashes", name)
		}
		listing += len("\n- " + name + ": " + m.Description)
	}
	// The prompt lists skills in at most 8 KB. The bundled ones may use
	// half of it, so they never crowd out the user's own.
	if listing > 4096 {
		t.Errorf("the bundled skills take %d bytes of the prompt's skill list; keep them within 4096 by shortening descriptions", listing)
	}
}

// The skills page of the docs names every bundled skill, and no skill that
// is not there.
func TestDocsListTheBundledSkills(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "agents", "skills.md"))
	if err != nil {
		t.Skip("the docs are not beside the code here:", err)
	}
	section, _, _ := strings.Cut(string(data), "## Where skills live")
	_, table, ok := strings.Cut(section, "## Skills that come with Habakkuk")
	if !ok {
		t.Fatal("docs/agents/skills.md has no section on the skills that come with Habakkuk")
	}
	entries, _ := fs.ReadDir(Bundled(), ".")
	have := map[string]bool{}
	for _, e := range entries {
		have[e.Name()] = true
		if !strings.Contains(table, "`"+e.Name()+"`") {
			t.Errorf("docs/agents/skills.md does not list the bundled skill %s", e.Name())
		}
	}
	for _, row := range strings.Split(table, "\n") {
		if !strings.HasPrefix(row, "| ") || strings.HasPrefix(row, "| For ") {
			continue
		}
		_, cell, _ := strings.Cut(strings.TrimPrefix(row, "| "), " | ")
		for i, part := range strings.Split(cell, "`") {
			if i%2 == 1 && !have[part] {
				t.Errorf("docs/agents/skills.md lists %s, which is not bundled", part)
			}
		}
	}
}

// The bundled skills pass the scan an install runs, with no findings at
// all, and ship nothing that could run as a program.
func TestBundledSkillsScanClean(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, Bundled()); err != nil {
		t.Fatal(err)
	}
	findings, err := hub.Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("scan finding: %s", f)
	}
	if scripts := hub.Scripts(dir); len(scripts) > 0 {
		t.Errorf("bundled skills ship scripts: %v", scripts)
	}
}

func bundledFixture() fstest.MapFS {
	return fstest.MapFS{
		"deploy/SKILL.md":          {Data: []byte(skillMD("deploy", "Ship it, the bundled way.", "Bundled deploy steps."))},
		"brief/SKILL.md":           {Data: []byte(skillMD("brief", "Write the brief.", "Bundled brief steps."))},
		"brief/templates/brief.md": {Data: []byte("# Brief for {{day}}")},
		"brief/.hidden":            {Data: []byte("x")},
		"Not_Valid/SKILL.md":       {Data: []byte(skillMD("x", "x", "x"))},
		"README.md":                {Data: []byte("not a skill")},
	}
}

// A bundled skill is listed, loads with its supporting files, and loses to
// a shared or agent skill of the same name.
func TestBundledSkillsHaveTheLowestPrecedence(t *testing.T) {
	l := fixture(t) // shared deploy and weekly-report; the agent's own deploy and inbox-zero
	l.Bundled = bundledFixture()
	list, err := l.List("habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range list {
		origin := "agent"
		switch {
		case s.Bundled:
			origin = "bundled"
		case s.Shared:
			origin = "shared"
		}
		got = append(got, s.Name+"|"+origin)
	}
	if want := "brief|bundled deploy|agent inbox-zero|agent weekly-report|shared"; strings.Join(got, " ") != want {
		t.Fatalf("list %v, want %s", got, want)
	}
	// Another agent has no deploy of its own: the shared one wins over the bundled one.
	if s, err := l.Get("other", "deploy"); err != nil || s.Bundled || !s.Shared {
		t.Fatalf("deploy for another agent: %+v %v", s, err)
	}
	s, body, files, err := l.View("habakkuk", "brief")
	if err != nil || !s.Bundled || !s.Shared || s.Dir != "" || body != "Bundled brief steps." {
		t.Fatalf("view: %+v %q %v", s, body, err)
	}
	if strings.Join(files, ",") != "templates/brief.md" {
		t.Fatalf("files %v", files)
	}
	if text, err := l.ReadFile("habakkuk", "brief", "templates/brief.md"); err != nil || text != "# Brief for {{day}}" {
		t.Fatalf("supporting file: %q %v", text, err)
	}
	for _, bad := range []string{"../deploy/SKILL.md", "/etc/hosts", "nope.md", "templates"} {
		if _, err := l.ReadFile("habakkuk", "brief", bad); err == nil {
			t.Errorf("read %q from a bundled skill", bad)
		}
	}
	if src, untrusted := l.Untrusted(s); untrusted {
		t.Fatalf("a bundled skill is trusted, got %q", src)
	}
	// Loading it is recorded like any other skill.
	l.MarkUsed(s)
	if _, ok := l.LastUsed()["bundled/brief"]; !ok {
		t.Fatalf("usage %v", l.LastUsed())
	}
	// Saving a copy under the same name makes it the agent's own.
	if _, _, err := l.Save("habakkuk", "brief", "My brief.", "My steps."); err != nil {
		t.Fatal(err)
	}
	if s, body, _, _ := l.View("habakkuk", "brief"); s.Bundled || body != "My steps." {
		t.Fatalf("the agent's copy does not win: %+v %q", s, body)
	}
	// Without the bundled folder nothing changes for a library.
	l.Bundled = nil
	if _, err := l.Get("other", "brief"); err == nil {
		t.Fatal("bundled skill found with none configured")
	}
}

// A skill is turned off by name, from the terminal or the config file,
// without removing anything; turning it on brings it back.
func TestTurnSkillsOffAndOn(t *testing.T) {
	l := fixture(t)
	l.Bundled = bundledFixture()
	names := func() string {
		list, err := l.List("habakkuk")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range list {
			out = append(out, s.Name)
		}
		return strings.Join(out, " ")
	}
	for _, name := range []string{"brief", "deploy", "weekly-report"} { // bundled, the agent's own, shared
		if err := l.SetOff(name, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.SetOff("brief", true); err != nil { // twice is fine
		t.Fatal(err)
	}
	if got := names(); got != "inbox-zero" {
		t.Fatalf("after turning three off: %s", got)
	}
	if _, err := l.Get("habakkuk", "brief"); err == nil || !strings.Contains(err.Error(), "hk skills enable brief") {
		t.Fatalf("loading a skill that is off: %v", err)
	}
	// The files are still there, and a fresh library reads the same state.
	if _, err := os.Stat(filepath.Join(l.AgentDir("habakkuk"), "deploy", FileName)); err != nil {
		t.Fatal("turning a skill off removed it")
	}
	again := &Library{Home: l.Home, Bundled: l.Bundled}
	if where, off := again.IsOff("deploy"); !off || where != "command" {
		t.Fatalf("state not kept: %q %v", where, off)
	}
	all, _ := l.Inventory("habakkuk")
	var reasons []string
	for _, e := range all {
		if !e.Usable() {
			reasons = append(reasons, e.Name+": "+e.Reason())
		}
	}
	if len(reasons) != 3 || !strings.Contains(reasons[0], "turned off") {
		t.Fatalf("inventory reasons %v", reasons)
	}
	for _, name := range []string{"brief", "deploy", "weekly-report"} {
		if err := l.SetOff(name, false); err != nil {
			t.Fatal(err)
		}
	}
	if got := names(); got != "brief deploy inbox-zero weekly-report" {
		t.Fatalf("after turning them on: %s", got)
	}
	// A name turned off in config.yaml stays off, and enable says where.
	l.Off = []string{"brief"}
	if got := names(); got != "deploy inbox-zero weekly-report" {
		t.Fatalf("config list ignored: %s", got)
	}
	if err := l.SetOff("brief", false); err == nil || !strings.Contains(err.Error(), "config.yaml") {
		t.Fatalf("enable of a name the config turns off: %v", err)
	}
	if err := l.SetOff("Bad Name", true); err == nil {
		t.Fatal("invalid name accepted")
	}
}

// Saving a skill whose name is a command says that /NAME will not run it.
func TestSkillSaveWarnsAboutACommandName(t *testing.T) {
	l := &Library{Home: t.TempDir(), CommandTaken: func(name string) bool { return name == "stop" }}
	r := tools.NewRegistry()
	RegisterTools(r, l)
	env := &tools.Env{Agent: "habakkuk", Workspace: t.TempDir(), Mode: tools.ModeWorkspace}
	save := func(name string) string {
		in, _ := json.Marshal(map[string]string{"name": name, "description": "A skill.", "body": "Steps."})
		res := r.Execute(context.Background(), env, llm.Block{Type: llm.ToolUse, ToolUseID: "t", ToolName: "skill_save", Input: in})
		if res.IsError {
			t.Fatalf("skill_save %s: %s", name, res.Text)
		}
		return res.Text
	}
	if out := save("stop"); !strings.Contains(out, "created skill stop") || !strings.Contains(out, "/skill stop") {
		t.Fatalf("skill_save stop: %q", out)
	}
	if out := save("stop"); !strings.Contains(out, "updated skill stop") || !strings.Contains(out, "/skill stop") {
		t.Fatalf("skill_save stop again: %q", out)
	}
	if out := save("report"); strings.Contains(out, "Note:") {
		t.Fatalf("skill_save report: %q", out)
	}
}

// A skill that needs a program or an environment variable the machine does
// not have is not offered, and says what is missing.
func TestRequirementsHideASkill(t *testing.T) {
	l := &Library{Home: t.TempDir()}
	write(t, filepath.Join(l.SharedDir(), "tracker", FileName),
		"---\nname: tracker\ndescription: Work with the tracker.\nrequires:\n  bins: [trackerctl, git]\n  env: TRACKER_TOKEN\n---\n\nSteps.\n")
	write(t, filepath.Join(l.SharedDir(), "elsewhere", FileName),
		"---\nname: elsewhere\ndescription: Only on another system.\nrequires: {os: [plan9]}\n---\n\nSteps.\n")
	write(t, filepath.Join(l.SharedDir(), "plain", FileName), skillMD("plain", "Needs nothing.", "Steps."))
	have := map[string]bool{"git": true}
	l.LookPath = func(name string) bool { return have[name] }
	l.Env = func(name string) bool { return have[name] }

	list, _ := l.List("habakkuk")
	if len(list) != 1 || list[0].Name != "plain" {
		t.Fatalf("list %+v", list)
	}
	_, err := l.Get("habakkuk", "tracker")
	if err == nil || !strings.Contains(err.Error(), "trackerctl") || !strings.Contains(err.Error(), "TRACKER_TOKEN") || strings.Contains(err.Error(), "git") {
		t.Fatalf("missing requirements: %v", err)
	}
	all, _ := l.Inventory("habakkuk")
	if len(all) != 3 || all[0].Name != "elsewhere" || !strings.Contains(all[0].Reason(), "plan9") || len(all[2].Missing) != 2 {
		t.Fatalf("inventory %+v", all)
	}
	// Once the program and the variable are there, the skill is offered.
	have["trackerctl"], have["TRACKER_TOKEN"] = true, true
	if s, err := l.Get("habakkuk", "tracker"); err != nil || len(s.Requires.Bins) != 2 || len(s.Requires.Env) != 1 {
		t.Fatalf("requirements met: %+v %v", s, err)
	}
	// An agent rewriting its own skill keeps what the skill requires.
	write(t, filepath.Join(l.AgentDir("habakkuk"), "mine", FileName),
		"---\nname: mine\ndescription: Old.\nrequires:\n  bins: [git]\n---\n\nOld steps.\n")
	if s, _, err := l.Save("habakkuk", "mine", "New.", "New steps."); err != nil || len(s.Requires.Bins) != 1 {
		t.Fatalf("save: %+v %v", s, err)
	}
	data, _ := os.ReadFile(filepath.Join(l.AgentDir("habakkuk"), "mine", FileName))
	if !strings.Contains(string(data), "requires:") || !strings.Contains(string(data), "git") {
		t.Fatalf("requires lost on save:\n%s", data)
	}
	// A skill that requires nothing is written without the block.
	l.Save("habakkuk", "simple", "Simple.", "Steps.")
	if data, _ := os.ReadFile(filepath.Join(l.AgentDir("habakkuk"), "simple", FileName)); strings.Contains(string(data), "requires") {
		t.Fatalf("empty requires written:\n%s", data)
	}
}
