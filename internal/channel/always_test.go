package channel

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// withRules gives the rig's agent a rules file and the app's turn policy
// in small: a chat turn from the owner stands in its chat for rules, a
// guest's stands nowhere.
func withRules() (func(*rig), func() *tools.RuleFile) {
	var rf *tools.RuleFile
	return func(r *rig) {
			rf = &tools.RuleFile{Path: filepath.Join(r.dir, "agent", tools.RulesFileName)}
			r.g.Runner.Env = func(s *agent.Spec, _ string) *tools.Env {
				return &tools.Env{Agent: s.Name, Workspace: s.Workspace, Mode: s.Mode, Rules: rf, Taint: tools.NewTaint(false)}
			}
			r.g.Runner.Policy = func(_ context.Context, _ *agent.Spec, origin json.RawMessage, env *tools.Env) {
				var to Address
				if json.Unmarshal(origin, &to) != nil || to.Account == "" {
					return
				}
				env.Policy = &tools.Policy{Channel: to.Account, Guest: to.Trust == TrustGuest}
				if !env.Policy.Guest {
					env.Policy.Place = tools.ChatPlace(to.Account, to.Conversation)
				}
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

func richRulesRig(t *testing.T, ch *config.Channel) (*rig, *rich, func() *tools.RuleFile) {
	t.Helper()
	prep, rules := withRules()
	r := openStateRig(t, t.TempDir(), ch, prep)
	a := &rich{recorder: r.a}
	r.g.Adapters["tg"] = a
	return r, a, rules
}

func TestAlwaysAllowButton(t *testing.T) {
	r, a, rules := richRulesRig(t, &config.Channel{AllowFrom: []string{"7"}})
	send(r, "m1", "please run it")
	waitFor(t, "buttons", func() bool { return a.lastRows() != nil })
	rows := a.lastRows()
	if len(rows) != 1 || len(rows[0]) != 3 || rows[0][0].Text != "Allow once" || rows[0][1].Text != "Always allow" ||
		rows[0][2].Text != "Deny" || rows[0][2].Style != ButtonDanger {
		t.Fatalf("rows = %+v", rows)
	}
	prompts, _, _ := a.snapshot()
	want := "**Always allow** saves a rule for this chat. I won't ask again before running exactly this command in your workspace or a folder inside it:\n```\necho approved-run\n```"
	if !strings.Contains(prompts[0], want) {
		t.Fatalf("prompt does not show the rule:\n%s", prompts[0])
	}
	press(r, "7", rows[0][1].Data)
	waitFor(t, "tool output", hasText(r.a, "tool said: approved-run"))
	waitFor(t, "ack", hasText(r.a, "Approved. I saved a rule for this chat and won't ask again for this."))
	saved := savedRules(t, rules())
	if len(saved) != 1 || saved[0].Command+saved[0].Exact != "echo approved-run" || saved[0].Place != tools.ChatPlace("tg", "100") ||
		saved[0].SavedBy != "7" || saved[0].Source != "chat" {
		t.Fatalf("saved %+v", saved)
	}
	// The next time, no prompt: the rule answers.
	send(r, "m2", "run it again")
	waitFor(t, "second run", hasText(r.a, "saved approval rule"))
	if prompts, _, _ := a.snapshot(); len(prompts) != 1 {
		t.Fatalf("asked again: %q", prompts)
	}
}

func TestAlwaysAllowTyped(t *testing.T) {
	prep, rules := withRules()
	r := openStateRig(t, t.TempDir(), &config.Channel{AllowFrom: []string{"7"}}, prep)
	send(r, "m1", "run it")
	waitFor(t, "prompt", hasText(r.a, "Reply **yes** to allow it once, **always** to allow it from now on, or **no**."))
	send(r, "m2", "Always")
	waitFor(t, "tool output", hasText(r.a, "tool said: approved-run"))
	if saved := savedRules(t, rules()); len(saved) != 1 {
		t.Fatalf("saved %+v\n%q", saved, r.a.texts())
	}
}

// A rule saved in one chat does not answer in another.
func TestRuleAppliesOnlyInItsChat(t *testing.T) {
	prep, rules := withRules()
	r := openStateRig(t, t.TempDir(), &config.Channel{AllowFrom: []string{"7"}}, prep)
	send(r, "m1", "run it")
	waitFor(t, "prompt", hasText(r.a, "**always**"))
	send(r, "m2", "always")
	waitFor(t, "tool output", hasText(r.a, "tool said: approved-run"))
	if len(savedRules(t, rules())) != 1 {
		t.Fatal("no rule saved")
	}
	other := dm("m3", "run it")
	other.Conversation = "200"
	r.g.Handle(context.Background(), other)
	waitFor(t, "prompt in the other chat", func() bool {
		return strings.Count(strings.Join(r.a.texts(), "\n"), "I'd like to run a command on your computer.") == 2
	})
}

// A turn that read outside content gets Approve and Deny only, and
// "always" saves nothing.
func TestAlwaysAllowIsNotOfferedToATaintedTurn(t *testing.T) {
	r, a, rules := richRulesRig(t, &config.Channel{AllowFrom: []string{"7"}})
	r.g.Runner.Env = func(s *agent.Spec, _ string) *tools.Env {
		return &tools.Env{Agent: s.Name, Workspace: s.Workspace, Mode: s.Mode, Rules: rules(), Taint: tools.NewTaint(true)}
	}
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

// A command with a backquote or a line break, or too long to read in a
// chat, gets no Always allow: what is shown must be what is saved.
func TestNoAlwaysForCommandsThatDoNotShowPlainly(t *testing.T) {
	for name, cmd := range map[string]string{
		"backquote":  "echo `id`",
		"line break": "echo one\necho two",
		"long":       "echo " + strings.Repeat("a", 320),
	} {
		t.Run(name, func(t *testing.T) {
			r, a, _ := richRulesRig(t, &config.Channel{AllowFrom: []string{"7"}})
			input, _ := json.Marshal(map[string]string{"command": cmd})
			r.g.Runner.Models = &stepModel{fn: func(_ llm.Request, last llm.Message) *llm.Response {
				for _, b := range last.Blocks {
					if b.Type == llm.ToolResult {
						return text("done")
					}
				}
				return toolCall("c1", "exec", string(input))
			}}
			send(r, "m1", "go")
			waitFor(t, "buttons", func() bool { return a.lastRows() != nil })
			prompts, _, _ := a.snapshot()
			if rows := a.lastRows(); len(rows[0]) != 2 || strings.Contains(prompts[0], "Always allow") {
				t.Fatalf("offered a rule:\n%s\n%+v", prompts[0], rows)
			}
			send(r, "m2", "no")
		})
	}
}

// An email conversation is never offered Always allow.
func TestNoAlwaysInEmail(t *testing.T) {
	prep, rules := withRules()
	r := openStateRig(t, t.TempDir(), &config.Channel{Type: "email", AllowFrom: []string{"7"}}, prep)
	send(r, "m1", "run it")
	waitFor(t, "prompt", hasText(r.a, "I'd like to run a command on your computer."))
	if hasText(r.a, "Always allow")() || hasText(r.a, "**always**")() {
		t.Fatalf("email offered a rule: %q", r.a.texts())
	}
	send(r, "m2", "always")
	waitFor(t, "refusal", hasText(r.a, "I can't save a rule for this one."))
	send(r, "m3", "no")
	waitFor(t, "denial", hasText(r.a, "tool said:"))
	if saved := savedRules(t, rules()); len(saved) != 0 {
		t.Fatalf("saved %+v", saved)
	}
}

// A press on an old prompt's button cannot answer the next prompt, nor
// save a rule for it.
func TestLatePressCannotAnswerTheNextPrompt(t *testing.T) {
	r, a, rules := richRulesRig(t, &config.Channel{AllowFrom: []string{"7"}})
	n := 0
	r.g.Runner.Models = &stepModel{fn: func(_ llm.Request, last llm.Message) *llm.Response {
		for _, b := range last.Blocks {
			if b.Type == llm.ToolResult {
				return text("tool said: " + b.Text)
			}
		}
		n++
		return toolCall("c", "exec", `{"command":"echo step`+string(rune('0'+n))+`"}`)
	}}
	send(r, "m1", "first")
	waitFor(t, "first prompt", func() bool { p, _, _ := a.snapshot(); return len(p) == 1 })
	old := a.lastRows()
	send(r, "m2", "yes")
	waitFor(t, "first ran", hasText(r.a, "tool said: step1"))
	send(r, "m3", "second")
	waitFor(t, "second prompt", func() bool { p, _, _ := a.snapshot(); return len(p) == 2 })
	press(r, "7", old[0][1].Data) // the first prompt's Always allow
	waitFor(t, "expired notice", hasText(r.a, "That request has expired. Please answer my latest request instead."))
	if saved := savedRules(t, rules()); len(saved) != 0 {
		t.Fatalf("a late press saved %+v", saved)
	}
	send(r, "m4", "no")
	waitFor(t, "second denied", hasText(r.a, "the user declined exec"))
}

// Only the owner answers: someone else in the chat can neither type
// "always" nor press Always allow.
func TestNonOwnerCannotSaveARule(t *testing.T) {
	r, a, rules := richRulesRig(t, &config.Channel{AllowFrom: []string{"7", "*"}})
	send(r, "m1", "run it")
	waitFor(t, "buttons", func() bool { return a.lastRows() != nil })
	press(r, "9", a.lastRows()[0][1].Data)
	other := dm("m2", "always")
	other.Sender = "9"
	r.g.Handle(context.Background(), other)
	time.Sleep(150 * time.Millisecond)
	if hasText(r.a, "tool said: approved-run")() || len(savedRules(t, rules())) != 0 {
		t.Fatal("someone other than the owner answered")
	}
	send(r, "m3", "no")
	waitFor(t, "denial", hasText(r.a, "the user declined exec"))
	if saved := savedRules(t, rules()); len(saved) != 0 {
		t.Fatalf("saved %+v", saved)
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
// the rig's chat. tainted is the schedule's mark (a turn steered by
// outside content created it).
func scheduledTurn(t *testing.T, r *rig, source, text string, tainted bool) {
	t.Helper()
	ctx := context.Background()
	sess, err := r.g.Runner.NewSession(ctx, "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := json.Marshal(Address{Account: "tg", Conversation: "100"})
	if _, err := r.g.Runner.SubmitTurn(ctx, agent.Turn{Session: sess.ID, Text: text, Origin: origin, Background: true,
		Source: source, Task: `scheduled task "Morning report"`, Tainted: tainted}); err != nil {
		t.Fatal(err)
	}
	r.g.Wake()
}

func TestScheduledRunAsksItsChat(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	scheduledTurn(t, r, agent.SourceSchedule, "run it", false)
	waitFor(t, "approval prompt", hasText(r.a, "I'd like to run a command on your computer."))
	waitFor(t, "task line", hasText(r.a, `This is for your scheduled task "Morning report".`))
	send(r, "m1", "yes")
	waitFor(t, "tool output", hasText(r.a, "tool said: approved-run"))
}

func TestScheduledRunDeniedAnswer(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	scheduledTurn(t, r, agent.SourceSchedule, "run it", false)
	waitFor(t, "approval prompt", hasText(r.a, "I'd like to run a command on your computer."))
	send(r, "m1", "no")
	waitFor(t, "denial result", hasText(r.a, "the user declined exec"))
}

// Other unattended work (a board card here) still has nobody to ask.
func TestOtherUnattendedRunsStillDeny(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	scheduledTurn(t, r, agent.SourceBoard, "run it", false)
	waitFor(t, "denial result", hasText(r.a, "no one is attending this run"))
	if hasText(r.a, "I'd like to run a command")() {
		t.Fatal("a board run asked in the chat")
	}
}

// A scheduled run's saved rule answers for later runs too: the schedule
// delivers to the chat the rule was saved in.
func TestScheduledRunUsesRules(t *testing.T) {
	prep, rules := withRules()
	r := openStateRig(t, t.TempDir(), &config.Channel{AllowFrom: []string{"7"}}, prep)
	scheduledTurn(t, r, agent.SourceSchedule, "run it", false)
	waitFor(t, "approval prompt", hasText(r.a, "**always**"))
	send(r, "m1", "always")
	waitFor(t, "tool output", hasText(r.a, "tool said: approved-run"))
	if saved := savedRules(t, rules()); len(saved) != 1 || saved[0].Place != tools.ChatPlace("tg", "100") {
		t.Fatalf("saved %+v", saved)
	}
	before := len(r.a.texts())
	scheduledTurn(t, r, agent.SourceSchedule, "run it", false)
	waitFor(t, "second run", func() bool {
		return strings.Contains(strings.Join(r.a.texts()[before:], "\n"), "saved approval rule")
	})
	if strings.Contains(strings.Join(r.a.texts()[before:], "\n"), "I'd like to run") {
		t.Fatal("the second run asked again")
	}
}

// A schedule created by a turn that read outside content runs tainted: it
// asks a person every time, and no rule answers for it.
func TestTaintedScheduledRunNeedsAPerson(t *testing.T) {
	prep, rules := withRules()
	r := openStateRig(t, t.TempDir(), &config.Channel{AllowFrom: []string{"7"}}, prep)
	if _, err := rules().Save(tools.Rule{Tool: "exec", Exact: "echo approved-run", Place: tools.ChatPlace("tg", "100")}); err != nil {
		t.Fatal(err)
	}
	scheduledTurn(t, r, agent.SourceSchedule, "run it", true)
	waitFor(t, "approval prompt", hasText(r.a, "this conversation included a web page or email"))
	if hasText(r.a, "**always**")() {
		t.Fatal("a tainted run was offered a rule")
	}
	send(r, "m1", "no")
	waitFor(t, "denial result", hasText(r.a, "the user declined exec"))
}
