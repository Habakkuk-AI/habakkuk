package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// openTestAppIn is openTestApp in a home prepared beforehand.
func openTestAppIn(t *testing.T, home, extra string) (*App, string, *syncBuf) {
	t.Helper()
	cfg := strings.Replace(config.Template, "workspace: ~/habakkuk-workspace", "workspace: "+filepath.Join(home, "ws"), 1) + extra
	if err := store.WriteFileAtomic(filepath.Join(home, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	a, err := Open(context.Background(), home, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a, home, logs
}

func TestHubCommands(t *testing.T) {
	home := t.TempDir()
	// An enabled plugin with a prompt command, a script command and one
	// whose name is taken by a built-in.
	dir := filepath.Join(home, "plugins", "notes")
	os.MkdirAll(filepath.Join(dir, "skills", "triage"), 0o755)
	hello := `echo "hi $1"`
	if runtime.GOOS == "windows" { // PowerShell: arguments come in $env:HABAKKUK_ARGS
		hello = `Write-Output "hi $env:HABAKKUK_ARGS"`
	}
	os.WriteFile(filepath.Join(dir, "habakkuk-plugin.yaml"), []byte(`name: notes
commands:
  standup: {description: Write my standup, prompt: "Standup for {{args}}"}
  hello: {run: '`+hello+`'}
  retry: {prompt: "shadow"}
`), 0o644)

	os.WriteFile(filepath.Join(dir, "skills", "triage", "SKILL.md"), []byte("---\nname: triage\ndescription: Triage notes.\n---\n\nSteps.\n"), 0o644)
	os.WriteFile(filepath.Join(home, "plugins", ".lock.json"), []byte(`{"version":1,"packages":{"notes":{"source":"x","sha256":"y","enabled":true,"trusted":true}}}`), 0o600)
	os.MkdirAll(filepath.Join(home, "agents", "habakkuk", "skills", "report"), 0o755)
	os.WriteFile(filepath.Join(home, "agents", "habakkuk", "skills", "report", "SKILL.md"), []byte("---\nname: report\ndescription: Weekly report.\n---\n\nWrite it.\n"), 0o644)

	a, _, logs := openTestAppIn(t, home, "")
	ctx := context.Background()
	sess, err := a.Runner.NewSession(ctx, "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	cmds := a.ChatCommands()
	for _, name := range []string{"/skills", "/skill", "/learn", "/standup", "/hello", "/usage", "/model"} {
		if _, ok := cmds[name]; !ok {
			t.Errorf("missing %s", name)
		}
	}
	if _, ok := cmds["/retry"]; ok {
		t.Error("a plugin replaced a built-in command")
	}
	if !strings.Contains(logs.String(), "plugin command left out") {
		t.Error("no warning for the shadowing command")
	}
	list := cmds["/skills"].Run(ctx, sess.ID, "")
	if !strings.Contains(list, "report (mine)") || !strings.Contains(list, "triage (plugin)") {
		t.Fatalf("/skills:\n%s", list)
	}
	text, err := cmds["/skill"].Prompt(ctx, sess.ID, "report for this week")
	if err != nil || !strings.Contains(text, "Write it.") || !strings.Contains(text, "Request: for this week") {
		t.Fatalf("/skill: %v\n%s", err, text)
	}
	if _, err := cmds["/skill"].Prompt(ctx, sess.ID, "nope"); err == nil {
		t.Fatal("unknown skill")
	}
	if text, err := cmds["/learn"].Prompt(ctx, sess.ID, "https://example.com/guide"); err != nil || !strings.Contains(text, "skill_save") {
		t.Fatalf("/learn: %v %s", err, text)
	}
	if text, _ := cmds["/standup"].Prompt(ctx, sess.ID, "Monday"); !strings.Contains(text, "Standup for Monday") {
		t.Fatalf("/standup: %s", text)
	}
	if out := cmds["/hello"].Run(ctx, sess.ID, "there"); out != "hi there" || !cmds["/hello"].Async {
		t.Fatalf("/hello: %q", out)
	}
}

// Every skill an agent can use is a slash command of its own, the skills
// that ship with the program are there on a new install, and a skill that
// is turned off is neither listed nor a command.
func TestSkillsAsCommandsAndBundledSkills(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, "agents", "habakkuk", "skills", "weekly-report"), 0o755)
	os.WriteFile(filepath.Join(home, "agents", "habakkuk", "skills", "weekly-report", "SKILL.md"), []byte("---\nname: weekly-report\ndescription: Weekly report.\n---\n\nWrite it.\n"), 0o644)
	// The agent's own skill with the name of a bundled one, and one named like a command.
	os.MkdirAll(filepath.Join(home, "agents", "habakkuk", "skills", "summarize"), 0o755)
	os.WriteFile(filepath.Join(home, "agents", "habakkuk", "skills", "summarize", "SKILL.md"), []byte("---\nname: summarize\ndescription: My way.\n---\n\nMy summary steps.\n"), 0o644)
	os.MkdirAll(filepath.Join(home, "skills", "usage"), 0o755)
	os.WriteFile(filepath.Join(home, "skills", "usage", "SKILL.md"), []byte("---\nname: usage\ndescription: Shadowed.\n---\n\nNever a command.\n"), 0o644)

	a, _, _ := openTestAppIn(t, home, "\nskills:\n  disabled: [brainstorm]\n")
	ctx := context.Background()
	sess, err := a.Runner.NewSession(ctx, "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	run := func(name, arg string) string {
		t.Helper()
		cmd, ok := a.ChatCommand("habakkuk", name)
		if !ok || cmd.Prompt == nil {
			t.Fatalf("%s is not a skill command", name)
		}
		text, err := cmd.Prompt(ctx, sess.ID, arg)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return text
	}
	if text := run("/weekly-report", "for last week"); !strings.Contains(text, "Write it.") || !strings.Contains(text, "Request: for last week") {
		t.Fatalf("/weekly-report:\n%s", text)
	}
	// An underscore stands for the hyphen, and case does not matter.
	if text := run("/Weekly_Report", ""); !strings.Contains(text, "Write it.") {
		t.Fatalf("/Weekly_Report:\n%s", text)
	}
	// A bundled skill is a command too, and the agent's own copy wins.
	if text := run("/daily-brief", ""); !strings.Contains(text, "[Skill daily-brief, invoked by the user]") {
		t.Fatalf("/daily-brief:\n%s", text)
	}
	if text := run("/summarize", "this"); !strings.Contains(text, "My summary steps.") {
		t.Fatalf("/summarize did not use the agent's own skill:\n%s", text)
	}
	// A real command is never replaced by a skill of the same name.
	if cmd, ok := a.ChatCommand("habakkuk", "/usage"); !ok || cmd.Prompt != nil {
		t.Fatal("the skill named usage took the /usage command")
	}
	for _, name := range []string{"/brainstorm", "/no-such-skill", "/Bad Name", "weekly-report", "/etc/hosts"} {
		if _, ok := a.SkillCommand("habakkuk", name); ok {
			t.Errorf("%s is a command", name)
		}
	}
	list := a.ChatCommands()["/skills"].Run(ctx, sess.ID, "")
	for _, want := range []string{"- daily-brief:", "- weekly-report (mine):", "- summarize (mine): My way.", "/<name> [request]"} {
		if !strings.Contains(list, want) {
			t.Errorf("/skills lacks %q:\n%s", want, list)
		}
	}
	if strings.Contains(list, "brainstorm") {
		t.Errorf("/skills lists a skill that is turned off:\n%s", list)
	}
	// The session's prompt lists the user's skills before the bundled ones.
	snap := a.snapshot(ctx, a.Runner.Agents["habakkuk"])
	var names []string
	for _, s := range snap.SkillSet {
		names = append(names, s.Name)
	}
	if len(names) < 20 || !slices.Contains(names, "daily-brief") || slices.Contains(names, "brainstorm") {
		t.Fatalf("snapshot skills %v", names)
	}

	// skills.bundled: false leaves only the user's own.
	home2 := t.TempDir()
	b, _, _ := openTestAppIn(t, home2, "\nskills:\n  bundled: false\n")
	if list, _ := b.Skills.List("habakkuk"); len(list) != 0 {
		t.Fatalf("bundled: false left %d skills", len(list))
	}
	if _, ok := b.SkillCommand("habakkuk", "/daily-brief"); ok {
		t.Fatal("bundled: false left /daily-brief")
	}
}

// A skill named like a command never becomes that command: not in a chat
// app, where the gateway knows its own commands, and not in hk chat or the
// terminal UI, which ask the app. It still runs with /skill NAME, and the
// places that create or list skills say so.
func TestSkillsNeverTakeACommandName(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{"stop", "remember", "undo-memory", "usage", "clear", "standup", "report"} {
		dir := filepath.Join(home, "agents", "habakkuk", "skills", name)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: A skill named "+name+".\n---\n\nSteps of "+name+".\n"), 0o644)
	}
	// An enabled plugin with a command named standup.
	plugin := filepath.Join(home, "plugins", "notes")
	os.MkdirAll(plugin, 0o755)
	os.WriteFile(filepath.Join(plugin, "habakkuk-plugin.yaml"), []byte("name: notes\ncommands:\n  standup: {prompt: \"Standup for {{args}}\"}\n"), 0o644)
	os.WriteFile(filepath.Join(home, "plugins", ".lock.json"), []byte(`{"version":1,"packages":{"notes":{"source":"x","sha256":"y","enabled":true,"trusted":true}}}`), 0o600)

	a, _, _ := openTestAppIn(t, home, "")
	ctx := context.Background()
	// Every command the app adds counts as taken, so a new one cannot be
	// left out of the list a skill's name is checked against.
	for name := range a.ChatCommands() {
		if !CommandTaken(a.Cfg, strings.TrimPrefix(name, "/")) {
			t.Errorf("a skill named %s would take the app's %s; add it to appCommands", strings.TrimPrefix(name, "/"), name)
		}
	}
	sess, err := a.Runner.NewSession(ctx, "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	// The gateway's own (/stop, /remember, /undo-memory with an underscore
	// for the hyphen), the app's (/usage), the terminal's (/clear) and a
	// plugin's (/standup).
	for _, name := range []string{"/stop", "/remember", "/REMEMBER", "/undo-memory", "/undo_memory", "/usage", "/clear", "/standup"} {
		if _, ok := a.SkillCommand("habakkuk", name); ok {
			t.Errorf("the skill behind %s took a command's name", name)
		}
	}
	// hk chat and the terminal UI ask ChatCommand: only real commands come back for those names.
	for _, name := range []string{"/stop", "/remember", "/undo_memory", "/clear"} {
		if _, ok := a.ChatCommand("habakkuk", name); ok {
			t.Errorf("ChatCommand answered %s with a skill", name)
		}
	}
	if cmd, ok := a.ChatCommand("habakkuk", "/standup"); !ok {
		t.Error("the plugin's /standup is gone")
	} else if text, _ := cmd.Prompt(ctx, sess.ID, "Monday"); !strings.Contains(text, "Standup for Monday") {
		t.Errorf("/standup ran the skill, not the plugin's command: %s", text)
	}
	// A skill with a free name is still a command, and the others run with /skill.
	if _, ok := a.SkillCommand("habakkuk", "/report"); !ok {
		t.Error("/report is not a command")
	}
	text, err := a.ChatCommands()["/skill"].Prompt(ctx, sess.ID, "stop now")
	if err != nil || !strings.Contains(text, "Steps of stop.") {
		t.Fatalf("/skill stop: %v\n%s", err, text)
	}
	// /skills says which ones need /skill, and the library knows for skill_save and hk skills.
	list := a.ChatCommands()["/skills"].Run(ctx, sess.ID, "")
	if !strings.Contains(list, "use /skill <name>") || !strings.Contains(list, "remember") || !strings.Contains(list, "stop") {
		t.Fatalf("/skills does not say which names are commands:\n%s", list)
	}
	if why := a.Skills.NoCommand("remember"); !strings.Contains(why, "/skill remember") {
		t.Fatalf("NoCommand: %q", why)
	}
	if why := a.Skills.NoCommand("report"); why != "" {
		t.Fatalf("NoCommand for a free name: %q", why)
	}
	// The fixed list of the app's own commands matches what ChatCommands builds.
	for name := range a.ChatCommands() {
		if name != "/standup" && !slices.Contains(appCommands, name) {
			t.Errorf("%s is a chat command but not in appCommands", name)
		}
	}
	for _, name := range appCommands {
		if _, ok := a.ChatCommands()[name]; !ok {
			t.Errorf("%s is in appCommands but not a chat command", name)
		}
	}
}

func TestCuratorRunsFromMaintain(t *testing.T) {
	home := t.TempDir()
	skill := filepath.Join(home, "agents", "habakkuk", "skills", "stale")
	os.MkdirAll(skill, 0o755)
	os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: stale\ndescription: Old.\n---\n\nx\n"), 0o644)
	old := time.Now().Add(-400 * 24 * time.Hour)
	os.Chtimes(filepath.Join(skill, "SKILL.md"), old, old)
	a, _, _ := openTestAppIn(t, home, "\nskills:\n  curate: weekly\n")
	if err := a.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(skill); !os.IsNotExist(err) {
		t.Fatal("stale skill not archived")
	}
	if a.Skills.CurateDue(a.Cfg.CurateEvery(), time.Now()) {
		t.Fatal("curator run not recorded")
	}
}
