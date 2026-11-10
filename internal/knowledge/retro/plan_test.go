package retro

import (
	"strings"
	"testing"
	"time"
)

func testEvidence() *Evidence {
	return &Evidence{
		Agent:    "habakkuk",
		Problems: []JobIssue{{Ref: "job:turn_bad", Kind: "turn", What: "failed", Session: "ses_1"}},
		Stalled:  []Stalled{{Ref: "card:card_stuck", What: "card", Title: "Fix login", Status: "doing", Days: 10}},
	}
}

func TestParsePlanKeepsOnlyWhatHoldsUp(t *testing.T) {
	prev := &Previous{ID: "retro_prev", Actions: []ActionState{{N: 1, Action: Action{Kind: KindSkill, Target: "pr-follow-up"}}}}
	answer := "Here is my retro:\n```json\n" + `{
	  "summary": "Work stalled on the login fix.",
	  "went_well": [
	    {"what": "Answered quickly", "evidence": ["session:ses_1"]},
	    {"what": "Made up", "evidence": ["job:nope"]}
	  ],
	  "went_badly": [
	    {"what": "The login card sat for ten days", "cause": "No one followed up on the PR", "evidence": ["card:card_stuck", "[job:turn_bad]", "card:card_stuck"]},
	    {"what": "No evidence at all", "cause": "?", "evidence": []}
	  ],
	  "actions": [
	    {"kind": "skill", "target": "PR-Follow-Up", "description": "Check open PRs daily", "change": "List open PRs; ping reviewers.", "why": "PRs sat unreviewed", "expected": "No PR waits more than 2 days", "risk": "low", "evidence": ["card:card_stuck", "job:invented"]},
	    {"kind": "memory", "target": "Login fix status", "change": "PR #42 waits for review.", "why": "keep track"},
	    {"kind": "soul", "target": "anything", "change": "Always finish what you start.", "why": "unfinished work", "risk": "medium"},
	    {"kind": "config", "target": "agents.habakkuk.fallbacks", "change": "[p/other]", "why": "auth errors", "risk": "high"},
	    {"kind": "schedule", "target": "schedule:sch_backup", "change": "pause", "why": "fails daily", "risk": "low"},
	    {"kind": "ask", "target": "Reviewers", "change": "Who reviews PRs?", "why": "nobody does", "risk": "low"},
	    {"kind": "teleport", "target": "x", "change": "y", "why": "z"},
	    {"kind": "skill", "target": "bad name!", "description": "d", "change": "c", "why": "w"},
	    {"kind": "skill", "target": "no-description", "change": "c", "why": "w"},
	    {"kind": "memory", "target": "Keys", "change": "the key is sk-ant-abcdefghijklmnopqrstuvwxyz0123", "why": "w"},
	    {"kind": "memory", "target": "Empty", "change": "", "why": "w"},
	    {"kind": "memory", "target": "No reason", "change": "text"},
	    {"kind": "ask", "target": "Risky", "change": "q", "why": "w", "risk": "extreme"}
	  ],
	  "follow_up": [
	    {"retro": "retro_prev", "action": 1, "verdict": "Did not help", "note": "PRs still sit", "evidence": ["card:card_stuck"]},
	    {"retro": "retro_prev", "action": 7, "verdict": "helped"},
	    {"retro": "retro_other", "action": 1, "verdict": "helped"},
	    {"retro": "retro_prev", "action": 1, "verdict": "great"}
	  ]
	}` + "\n```\nHope that helps."
	p, err := ParsePlan(answer, testEvidence(), prev)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.WentWell) != 1 || p.WentWell[0].What != "Answered quickly" {
		t.Errorf("went well %+v", p.WentWell)
	}
	if len(p.WentBadly) != 1 || strings.Join(p.WentBadly[0].Evidence, ",") != "card:card_stuck,job:turn_bad" || p.WentBadly[0].Cause == "" {
		t.Errorf("went badly %+v", p.WentBadly)
	}
	var kinds []string
	for _, a := range p.Actions {
		kinds = append(kinds, a.Kind+":"+a.Target)
	}
	if got := strings.Join(kinds, " "); got != "skill:pr-follow-up memory:Login fix status soul:SOUL.md config:agents.habakkuk.fallbacks schedule:sch_backup ask:Reviewers" {
		t.Errorf("actions %s", got)
	}
	if a := p.Actions[0]; len(a.Evidence) != 1 || a.Risk != "low" {
		t.Errorf("skill action %+v", a)
	}
	if p.Actions[1].Risk != "medium" {
		t.Errorf("default risk %q", p.Actions[1].Risk)
	}
	if len(p.FollowUp) != 1 || p.FollowUp[0].Verdict != DidNotHelp {
		t.Errorf("follow-up %+v", p.FollowUp)
	}
	dropped := strings.Join(p.Dropped, "\n")
	for _, want := range []string{"Made up", "No evidence at all", `kind "teleport"`, "invalid skill name", "needs a one-line description",
		"looks like", "no change", "no reason", `risk "extreme"`, "no such action", "not the previous retro", `verdict "great"`} {
		if !strings.Contains(dropped, want) {
			t.Errorf("dropped lacks %q:\n%s", want, dropped)
		}
	}
	if strings.Contains(dropped+p.Summary, "sk-ant-") {
		// The refusal names the problem, not the secret.
		for _, d := range p.Dropped {
			if strings.Contains(d, "sk-ant-abcdefghij") {
				t.Errorf("secret repeated in dropped: %q", d)
			}
		}
	}
	if p.Quiet() {
		t.Error("a plan with findings is quiet")
	}
}

func TestParsePlanQuietAndBroken(t *testing.T) {
	p, err := ParsePlan(`{"summary": "[SILENT]"}`, testEvidence(), nil)
	if err != nil || !p.Quiet() || p.Summary != Silent {
		t.Fatalf("%+v %v", p, err)
	}
	// [SILENT] with findings is not silent.
	p, err = ParsePlan(`{"summary": "[SILENT]", "went_badly": [{"what": "x", "evidence": ["job:turn_bad"]}]}`, testEvidence(), nil)
	if err != nil || p.Quiet() || p.Summary == Silent {
		t.Fatalf("%+v %v", p, err)
	}
	// Follow-ups need a previous retro.
	p, _ = ParsePlan(`{"summary": "ok", "follow_up": [{"retro": "r", "action": 1, "verdict": "helped"}]}`, testEvidence(), nil)
	if len(p.FollowUp) != 0 || len(p.Dropped) != 1 {
		t.Fatalf("%+v", p)
	}
	for _, bad := range []string{"", "I could not decide.", `{"summary": `, `{"actions": "many"}`} {
		if _, err := ParsePlan(bad, testEvidence(), nil); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	// At most maxActions.
	var b strings.Builder
	b.WriteString(`{"summary": "s", "actions": [`)
	for i := range maxActions + 3 {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"kind": "ask", "target": "t", "change": "q", "why": "w"}`)
	}
	b.WriteString("]}")
	p, err = ParsePlan(b.String(), testEvidence(), nil)
	if err != nil || len(p.Actions) != maxActions || len(p.Dropped) != 3 {
		t.Fatalf("%d actions, dropped %v, %v", len(p.Actions), p.Dropped, err)
	}
}

func TestJobIDIsStable(t *testing.T) {
	loc := time.FixedZone("x", 5*3600)
	from := time.Date(2026, 11, 2, 17, 0, 0, 0, loc)
	to := from.AddDate(0, 0, 7)
	a := JobID("habakkuk", from, to)
	if a != "retro_habakkuk_20261102T1200_20261109T1200" {
		t.Fatalf("id %q", a)
	}
	if JobID("habakkuk", from.UTC(), to.UTC()) != a || JobID("habakkuk", from.Add(30*time.Second), to) != a {
		t.Fatal("the same period in another zone or within the minute got another id")
	}
	if JobID("habakkuk", from, to.Add(time.Hour)) == a || JobID("other", from, to) == a {
		t.Fatal("another period or agent got the same id")
	}
}
