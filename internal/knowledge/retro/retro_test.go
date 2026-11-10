package retro

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
)

// planWithEveryKind proposes one action of each kind, citing the fixture.
const planWithEveryKind = `{
  "summary": "The login work stalled and a delivery kept failing.",
  "went_well": [{"what": "Quick answers in chat", "evidence": ["job:turn_fast1"]}],
  "went_badly": [
    {"what": "The login card sat for ten days", "cause": "Nobody followed up on the PR", "evidence": ["card:card_stuck"]},
    {"what": "A reply was retried 7 times", "cause": "The chat no longer exists", "evidence": ["job:dlv_bad"]}
  ],
  "actions": [
    {"kind": "skill", "target": "pr-follow-up", "description": "Use when a pull request has waited for review", "change": "List open pull requests every morning and ask the reviewer.", "why": "PRs sat", "expected": "No PR waits over two days", "risk": "low", "evidence": ["card:card_stuck"]},
    {"kind": "memory", "target": "Delivery targets", "change": "tg:555 no longer exists; use the home chat.", "why": "retries", "expected": "No retried deliveries", "risk": "low"},
    {"kind": "soul", "change": "Finish or hand back every task you start.", "why": "unfinished work", "risk": "medium"},
    {"kind": "config", "target": "agents.habakkuk.fallbacks", "change": "[p/backup]", "why": "auth errors", "risk": "high"},
    {"kind": "schedule", "target": "sch_backup", "change": "pause", "why": "fails every night", "risk": "low"},
    {"kind": "ask", "target": "Reviewers", "change": "Who should review pull requests?", "why": "nobody does", "risk": "low"}
  ]
}`

func setupRun(t *testing.T, rg *rig, apply string) {
	t.Helper()
	fixture(t, rg)
	rg.model.answer = planWithEveryKind
	rg.r.Apply = apply
	if _, err := rg.r.Enqueue(context.Background(), "habakkuk", rg.from, rg.to, false); err != nil {
		t.Fatal(err)
	}
}

func statuses(ret *Retro) string {
	var s []string
	for _, a := range ret.Actions {
		s = append(s, a.Kind+"="+a.Status)
	}
	return strings.Join(s, " ")
}

func TestProposeChangesNothing(t *testing.T) {
	ctx := context.Background()
	rg := newRig(t)
	setupRun(t, rg, ApplyPropose)
	before := len(rg.changes(t))
	j, summary, err := rg.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if j.ID != JobID("habakkuk", rg.from, rg.to) || summary != "2 went badly, 6 proposed" {
		t.Fatalf("job %s: %q", j.ID, summary)
	}
	if n := len(rg.changes(t)); n != before {
		t.Fatalf("propose made %d memory changes", n-before)
	}
	if _, err := os.Stat(filepath.Join(rg.home, "agents", "habakkuk", "skills", "pr-follow-up")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("propose wrote a skill")
	}
	if _, err := os.Stat(filepath.Join(rg.home, "agents", "habakkuk", "SOUL.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("propose wrote SOUL.md")
	}
	if sc, _ := rg.scheds.Get(ctx, "sch_backup"); !sc.Enabled {
		t.Fatal("propose paused a schedule")
	}
	ret, err := rg.r.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := statuses(ret); got != "skill=proposed memory=proposed soul=proposed config=proposed schedule=proposed ask=proposed" {
		t.Fatalf("statuses %s", got)
	}
	// The model saw the soul, skills, schedules and evidence, with ids.
	in := rg.model.inputs[0]
	for _, want := range []string{"<evidence>", "card:card_stuck", "sch_backup", "job:dlv_bad"} {
		if !strings.Contains(in, want) {
			t.Errorf("model input lacks %q", want)
		}
	}
	// The report is written and the summary delivered to the home chat.
	report, err := os.ReadFile(filepath.Join(rg.home, filepath.FromSlash(ret.Report)))
	if err != nil {
		t.Fatal(err)
	}
	day := rg.to.UTC().Format("2006-01-02")
	if ret.Report != "agents/habakkuk/retros/"+day+".md" || !strings.Contains(string(report), "### 1. skill `pr-follow-up` [proposed]") ||
		!strings.Contains(string(report), "Likely cause: Nobody followed up") || !strings.Contains(string(report), "## Evidence") {
		t.Fatalf("report %s:\n%s", ret.Report, report)
	}
	if len(rg.delivered) != 1 || rg.delivered[0].target != "tg:home" {
		t.Fatalf("delivered %+v", rg.delivered)
	}
	msg := rg.delivered[0].text
	for _, want := range []string{"**What went wrong**", "The login card sat", "**Needs a yes**", "1. skill pr-follow-up", "/retro apply N", ret.Report} {
		if !strings.Contains(msg, want) {
			t.Errorf("summary lacks %q:\n%s", want, msg)
		}
	}
	// Running the finished job again changes and sends nothing.
	rg.model.fail = true
	if out, err := rg.rerun(t, j); err != nil || out != summary || len(rg.delivered) != 1 {
		t.Fatalf("rerun: %q %v %d", out, err, len(rg.delivered))
	}
}

func TestSafeAppliesOnlySkillsAndMemoryAndIsUndoable(t *testing.T) {
	ctx := context.Background()
	rg := newRig(t)
	setupRun(t, rg, ApplySafe)
	j, summary, err := rg.run(t)
	if err != nil {
		t.Fatal(err)
	}
	ret, _ := rg.r.Get(ctx, j.ID)
	if got := statuses(ret); got != "skill=applied memory=applied soul=proposed config=proposed schedule=proposed ask=proposed" {
		t.Fatalf("statuses %s", got)
	}
	if summary != "2 went badly, 6 proposed, 2 applied" {
		t.Fatalf("summary %q", summary)
	}
	s, err := rg.skills.Get("habakkuk", "pr-follow-up")
	if err != nil || !strings.Contains(s.Description, "pull request") {
		t.Fatalf("skill %+v %v", s, err)
	}
	note, err := rg.mem.Read(ctx, "habakkuk", "delivery-targets")
	if err != nil || !note.Untrusted() {
		t.Fatalf("note %+v %v", note, err)
	}
	if _, err := os.Stat(filepath.Join(rg.home, "agents", "habakkuk", "SOUL.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("safe mode wrote SOUL.md")
	}
	if sc, _ := rg.scheds.Get(ctx, "sch_backup"); !sc.Enabled {
		t.Fatal("safe mode paused a schedule")
	}
	// Both changes are in the history as the retro's, and undo cleanly.
	var ids []string
	for _, c := range rg.changes(t) {
		if c.By == history.ByRetro {
			ids = append(ids, c.ID)
			if !strings.Contains(c.Note, j.ID) || !c.Tainted {
				t.Errorf("change %+v", c)
			}
		}
	}
	if len(ids) != 2 || !strings.Contains(ret.Actions[0].Result, "change "+ids[1]) && !strings.Contains(ret.Actions[0].Result, "change "+ids[0]) {
		t.Fatalf("retro changes %v; result %q", ids, ret.Actions[0].Result)
	}
	uctx := history.WithActor(ctx, history.Actor{By: history.ByUser, Tool: "/undo-memory"})
	for _, id := range ids {
		if _, r, err := rg.mem.Restore(uctx, "habakkuk", id, history.Before); err != nil || r == nil {
			t.Fatalf("undo %s: %v %v", id, r, err)
		}
	}
	if _, err := rg.mem.Read(ctx, "habakkuk", "delivery-targets"); err == nil {
		t.Fatal("note still there after undo")
	}
	if _, err := rg.skills.Get("habakkuk", "pr-follow-up"); err == nil {
		t.Fatal("skill still there after undo")
	}
	// The summary says what changed and what still needs a yes.
	msg := rg.delivered[0].text
	if !strings.Contains(msg, "**Changed**") || !strings.Contains(msg, "3. soul SOUL.md") || strings.Contains(msg, "**Needs a yes**\n1.") {
		t.Fatalf("summary:\n%s", msg)
	}
}

func TestResumeAfterCrashAppliesOnce(t *testing.T) {
	ctx := context.Background()
	rg := newRig(t)
	setupRun(t, rg, ApplySafe)
	rg.r.crashAfter = func(n int) bool { return n == 1 } // dies before the memory action
	j, _, err := rg.run(t)
	if !errors.Is(err, errCrash) {
		t.Fatalf("want the simulated crash, got %v", err)
	}
	if _, err := rg.mem.Read(ctx, "habakkuk", "delivery-targets"); err == nil {
		t.Fatal("second action applied before the crash")
	}
	afterCrash := len(rg.changes(t))

	// A restarted process: fresh stores and runner, and a model that must
	// not be asked again.
	rg.open(t)
	rg.r.Apply = ApplySafe
	rg.model.fail = true
	out, err := rg.rerun(t, j)
	if err != nil {
		t.Fatal(err)
	}
	if out != "2 went badly, 6 proposed, 2 applied" || rg.model.calls() != 1 {
		t.Fatalf("resumed: %q, %d model calls", out, rg.model.calls())
	}
	if n := len(rg.changes(t)); n != afterCrash+1 {
		t.Fatalf("resume made %d changes, want 1", n-afterCrash)
	}

	// A crash after applying but before recording: applying again makes
	// no second change.
	rg2 := newRig(t)
	setupRun(t, rg2, ApplySafe)
	rg2.r.crashAfter = func(n int) bool { return n == 2 }
	j2, _, _ := rg2.run(t)
	rg2.exec(t, rg2.r.db(), `UPDATE retros SET next = 0`)
	rg2.exec(t, rg2.r.db(), `UPDATE retro_actions SET status = 'proposed', result = ''`)
	before := len(rg2.changes(t))
	rg2.r.crashAfter = nil
	if _, err := rg2.rerun(t, j2); err != nil {
		t.Fatal(err)
	}
	if n := len(rg2.changes(t)); n != before {
		t.Fatalf("re-applying made %d new changes", n-before)
	}
	ret, _ := rg2.r.Get(ctx, j2.ID)
	if ret.Actions[0].Status != Applied || ret.Actions[1].Status != Applied {
		t.Fatalf("statuses %s", statuses(ret))
	}
}

func TestApproveAndReject(t *testing.T) {
	ctx := context.Background()
	rg := newRig(t)
	setupRun(t, rg, ApplyPropose)
	soulPath := filepath.Join(rg.home, "agents", "habakkuk", "SOUL.md")
	if err := os.MkdirAll(filepath.Dir(soulPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(soulPath, []byte("You are Habakkuk.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	j, _, err := rg.run(t)
	if err != nil {
		t.Fatal(err)
	}
	actor := history.Actor{By: history.ByUser, Tool: "cli"}
	out, err := rg.r.ApplyActions(ctx, j.ID, []int{3, 4, 5, 6}, actor)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]Outcome{}
	for _, o := range out {
		got[o.N] = o
	}
	// SOUL.md: appended, recorded as the user's, undoable.
	soul, _ := os.ReadFile(soulPath)
	if got[3].Status != Applied || string(soul) != "You are Habakkuk.\n\nFinish or hand back every task you start.\n" {
		t.Fatalf("soul %+v: %q", got[3], soul)
	}
	var soulChange history.Change
	for _, c := range rg.changes(t) {
		if c.Kind == history.KindSoul && c.By == history.ByUser {
			soulChange = c
		}
	}
	if soulChange.ID == "" || !strings.Contains(got[3].Result, soulChange.ID) || soulChange.Tool != "cli" {
		t.Fatalf("soul change %+v, result %q", soulChange, got[3].Result)
	}
	// Config: never edited, only spelled out.
	if got[4].Status != Approved || !strings.Contains(got[4].Result, "agents.habakkuk.fallbacks") || !strings.Contains(got[4].Result, "[p/backup]") {
		t.Fatalf("config %+v", got[4])
	}
	// Schedule: paused, and resumable.
	if sc, _ := rg.scheds.Get(ctx, "sch_backup"); sc.Enabled || got[5].Status != Applied {
		t.Fatalf("schedule %+v %+v", sc, got[5])
	}
	if got[6].Status != Approved {
		t.Fatalf("ask %+v", got[6])
	}
	// Applying again is harmless.
	again, err := rg.r.ApplyActions(ctx, j.ID, []int{3}, actor)
	if err != nil || !strings.HasPrefix(again[0].Result, "already applied") {
		t.Fatalf("again %+v %v", again, err)
	}
	soul2, _ := os.ReadFile(soulPath)
	if string(soul2) != string(soul) {
		t.Fatal("SOUL.md changed twice")
	}
	// Undo the SOUL.md edit.
	if _, r, err := rg.mem.Restore(ctx, "habakkuk", soulChange.ID, history.Before); err != nil || r == nil {
		t.Fatalf("undo soul: %v %v", r, err)
	}
	if soul, _ := os.ReadFile(soulPath); string(soul) != "You are Habakkuk.\n" {
		t.Fatalf("soul after undo %q", soul)
	}
	// Reject: a proposed action, not an applied one; bad numbers refused.
	if o, err := rg.r.Reject(ctx, j.ID, 1); err != nil || o.Status != Rejected {
		t.Fatalf("reject %+v %v", o, err)
	}
	if _, err := rg.r.Reject(ctx, j.ID, 3); err == nil {
		t.Fatal("rejected an applied action")
	}
	if _, err := rg.r.ApplyActions(ctx, j.ID, []int{9}, actor); err == nil {
		t.Fatal("applied action 9 of 6")
	}
	// A rejected action can still be applied if the owner changes their
	// mind.
	if o, err := rg.r.ApplyActions(ctx, j.ID, []int{1}, actor); err != nil || o[0].Status != Applied {
		t.Fatalf("apply after reject %+v %v", o, err)
	}
	ret, _ := rg.r.Get(ctx, j.ID)
	if got := statuses(ret); got != "skill=applied memory=proposed soul=applied config=approved schedule=applied ask=approved" {
		t.Fatalf("statuses %s", got)
	}
	report, _ := os.ReadFile(filepath.Join(rg.home, filepath.FromSlash(ret.Report)))
	if !strings.Contains(string(report), "### 3. soul `SOUL.md` [applied]") || !strings.Contains(string(report), "### 4. config `agents.habakkuk.fallbacks` [approved]") {
		t.Fatalf("report not updated:\n%s", report)
	}
}

func TestQuietRetroIsNotDelivered(t *testing.T) {
	ctx := context.Background()
	rg := newRig(t)
	rg.model.answer = `{"summary": "[SILENT]"}`
	if _, err := rg.r.Enqueue(ctx, "habakkuk", rg.from, rg.to, false); err != nil {
		t.Fatal(err)
	}
	if _, out, err := rg.run(t); err != nil || out != "nothing notable" {
		t.Fatalf("%q %v", out, err)
	}
	if len(rg.delivered) != 0 {
		t.Fatalf("a quiet retro was delivered: %+v", rg.delivered)
	}
	// One asked for on demand is never delivered either.
	rg.model.answer = planWithEveryKind
	fixture(t, rg)
	if _, err := rg.r.Enqueue(ctx, "habakkuk", rg.from.Add(time.Hour), rg.to, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rg.run(t); err != nil || len(rg.delivered) != 0 {
		t.Fatalf("on-demand retro delivered: %v %+v", err, rg.delivered)
	}
}

func TestFollowUpSeesThePreviousRetro(t *testing.T) {
	ctx := context.Background()
	rg := newRig(t)
	setupRun(t, rg, ApplyPropose)
	first, _, err := rg.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rg.r.ApplyActions(ctx, first.ID, []int{1}, history.Actor{By: history.ByUser, Tool: "cli"}); err != nil {
		t.Fatal(err)
	}
	rg.model.answer = `{"summary": "The PR skill did not help.", "follow_up": [{"retro": "` + first.ID + `", "action": 1, "verdict": "did_not_help", "note": "the card is still stuck", "evidence": ["card:card_stuck"]}]}`
	if _, err := rg.r.Enqueue(ctx, "habakkuk", rg.to.Add(-time.Hour), rg.to.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	second, _, err := rg.run(t)
	if err != nil {
		t.Fatal(err)
	}
	in := rg.model.inputs[1]
	if !strings.Contains(in, `<previous_retro id="`+first.ID+`">`) || !strings.Contains(in, "action 1 [applied] skill pr-follow-up") {
		t.Fatalf("second retro input:\n%s", in)
	}
	ret, _ := rg.r.Get(ctx, second.ID)
	if len(ret.Plan.FollowUp) != 1 || ret.Plan.FollowUp[0].Verdict != DidNotHelp || ret.Plan.Quiet() {
		t.Fatalf("follow-up %+v", ret.Plan)
	}
	list, err := rg.r.List(ctx, "habakkuk", 10)
	if err != nil || len(list) != 2 || list[0].ID != second.ID || list[0].Evidence != nil {
		t.Fatalf("list %+v %v", list, err)
	}
	if latest, err := rg.r.Resolve(ctx, "habakkuk", "latest"); err != nil || latest.ID != second.ID {
		t.Fatalf("latest %v %v", latest, err)
	}
}

func TestTickQueuesEachSlotOnce(t *testing.T) {
	ctx := context.Background()
	rg := newRig(t)
	rg.r.Spec = "0 17 * * 5" // weekly, fri 17:00 UTC
	rg.now = time.Date(2026, 11, 13, 18, 30, 0, 0, time.UTC)
	// No agent had turns: nothing to queue.
	if err := rg.r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countJobs(t, rg); n != 0 {
		t.Fatalf("%d jobs with no active agent", n)
	}
	rg.exec(t, rg.transcripts, `INSERT INTO sessions (id, agent, system_prompt, created_at, updated_at) VALUES ('s1', 'habakkuk', '', ?, ?), ('s2', 'stranger', '', ?, ?)`,
		rg.now.Add(-48*time.Hour).UnixMilli(), rg.now.Add(-2*time.Hour).UnixMilli(), rg.now.Add(-48*time.Hour).UnixMilli(), rg.now.Add(-2*time.Hour).UnixMilli())
	for range 3 {
		if err := rg.r.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	slot := time.Date(2026, 11, 13, 17, 0, 0, 0, time.UTC)
	want := JobID("habakkuk", slot.AddDate(0, 0, -7), slot)
	if j, err := rg.queue.Get(ctx, want); err != nil || j.Kind != Kind {
		t.Fatalf("job %s: %v %v", want, j, err)
	}
	if n := countJobs(t, rg); n != 1 {
		t.Fatalf("%d jobs, want 1 (unknown agents are skipped, ticks repeat harmlessly)", n)
	}
	// Two days later the slot is too old to run late; the next week
	// gets its own job.
	rg.now = rg.now.Add(48 * time.Hour)
	_ = rg.r.Tick(ctx)
	if n := countJobs(t, rg); n != 1 {
		t.Fatalf("late slot queued: %d jobs", n)
	}
	rg.r.Only = []string{"habakkuk"}
	rg.now = time.Date(2026, 11, 20, 17, 1, 0, 0, time.UTC)
	_ = rg.r.Tick(ctx)
	if n := countJobs(t, rg); n != 2 {
		t.Fatalf("%d jobs after the next slot", n)
	}
	// Off: nothing.
	rg.r.Spec = ""
	rg.now = time.Date(2026, 11, 27, 17, 1, 0, 0, time.UTC)
	_ = rg.r.Tick(ctx)
	if n := countJobs(t, rg); n != 2 {
		t.Fatalf("queued while off: %d", n)
	}
}

func countJobs(t *testing.T, rg *rig) int {
	t.Helper()
	var n int
	if err := rg.control.QueryRow(`SELECT count(*) FROM jobs WHERE kind = ?`, Kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
