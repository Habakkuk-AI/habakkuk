package channel

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// withRules gives the rig's agent a rules file, and taints every turn
// when tainted is set.
func withRules(tainted bool) (func(*rig), func() *tools.RuleFile) {
	var rf *tools.RuleFile
	return func(r *rig) {
			rf = &tools.RuleFile{Path: filepath.Join(r.dir, "agent", tools.RulesFileName)}
			r.g.Runner.Env = func(s *agent.Spec, _ string) *tools.Env {
				return &tools.Env{Agent: s.Name, Workspace: s.Workspace, Mode: s.Mode, Rules: rf, Taint: tools.NewTaint(tainted)}
			}
		}, func() *tools.RuleFile {
			return rf
		}
}

func savedRules(t *testing.T, rf *tools.RuleFile) []tools.Rule {
	t.Helper()
	saved, err := rf.Saved()
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func TestAlwaysAllowButton(t *testing.T) {
	prep, rules := withRules(false)
	r := openStateRig(t, t.TempDir(), &config.Channel{AllowFrom: []string{"7"}}, prep)
	a := &rich{recorder: r.a}
	r.g.Adapters["tg"] = a
	send(r, "m1", "please run it")
	waitFor(t, "buttons", func() bool { return a.lastRows() != nil })
	rows := a.lastRows()
	if len(rows) != 1 || len(rows[0]) != 3 || rows[0][0].Text != "Allow once" || rows[0][1].Text != "Always allow" ||
		rows[0][2].Text != "Deny" || rows[0][2].Style != ButtonDanger {
		t.Fatalf("rows = %+v", rows)
	}
	prompts, _, _ := a.snapshot()
	if !strings.Contains(prompts[0], "**Always allow** saves a rule, so I won't ask again for `echo approved-run` run in your workspace.") {
		t.Fatalf("prompt does not say what the rule covers:\n%s", prompts[0])
	}
	press(r, "7", rows[0][1].Data)
	waitFor(t, "tool output", hasText(r.a, "tool said: approved-run"))
	waitFor(t, "ack", hasText(r.a, "I saved a rule and won't ask again for `echo approved-run`"))
	saved := savedRules(t, rules())
	if len(saved) != 1 || saved[0].Command != "echo approved-run" || saved[0].Tool != "exec" {
		t.Fatalf("saved %+v", saved)
	}
	// The next time, no prompt: the rule answers.
	send(r, "m2", "run it again")
	waitFor(t, "second run", func() bool { return strings.Count(strings.Join(r.a.texts(), "\n"), "saved approval rule") > 0 })
	if prompts, _, _ := a.snapshot(); len(prompts) != 1 {
		t.Fatalf("asked again: %q", prompts)
	}
}

func TestAlwaysAllowTyped(t *testing.T) {
	prep, rules := withRules(false)
	r := openStateRig(t, t.TempDir(), &config.Channel{AllowFrom: []string{"7"}}, prep)
	send(r, "m1", "run it")
	waitFor(t, "prompt", hasText(r.a, "Reply **yes** to allow it once, **always** to allow it from now on, or **no**."))
	send(r, "m2", "Always")
	waitFor(t, "tool output", hasText(r.a, "tool said: approved-run"))
	if saved := savedRules(t, rules()); len(saved) != 1 {
		t.Fatalf("saved %+v\n%q", saved, r.a.texts())
	}
}

// A turn that read outside content gets Approve and Deny only, and
// "always" saves nothing.
func TestAlwaysAllowIsNotOfferedToATaintedTurn(t *testing.T) {
	prep, rules := withRules(true)
	r := openStateRig(t, t.TempDir(), &config.Channel{AllowFrom: []string{"7"}}, prep)
	a := &rich{recorder: r.a}
	r.g.Adapters["tg"] = a
	send(r, "m1", "run it")
	waitFor(t, "buttons", func() bool { return a.lastRows() != nil })
	if rows := a.lastRows(); len(rows[0]) != 2 || rows[0][0].Text != "Approve" {
		t.Fatalf("rows = %+v", rows)
	}
	if prompts, _, _ := a.snapshot(); strings.Contains(prompts[0], "Always allow") {
		t.Fatalf("a tainted turn was offered a rule:\n%s", prompts[0])
	}
	send(r, "m2", "always")
	waitFor(t, "refusal", hasText(r.a, "I can't save a rule for this one."))
	send(r, "m3", "yes")
	waitFor(t, "tool output", hasText(r.a, "tool said: approved-run"))
	if saved := savedRules(t, rules()); len(saved) != 0 {
		t.Fatalf("a tainted turn saved %+v", saved)
	}
}

// A button pressed on a prompt that had no Always allow (before a
// restart) does not answer one that has it.
func TestButtonDecision(t *testing.T) {
	cases := []struct {
		i      int
		always bool
		want   tools.Decision
		ok     bool
	}{
		{0, false, tools.DecisionOnce, true}, {1, false, tools.DecisionDeny, true}, {2, false, tools.DecisionDeny, false},
		{0, true, tools.DecisionOnce, true}, {1, true, tools.DecisionAlways, true}, {2, true, tools.DecisionDeny, true},
		{-1, true, tools.DecisionDeny, false}, {3, true, tools.DecisionDeny, false},
	}
	for _, c := range cases {
		if d, ok := buttonDecision(c.i, c.always); d != c.want || ok != c.ok {
			t.Errorf("buttonDecision(%d, %v) = %v, %v", c.i, c.always, d, ok)
		}
	}
}

// scheduledTurn queues a scheduled run in a fresh session that delivers to
// the rig's chat.
func scheduledTurn(t *testing.T, r *rig, source, text string) {
	t.Helper()
	ctx := context.Background()
	sess, err := r.g.Runner.NewSession(ctx, "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := json.Marshal(Address{Account: "tg", Conversation: "100"})
	if _, err := r.g.Runner.SubmitTurn(ctx, agent.Turn{Session: sess.ID, Text: text, Origin: origin, Background: true,
		Source: source, Task: `scheduled task "Morning report"`}); err != nil {
		t.Fatal(err)
	}
	r.g.Wake()
}

func TestScheduledRunAsksItsChat(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	scheduledTurn(t, r, agent.SourceSchedule, "run it")
	waitFor(t, "approval prompt", hasText(r.a, "I'd like to run a command on your computer."))
	waitFor(t, "task line", hasText(r.a, `This is for your scheduled task "Morning report".`))
	send(r, "m1", "yes")
	waitFor(t, "tool output", hasText(r.a, "tool said: approved-run"))
}

func TestScheduledRunDeniedAnswer(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	scheduledTurn(t, r, agent.SourceSchedule, "run it")
	waitFor(t, "approval prompt", hasText(r.a, "I'd like to run a command on your computer."))
	send(r, "m1", "no")
	waitFor(t, "denial result", hasText(r.a, "the user declined exec"))
}

// Other unattended work (a board card here) still has nobody to ask.
func TestOtherUnattendedRunsStillDeny(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	scheduledTurn(t, r, agent.SourceBoard, "run it")
	waitFor(t, "denial result", hasText(r.a, "no one is attending this run"))
	if hasText(r.a, "I'd like to run a command")() {
		t.Fatal("a board run asked in the chat")
	}
}

// A scheduled run's saved rule answers for later runs too.
func TestScheduledRunUsesRules(t *testing.T) {
	prep, rules := withRules(false)
	r := openStateRig(t, t.TempDir(), &config.Channel{AllowFrom: []string{"7"}}, prep)
	scheduledTurn(t, r, agent.SourceSchedule, "run it")
	waitFor(t, "approval prompt", hasText(r.a, "**always**"))
	send(r, "m1", "always")
	waitFor(t, "tool output", hasText(r.a, "tool said: approved-run"))
	if saved := savedRules(t, rules()); len(saved) != 1 {
		t.Fatalf("saved %+v", saved)
	}
	before := len(r.a.texts())
	scheduledTurn(t, r, agent.SourceSchedule, "run it")
	waitFor(t, "second run", func() bool {
		return strings.Contains(strings.Join(r.a.texts()[before:], "\n"), "saved approval rule")
	})
	if strings.Contains(strings.Join(r.a.texts()[before:], "\n"), "I'd like to run") {
		t.Fatal("the second run asked again")
	}
}

// A scheduled turn that read outside content still needs a person: it
// asks, and no rule answers for it.
func TestTaintedScheduledRunNeedsAPerson(t *testing.T) {
	prep, rules := withRules(true)
	r := openStateRig(t, t.TempDir(), &config.Channel{AllowFrom: []string{"7"}}, prep)
	rf := rules()
	if _, err := rf.Save(tools.Rule{Tool: "exec", Command: "echo approved-run"}); err != nil {
		t.Fatal(err)
	}
	scheduledTurn(t, r, agent.SourceSchedule, "run it")
	waitFor(t, "approval prompt", hasText(r.a, "this conversation included a web page or email"))
	send(r, "m1", "no")
	waitFor(t, "denial result", hasText(r.a, "the user declined exec"))
}
