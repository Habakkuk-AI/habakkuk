package retro

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/usage"
)

// fixture fills the stores with a week that went partly wrong, the way
// production weeks do: engineering work started and never finished, a
// delivery retried against a bad chat, fallbacks on auth errors, slow
// turns, a scheduled run denied an approval nobody could give, and a user
// correcting the agent.
func fixture(t *testing.T, rg *rig) map[string]string {
	t.Helper()
	ctx := context.Background()
	ids := map[string]string{}
	h := func(n int) time.Time { return rg.now.Add(-time.Duration(n) * time.Hour) }

	// Turns: three done (one slow), one failed with a secret in its error.
	slow := rg.conversation(t, "turn_slow", user("Open a pull request for the login fix"), assistant("Opened PR #42."))
	ids["slow"] = slow
	rg.job(t, "turn_fast1", "turn", "habakkuk", "done", 1, "", "ok", map[string]any{"session": "s1", "text": "hi"}, h(30), h(30).Add(2*time.Second))
	rg.job(t, "turn_fast2", "turn", "habakkuk", "done", 1, "", "ok", map[string]any{"session": "s2", "text": "weather?"}, h(29), h(29).Add(3*time.Second))
	rg.job(t, "turn_slow", "turn", "habakkuk", "done", 1, "", "ok", map[string]any{"session": slow, "text": "Open a pull request for the login fix"}, h(28), h(28).Add(9*time.Minute))
	rg.job(t, "turn_bad", "turn", "habakkuk", "failed", 3, "provider said: invalid key sk-ant-abcdefghijklmnopqrstuvwxyz0123", "", map[string]any{"session": "s3"}, h(20), h(20))
	// A job the worker died in (retried with no error), one retried after
	// an error, one canceled, and another agent's failure.
	rg.job(t, "turn_crash", "turn", "habakkuk", "done", 2, "", "ok", map[string]any{"session": "s4"}, h(10), h(10))
	rg.job(t, "subturn_retry", "subturn", "habakkuk", "done", 2, "rate limited", "ok", map[string]any{}, h(9), h(9))
	rg.job(t, "turn_cancel", "turn", "habakkuk", "canceled", 1, "", "", map[string]any{}, h(8), h(8))
	rg.job(t, "turn_other", "turn", "helper", "failed", 3, "boom", "", map[string]any{}, h(8), h(8))
	// Before the period: not counted.
	rg.job(t, "turn_old", "turn", "habakkuk", "failed", 3, "old", "", map[string]any{}, h(24*30), h(24*30))

	// A scheduled run whose turn asked for exec with nobody there, and a
	// script schedule that keeps failing.
	rg.exec(t, rg.control, `INSERT INTO schedules (id, agent, name, spec, prompt, next_run, last_status, fail_streak, created_at)
		VALUES ('sch_digest', 'habakkuk', 'digest', '0 8 * * *', 'send the digest', 0, 'started', 0, 0),
		       ('sch_backup', 'habakkuk', 'backup', '0 2 * * *', '', 0, 'error: exit 1: disk full', 4, 0),
		       ('sch_fine', 'habakkuk', 'fine', '@every 1h', 'ok', 0, 'ok', 0, 0)`)
	rg.job(t, "sched_d1", "scheduled", "habakkuk", "done", 1, "", "turn_sched_d1", map[string]any{"schedule": "sch_digest"}, h(40), h(40))
	rg.job(t, "turn_sched_d1", "turn", "habakkuk", "done", 1, "", "ok", map[string]any{"source": "schedule"}, h(40), h(40).Add(time.Minute))
	rg.job(t, "sched_b1", "scheduled", "habakkuk", "done", 1, "", "error: exit 1: disk full", map[string]any{"schedule": "sch_backup"}, h(30), h(30))
	rg.job(t, "sched_b2", "scheduled", "habakkuk", "done", 1, "", "skipped: daily limit reached", map[string]any{"schedule": "sch_backup"}, h(6), h(6))
	rg.job(t, "sched_f1", "scheduled", "habakkuk", "done", 1, "", "ok: sent", map[string]any{"schedule": "sch_fine"}, h(5), h(5))
	sched := rg.conversation(t, "turn_sched_d1",
		user("[Scheduled task \"digest\" (0 8 * * *). Do it now.]\nsend the digest"),
		callTool("c1", "exec"),
		toolResult("c1", "denied by permission policy: exec needs approval and no one is attending this run", true),
		assistant("I could not run the digest script."))
	ids["sched"] = sched

	// A chat with tool errors, a reviewer denial, a correction, a repeat,
	// a fallback answer and a polite "no thanks".
	chat := rg.conversation(t, "turn_chat",
		user("Summarize the board for me"),
		callTool("c2", "fetch_url"),
		toolResult("c2", "error: 404 not found", true),
		callTool("c3", "exec"),
		toolResult("c3", "denied by permission policy: the reviewer denied exec: deletes files (find a safer way, or ask the user)", true),
		llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "Here is the board.", Name: "p/backup"}}},
		user("No, that's not what I asked. I wanted the open cards only."),
		assistant("Sorry, here are the open cards."),
		user("Summarize the open board cards for me please"),
		assistant("Done."),
		user("Summarize the open board cards for me please"),
		user("no thanks, that's all"),
		callTool("c4", "fetch_url"),
		toolResult("c4", "error: timeout", true),
		callTool("c5", "exec"),
		toolResult("c5", "ok\n\n[approved by the reviewer: routine]", false),
	)
	ids["chat"] = chat

	// The board: a card nobody touched for ten days, one with a recent
	// comment, one done this week.
	rg.exec(t, rg.control, `INSERT INTO cards (id, title, status, assignee, created_by, created_at, updated_at) VALUES
		('card_stuck', 'Fix the login bug and open a PR', 'doing', 'habakkuk', 'user', ?, ?),
		('card_alive', 'Write the release notes', 'todo', 'habakkuk', 'user', ?, ?),
		('card_done', 'Rotate the keys', 'done', 'habakkuk', 'user', ?, ?)`,
		h(24*12).UnixMilli(), h(24*10).UnixMilli(), h(24*12).UnixMilli(), h(24*12).UnixMilli(), h(48).UnixMilli(), h(47).UnixMilli())
	rg.exec(t, rg.control, `INSERT INTO card_comments (card, author, text, at) VALUES ('card_alive', 'habakkuk', 'started', ?)`, h(5).UnixMilli())
	// A delegated task open for five days.
	rg.job(t, "subturn_open", "subturn", "habakkuk", "pending", 0, "", "", map[string]any{"sub": map[string]any{"task": "Review PR #42"}}, h(24*5), h(24*5))

	// Deliveries: one to the agent's chat retried seven times, one to
	// another agent's chat.
	rg.exec(t, rg.control, `INSERT INTO conversations (account, conversation, thread, agent, session_id, last_at) VALUES
		('tg', '555', '', 'habakkuk', 's1', 0), ('tg', '777', '', 'helper', 's9', 0)`)
	rg.job(t, "dlv_bad", "deliver", "", "pending", 7, "chat not found", "", map[string]any{"to": map[string]any{"account": "tg", "conversation": "555"}}, h(30), h(1))
	rg.job(t, "dlv_other", "deliver", "", "failed", 20, "chat not found", "", map[string]any{"to": map[string]any{"account": "tg", "conversation": "777"}}, h(30), h(1))
	rg.job(t, "dlv_ok", "deliver", "", "done", 1, "", "", map[string]any{"to": map[string]any{"account": "tg", "conversation": "555"}}, h(30), h(30))

	// Usage on two models.
	for _, c := range []usage.Call{
		{At: h(30), Provider: "anthropic", Model: "claude-opus-5-5", Agent: "habakkuk", Usage: llm.Usage{InputTokens: 1000, OutputTokens: 500}},
		{At: h(29), Provider: "anthropic", Model: "claude-opus-5-5", Agent: "habakkuk", Usage: llm.Usage{InputTokens: 1000, OutputTokens: 500}},
		{At: h(28), Provider: "p", Model: "backup", Agent: "habakkuk", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}},
		{At: h(28), Provider: "p", Model: "main", Agent: "helper", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}},
	} {
		if err := rg.usage.Record(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	// The log: a fallback on an auth error, and one of another agent's.
	logLines := []string{
		fmt.Sprintf(`{"time":%q,"level":"WARN","msg":"falling back","from":"p/main","to":"p/backup","reason":"auth"}`, h(28).UTC().Format(time.RFC3339Nano)),
		fmt.Sprintf(`{"time":%q,"level":"WARN","msg":"falling back","from":"p/main","to":"p/backup","reason":"auth"}`, h(27).UTC().Format(time.RFC3339Nano)),
		fmt.Sprintf(`{"time":%q,"level":"WARN","msg":"falling back","from":"q/other","to":"q/two","reason":"rate_limit"}`, h(27).UTC().Format(time.RFC3339Nano)),
		fmt.Sprintf(`{"time":%q,"level":"WARN","msg":"credential cooling down","endpoint":"p","credential":"P_KEY","reason":"auth","for":"30m0s"}`, h(28).UTC().Format(time.RFC3339Nano)),
		`not json at all`,
	}
	if err := os.WriteFile(filepath.Join(rg.home, "habakkuk.log"), []byte(strings.Join(logLines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Memory: a note the agent saved, then the user undid it.
	actx := history.WithActor(ctx, history.Actor{By: history.ByAgent, Tool: "memory_save"})
	if _, _, err := rg.mem.Save(actx, "habakkuk", "Board summary style", "full board", nil); err != nil {
		t.Fatal(err)
	}
	cs := rg.changes(t)
	ids["saved"] = cs[0].ID
	uctx := history.WithActor(ctx, history.Actor{By: history.ByUser, Tool: "/undo-memory"})
	if _, r, err := rg.mem.Restore(uctx, "habakkuk", cs[0].ID, history.Before); err != nil || r == nil {
		t.Fatalf("undo: %v %v", r, err)
	}
	ids["undo"] = rg.changes(t)[0].ID
	return ids
}

func TestGatherEvidence(t *testing.T) {
	ctx := context.Background()
	rg := newRig(t)
	ids := fixture(t, rg)
	src := rg.r.Sources
	src.Undos = func(context.Context, string) ([]Undo, error) {
		return []Undo{{ID: "cp1", At: rg.now.Add(-time.Hour), Session: ids["chat"], Task: "clean the repo"},
			{ID: "cp0", At: rg.now.Add(-30 * 24 * time.Hour), Task: "too old"}}, nil
	}
	ev, err := Gather(ctx, src, rg.r.Agents["habakkuk"], rg.from, rg.to)
	if err != nil {
		t.Fatal(err)
	}
	text := ev.Text()
	refs := ev.Refs()

	// Turns: latency over the done turns, the slowest with its message.
	if ev.Turns.Count != 9 || ev.Turns.Failed != 1 || ev.Turns.P95MS != 9*60*1000 {
		t.Errorf("turns %+v", ev.Turns)
	}
	if s := ev.Turns.Slow[0]; s.Ref != "job:turn_slow" || s.Session != ids["slow"] || !strings.Contains(s.First, "pull request") {
		t.Errorf("slowest %+v", s)
	}
	// Jobs: failed (with the key redacted), interrupted, retried, canceled;
	// another agent's and older ones left out.
	what := map[string]string{}
	for _, p := range ev.Problems {
		what[p.Ref] = p.What
		if strings.Contains(p.Error, "sk-ant-") {
			t.Errorf("secret in evidence: %q", p.Error)
		}
	}
	for ref, want := range map[string]string{"job:turn_bad": "failed", "job:turn_crash": "interrupted", "job:subturn_retry": "retried", "job:turn_cancel": "canceled"} {
		if what[ref] != want {
			t.Errorf("%s: %q, want %q (%v)", ref, what[ref], want, what)
		}
	}
	if refs["job:turn_other"] || refs["job:turn_old"] {
		t.Error("another agent's or an old job in the evidence")
	}
	// Tool errors by tool; approvals by kind; the scheduled run's denial.
	if len(ev.ToolErrors) != 1 || ev.ToolErrors[0].Tool != "fetch_url" || ev.ToolErrors[0].Count != 2 {
		t.Errorf("tool errors %+v", ev.ToolErrors)
	}
	if a := ev.Approvals; a.NoApprover != 1 || a.ReviewerDenied != 1 || a.ReviewerApproved != 1 {
		t.Errorf("approvals %+v", a)
	}
	// Schedules: the digest's run was denied an approval, the backup
	// script fails and was skipped by a budget; the fine one is counted.
	sch := map[string]ScheduleIssue{}
	for _, s := range ev.Schedules {
		sch[s.Ref] = s
	}
	if s := sch["schedule:sch_digest"]; s.Denied != 1 || s.Runs != 1 || len(s.Refs) != 1 || s.Refs[0] != "job:sched_d1" {
		t.Errorf("digest %+v", s)
	}
	if s := sch["schedule:sch_backup"]; s.Errors != 1 || s.BudgetSkips != 1 || s.FailStreak != 4 {
		t.Errorf("backup %+v", s)
	}
	if ev.SchedulesOK != 1 || len(ev.Schedules) != 2 {
		t.Errorf("schedules ok %d, issues %+v", ev.SchedulesOK, ev.Schedules)
	}
	// Board: the untouched card and the open delegated task are stalled;
	// the commented one is not.
	if !refs["card:card_stuck"] || refs["card:card_alive"] || !refs["job:subturn_open"] || ev.CardsDone != 1 {
		t.Errorf("stalled %+v, done %d", ev.Stalled, ev.CardsDone)
	}
	// Deliveries: only the agent's chat.
	if len(ev.Deliveries) != 1 || ev.Deliveries[0].Ref != "job:dlv_bad" || ev.Deliveries[0].Attempts != 7 || ev.Deliveries[0].Target != "tg:555" {
		t.Errorf("deliveries %+v", ev.Deliveries)
	}
	// Fallbacks: the transcript's answer from the backup model and the
	// log's auth fallbacks and cool-down; not another provider's.
	var fromLog, fromTranscript, cooling bool
	for _, f := range ev.Fallbacks {
		switch {
		case f.From == "p/main" && f.To == "p/backup" && f.Reason == "auth" && f.Count == 2:
			fromLog = true
		case f.To == "p/backup" && f.Count == 1 && len(f.Refs) == 1 && f.Refs[0] == "session:"+ids["chat"]:
			fromTranscript = true
		case strings.Contains(f.To, "P_KEY cooling down") && f.Reason == "auth":
			cooling = true
		case f.From == "q/other":
			t.Errorf("another provider's fallback: %+v", f)
		}
	}
	if !fromLog || !fromTranscript || !cooling {
		t.Errorf("fallbacks %+v", ev.Fallbacks)
	}
	// Cost by model, the agent's calls only.
	if len(ev.Cost) != 2 || ev.Cost[0].Model != "anthropic/claude-opus-5-5" || ev.Cost[0].Calls != 2 || ev.Cost[0].USD <= 0 {
		t.Errorf("cost %+v", ev.Cost)
	}
	// Corrections: the "no, that's not what I asked", the repeated request,
	// the /undo-memory and the /undo; not "no thanks".
	kinds := map[string]int{}
	for _, c := range ev.Corrections {
		kinds[c.Kind]++
		if strings.Contains(c.Text, "no thanks") {
			t.Errorf("polite no counted: %+v", c)
		}
	}
	if kinds["correction"] != 1 || kinds["repeat"] != 1 || kinds["undo-memory"] != 1 || kinds["undo"] != 1 {
		t.Errorf("corrections %+v", ev.Corrections)
	}
	if !refs["change:"+ids["undo"]] || !refs["change:"+ids["saved"]] || !refs["checkpoint:cp1"] {
		t.Errorf("changes %+v", ev.Changes)
	}
	// The rendered text cites its sources and keeps secrets out.
	for _, want := range []string{"job:turn_bad", "session:" + ids["sched"], "card:card_stuck", "schedule:sch_backup",
		"job:dlv_bad", "log:", "nobody attending the run 1", "fetch_url: 2 errors", "[redacted]"} {
		if !strings.Contains(text, want) {
			t.Errorf("evidence text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "sk-ant-abc") {
		t.Error("secret in the evidence text")
	}
	// Every ref the text cites is a known one.
	if len(refs) < 20 {
		t.Errorf("only %d refs", len(refs))
	}
}

func TestGatherEmptyPeriod(t *testing.T) {
	rg := newRig(t)
	ev, err := Gather(context.Background(), rg.r.Sources, rg.r.Agents["habakkuk"], rg.from, rg.to)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Turns.Count != 0 || len(ev.Problems) != 0 || len(ev.Refs()) != 0 {
		t.Fatalf("%+v", ev)
	}
	if !strings.Contains(ev.Text(), "0 sessions, 0 turns") {
		t.Fatal(ev.Text())
	}
}

func TestCorrectionPatterns(t *testing.T) {
	for text, want := range map[string]bool{
		"No, the other file":                 true,
		"that’s not what I asked":            true,
		"You didn't save it":                 true,
		"wrong date":                         true,
		"I already told you to use metric":   true,
		"try again":                          true,
		"Nope":                               true,
		"Nothing else for now":               false,
		"Notice the new design":              false,
		"Can you summarize my inbox?":        false,
		"Please write the release notes now": false,
	} {
		if got := reCorrection.MatchString(text) && !politeNo(text); got != want {
			t.Errorf("%q: %v, want %v", text, got, want)
		}
	}
	if !repeated(normalize("Summarize the open cards please"), []string{normalize("summarize the open cards, please!")}) {
		t.Error("near-identical request not a repeat")
	}
	if repeated(normalize("ok"), []string{"ok"}) {
		t.Error("one word counted as a repeat")
	}
}
