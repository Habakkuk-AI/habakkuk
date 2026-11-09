package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func skillMD(name, desc, body string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n" + body + "\n"
}

// fixture: two shared skills, one overridden by the agent, plus junk folders.
func fixture(t *testing.T) *Library {
	t.Helper()
	l := &Library{Home: t.TempDir()}
	write(t, filepath.Join(l.SharedDir(), "weekly-report", FileName), skillMD("weekly-report", "Write the Habakkuk weekly report.", "Shared report steps."))
	write(t, filepath.Join(l.SharedDir(), "weekly-report", "templates", "report.md"), "# Week {{n}}")
	write(t, filepath.Join(l.SharedDir(), "deploy", FileName), skillMD("deploy", "Ship the app.", "Shared deploy steps."))
	write(t, filepath.Join(l.SharedDir(), "Bad_Name", FileName), skillMD("bad", "x", "x"))
	write(t, filepath.Join(l.SharedDir(), "no-skill-file", "README.md"), "nothing")
	write(t, filepath.Join(l.AgentDir("habakkuk"), "deploy", FileName), skillMD("ignored-name", "Ship the app, Habakkuk's way.", "Agent deploy steps."))
	write(t, filepath.Join(l.AgentDir("habakkuk"), "inbox-zero", FileName), "No frontmatter here, just steps.")
	return l
}

func TestDiscoveryAndPrecedence(t *testing.T) {
	l := fixture(t)
	list, err := l.List("habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range list {
		got = append(got, s.Name+"|"+s.Description+"|"+map[bool]string{true: "shared", false: "agent"}[s.Shared])
	}
	want := []string{
		"deploy|Ship the app, Habakkuk's way.|agent",
		"inbox-zero||agent",
		"weekly-report|Write the Habakkuk weekly report.|shared",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("list:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Another agent sees only the shared versions.
	ops, _ := l.List("ops")
	if len(ops) != 2 || ops[0].Name != "deploy" || !ops[0].Shared {
		t.Fatalf("ops list: %+v", ops)
	}
	_, body, _, err := l.View("habakkuk", "deploy")
	if err != nil || body != "Agent deploy steps." {
		t.Fatalf("habakkuk deploy body %q %v", body, err)
	}
	_, body, _, _ = l.View("ops", "deploy")
	if body != "Shared deploy steps." {
		t.Fatalf("ops deploy body %q", body)
	}
	s, body, files, err := l.View("habakkuk", "weekly-report")
	if err != nil || !s.Shared || body != "Shared report steps." || strings.Join(files, ",") != "templates/report.md" {
		t.Fatalf("view: %+v %q %v %v", s, body, files, err)
	}
	if _, _, _, err := l.View("habakkuk", "missing"); err == nil {
		t.Fatal("missing skill found")
	}
	// No skills at all is not an error.
	empty := &Library{Home: t.TempDir()}
	if list, err := empty.List("habakkuk"); err != nil || len(list) != 0 {
		t.Fatalf("empty home: %v %v", list, err)
	}
}

func TestSupportingFilesStayInsideTheSkill(t *testing.T) {
	l := fixture(t)
	got, err := l.ReadFile("habakkuk", "weekly-report", "templates/report.md")
	if err != nil || got != "# Week {{n}}" {
		t.Fatalf("read: %q %v", got, err)
	}
	secret := filepath.Join(l.Home, "config.yaml")
	write(t, secret, "keys_env: [SECRET]")
	dir := filepath.Join(l.SharedDir(), "weekly-report")
	os.Symlink(secret, filepath.Join(dir, "escape.md"))
	os.Symlink(l.Home, filepath.Join(dir, "home"))
	// A symlink that stays inside the folder is fine.
	aliased := os.Symlink(filepath.FromSlash("templates/report.md"), filepath.Join(dir, "alias.md")) == nil

	for _, p := range []string{
		"../../config.yaml",
		"templates/../../../config.yaml",
		"..",
		secret, // absolute
		"escape.md",
		"home/config.yaml",
		"",
		".",
		"templates", // a directory
	} {
		if out, err := l.ReadFile("habakkuk", "weekly-report", p); err == nil {
			t.Errorf("path %q was allowed: %q", p, out)
		}
	}
	if got, err := l.ReadFile("habakkuk", "weekly-report", "alias.md"); aliased && (err != nil || got != "# Week {{n}}") {
		t.Errorf("in-folder symlink: %q %v", got, err)
	}
	if _, err := l.ReadFile("habakkuk", "../skills/weekly-report", "templates/report.md"); err == nil {
		t.Error("bad skill name accepted")
	}
}

func TestSaveKeepsBackup(t *testing.T) {
	l := fixture(t)
	s, created, err := l.Save("habakkuk", "standup", "Run the daily standup.", "1. Ask for blockers.")
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, BackupName)); !os.IsNotExist(err) {
		t.Fatal("backup written on create")
	}
	first, _ := os.ReadFile(filepath.Join(s.Dir, FileName))
	_, created, err = l.Save("habakkuk", "standup", "Run the daily standup, briefly.", "1. Ask for blockers.\n2. Stop at 10 minutes.")
	if err != nil || created {
		t.Fatalf("update: %v %v", created, err)
	}
	bak, _ := os.ReadFile(filepath.Join(s.Dir, BackupName))
	if string(bak) != string(first) {
		t.Fatalf("backup = %q, want %q", bak, first)
	}
	_, body, _, _ := l.View("habakkuk", "standup")
	if !strings.Contains(body, "10 minutes") {
		t.Fatalf("body %q", body)
	}
	got, _ := l.Get("habakkuk", "standup")
	if got.Description != "Run the daily standup, briefly." {
		t.Fatalf("description %q", got.Description)
	}
	// Saving over a shared skill's name creates the agent's own copy and
	// leaves the shared one alone.
	if _, created, err := l.Save("habakkuk", "weekly-report", "Mine.", "My steps."); err != nil || !created {
		t.Fatalf("override: %v %v", created, err)
	}
	if _, body, _, _ := l.View("ops", "weekly-report"); body != "Shared report steps." {
		t.Fatalf("shared skill changed: %q", body)
	}
	for _, c := range []struct{ name, desc, body string }{
		{"Bad Name", "d", "b"},
		{"standup", "", "b"},
		{"standup", "d", "  "},
		{"standup", strings.Repeat("d", MaxDescription+1), "b"},
	} {
		if _, _, err := l.Save("habakkuk", c.name, c.desc, c.body); err == nil {
			t.Errorf("save %+v accepted", c)
		}
	}
	if _, _, err := l.Save("../evil", "x", "d", "b"); err == nil {
		t.Error("bad agent name accepted")
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"a", "deploy", "weekly-report-2", "0day", strings.Repeat("a", 64)} {
		if err := ValidName(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-lead", "Upper", "under_score", "dot.ted", "sp ace", "a/b", "..", strings.Repeat("a", 65)} {
		if ValidName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTools(t *testing.T) {
	ctx := context.Background()
	l := fixture(t)
	r := tools.NewRegistry()
	RegisterTools(r, l)
	env := &tools.Env{Agent: "habakkuk", Workspace: t.TempDir(), Mode: tools.ModeWorkspace}
	call := func(name string, in any) llm.Block {
		b, _ := json.Marshal(in)
		return r.Execute(ctx, env, llm.Block{Type: llm.ToolUse, ToolUseID: "t", ToolName: name, Input: b})
	}
	res := call("skill_view", map[string]any{"name": "weekly-report"})
	if res.IsError || !strings.Contains(res.Text, "Shared report steps.") || !strings.Contains(res.Text, "- templates/report.md") {
		t.Fatalf("view: %q", res.Text)
	}
	res = call("skill_file", map[string]any{"name": "weekly-report", "path": "templates/report.md"})
	if res.IsError || res.Text != "# Week {{n}}" {
		t.Fatalf("file: %q", res.Text)
	}
	if res := call("skill_file", map[string]any{"name": "weekly-report", "path": "../../config.yaml"}); !res.IsError {
		t.Fatalf("traversal: %q", res.Text)
	}
	// skill_save runs without approval in workspace mode.
	res = call("skill_save", map[string]any{"name": "triage", "description": "Sort the inbox.", "body": "Steps."})
	if res.IsError || !strings.Contains(res.Text, "created skill triage") {
		t.Fatalf("save: %q", res.Text)
	}
	res = call("skill_save", map[string]any{"name": "triage", "description": "Sort the inbox.", "body": "Better steps."})
	if res.IsError || !strings.Contains(res.Text, "SKILL.md.bak") {
		t.Fatalf("update: %q", res.Text)
	}
	env.Mode = tools.ModeReadOnly
	if res := call("skill_save", map[string]any{"name": "x", "description": "d", "body": "b"}); !strings.Contains(res.Text, "denied") {
		t.Fatalf("read-only save: %q", res.Text)
	}
}
