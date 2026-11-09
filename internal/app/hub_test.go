package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
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
