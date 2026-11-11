package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
)

// Every spelling of a quoted or escaped leading ~ or = gets no rule, empty
// quotes in front included; only an unquoted leading ~/ is the home folder.
func TestQuotedTildeAndEqualsEverySpelling(t *testing.T) {
	for _, c := range []string{
		`ls '~/x'`, `ls "~/x"`, `ls \~/x`, `ls '~'/x`, `ls "~"/x`, `'~/bin/t'`, `"~/bin/t"`, `\~/bin/t`,
		`ls =ls`, `ls '=ls'`, `ls "=ls"`, `ls \=ls`, `=ls`, `ls ~`, `ls ~user/x`, `ls a=~/x`, `ls --p=~/x`,
		`ls ''~/x`, `ls ""~/x`,
		`ls ''"~/x"`, `ls ""'~/x'`, `ls ''\~/x`, `''"~/bin/t"`, `ls ''"=ls"`, `ls ""\=ls`, `ls ''=ls`,
	} {
		if w, ok := simpleCommand(c); ok {
			t.Errorf("simple: %q -> %q", c, w)
		}
	}
	if !sameLine("cat ~/x", "cat ~/x") {
		t.Fatal("the same line")
	}
	for _, c := range []string{`cat ''"~/x"`, `cat ""'~/x'`, `cat ''\~/x`} {
		if sameLine("cat ~/x", c) {
			t.Errorf("rule `cat ~/x` (the home folder) matches %q (a folder named ~ here)", c)
		}
	}
}

// A program written ”"~/bin/t" runs ./~/bin/t: no rule is offered for it.
func TestQuotedTildeProgramGetsNoRule(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	_, env, _ := ruleRig(t, &deciding{})
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, "bin"), 0o755)
	os.WriteFile(filepath.Join(home, "bin", "t"), []byte("#!/bin/sh\necho home\n"), 0o755)
	os.MkdirAll(filepath.Join(env.Workspace, "~", "bin"), 0o755)
	os.WriteFile(filepath.Join(env.Workspace, "~", "bin", "t"), []byte("#!/bin/sh\necho local\n"), 0o755)
	if p := ProposeRule(env, execTool{}, call("exec", map[string]any{"command": `''"~/bin/t"`})); p != nil {
		t.Fatalf("offered %+v", p)
	}
}

// Habakkuk's home: its files are guarded, by name and through links, and
// no rule is offered at it, under it or through a link to it.
func TestHomeGuardsEverywhere(t *testing.T) {
	ap := &deciding{d: DecisionAlways}
	r, env, rf := ruleRig(t, ap)
	home := t.TempDir()
	env.Policy = &Policy{Place: PlaceLocal, Home: home}
	for _, n := range []string{"config.yaml", ".env", "state.db", "control.db-wal", "Config.YAML", ".ENV.local", "usage.db-shm"} {
		p := filepath.Join(home, n)
		os.WriteFile(p, []byte("x"), 0o600)
		if guardedPath(env, p) == "" {
			t.Errorf("not guarded: %s", n)
		}
	}
	for _, n := range []string{"approvals.json", "APPROVALS.JSON.lock", "approvals.json.bak"} {
		if guardedPath(env, filepath.Join(env.Workspace, n)) == "" {
			t.Errorf("not guarded: %s", n)
		}
	}
	for _, mode := range []Mode{ModeWorkspace, ModeFull} {
		env.Mode = mode
		ap.d, ap.asks = DecisionDeny, 0
		r.Execute(context.Background(), env, call("write_file", map[string]any{"path": filepath.Join(home, "config.yaml"), "content": "y"}))
		if ap.asks == 0 {
			t.Errorf("%s: config.yaml write did not ask", mode)
		}
		link := filepath.Join(env.Workspace, "cfg-"+string(mode))
		os.Symlink(filepath.Join(home, ".env"), link)
		ap.asks = 0
		r.Execute(context.Background(), env, call("write_file", map[string]any{"path": link, "content": "y"}))
		if ap.asks == 0 {
			t.Errorf("%s: .env through a symlink did not ask", mode)
		}
	}
	env.Mode = ModeWorkspace
	sub := filepath.Join(home, "notes")
	os.MkdirAll(sub, 0o755)
	os.Symlink(home, filepath.Join(env.Workspace, "hk"))
	for _, c := range []map[string]any{
		{"command": "ls", "cwd": home}, {"command": "ls", "cwd": sub}, {"command": "ls", "cwd": filepath.Join(env.Workspace, "hk")},
		{"command": "ls " + filepath.Join(home, "new.txt")}, {"command": "cat hk/config.yaml"},
	} {
		if p := ProposeRule(env, execTool{}, call("exec", c)); p != nil {
			t.Errorf("rule offered: %v -> %+v", c, p)
		}
	}
	for _, p := range []string{filepath.Join(sub, "a.md"), filepath.Join(env.Workspace, "hk", "notes", "a.md")} {
		if pr := ProposeRule(env, writeFile{}, call("write_file", map[string]any{"path": p, "content": "x"})); pr != nil {
			t.Errorf("file rule offered under home: %s -> %+v", p, pr)
		}
	}
	if saved, _ := rf.Saved(); len(saved) != 0 {
		t.Errorf("saved %+v", saved)
	}
}

// No rule is offered for a program that can't be resolved.
func TestUnresolvedProgramGetsNoRule(t *testing.T) {
	_, env, _ := ruleRig(t, &deciding{})
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, c := range []string{"no-such-program-xyz", "./missing.sh", "~/bin/missing", "/nonexistent/bin/x"} {
		if p := ProposeRule(env, execTool{}, call("exec", map[string]any{"command": c})); p != nil {
			t.Errorf("offered for unresolved %q: %+v", c, p)
		}
	}
}

// Exec rules only where commands run on this machine in bash, sh or dash:
// not on a Windows host, and not when HABAKKUK_SHELL names zsh, pwsh, fish
// or another program. Config exec rules don't apply there either.
func TestExecRulesOnlyUnderBashShOrDash(t *testing.T) {
	check := func(name string) {
		t.Helper()
		ap := &deciding{d: DecisionDeny}
		r, env, rf := ruleRig(t, ap, Rule{Tool: "exec", Exact: "echo hi"})
		if p := ProposeRule(env, execTool{}, call("exec", map[string]any{"command": "echo hi"})); p != nil {
			t.Errorf("%s: offered %+v", name, p)
		}
		r.Execute(context.Background(), env, call("exec", map[string]any{"command": "echo hi"}))
		if ap.asks == 0 {
			t.Errorf("%s: a config exec rule applied", name)
		}
		if saved, _ := rf.Saved(); len(saved) != 0 {
			t.Errorf("%s: saved %+v", name, saved)
		}
	}
	windowsHost = true
	check("windows host")
	windowsHost = false
	stub := shellName
	defer func() { shellName = stub }()
	if runtime.GOOS != "windows" {
		shellName = platform.ScriptShellName // the real one, reading HABAKKUK_SHELL
		for _, sh := range []string{"zsh", "pwsh", "/bin/zsh", "/opt/homebrew/bin/fish", "/usr/local/bin/my-wrapper"} {
			t.Setenv("HABAKKUK_SHELL", sh)
			check("HABAKKUK_SHELL=" + sh)
		}
	}
	for _, sh := range []string{"bash", "sh", "dash"} {
		shellName = func() string { return sh }
		if !posixShell(&Env{}) {
			t.Errorf("%s: no exec rules", sh)
		}
	}
	for _, sh := range []string{"zsh", "PowerShell", "fish"} {
		shellName = func() string { return sh }
		if posixShell(&Env{}) {
			t.Errorf("%s: exec rules", sh)
		}
	}
}

// Quoted or escaped ~ and = after any number of empty quotes, or as a word
// of their own, are refused; ~ or = later in a word are not expansions.
func TestQuotedTildeAndEqualsAfterEmptyQuotes(t *testing.T) {
	for _, c := range []string{
		`cat ''"~/x"`, `cat ""'~/x'`, `cat ''\~/x`, `cat ""\~/x`, `cat ''~/x`, `cat ""~/x`,
		`cat ''''"~"`, `cat """"'~'`, `cat '''~'`, `cat """~"`,
		`cat ''=ls`, `cat ""=ls`, `cat ''"=ls"`, `cat ""'=ls'`, `cat ''\=ls`, `cat ""\=ls`,
		`cat '~/x'`, `cat "~/x"`, `cat \~/x`, `cat '=ls'`, `cat "=ls"`, `cat \=ls`,
		`""=ls`, `''"~/bin/ls"`, `"~/bin/ls"`, `\~/bin/ls`,
		`cat a ''"~"`, `cat '' "~"`,
	} {
		if w, ok := shellWords(c); ok {
			t.Errorf("%s splits as %q", c, w)
		}
	}
	for _, c := range []string{`cat ~/x`, `cat a'~'`, `cat "a=b"`, `cat a\=b`, `cat x'='`} {
		if _, ok := shellWords(c); !ok {
			t.Errorf("%s refused", c)
		}
	}
}
