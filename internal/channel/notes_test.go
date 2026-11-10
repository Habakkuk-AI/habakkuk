package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/state"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// withNotes gives a rig state.db, which chat notes live in.
func withNotes(t *testing.T, r *rig) {
	t.Helper()
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	r.g.State = st
	r.g.Runner.Notes = r.g
}

var chat100 = Address{Account: "tg", Conversation: "100"}

// waitJob waits until a job is done.
func waitJob(t *testing.T, q *jobs.Queue, id string) {
	t.Helper()
	waitFor(t, "job "+id, func() bool {
		j, err := q.Get(context.Background(), id)
		return err == nil && j != nil && j.Status == jobs.Done
	})
}

// chatSession is the session bound to chat 100.
func chatSession(t *testing.T, r *rig) string {
	t.Helper()
	conv, err := r.g.Store.Conversation(context.Background(), chat100)
	if err != nil || conv == nil {
		t.Fatalf("no conversation: %v", err)
	}
	return conv.SessionID
}

// userTexts is every user message in a session's transcript.
func userTexts(t *testing.T, r *rig, sessionID string) []string {
	t.Helper()
	recs, err := r.g.Runner.Sessions.Messages(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, rec := range recs {
		if rec.Message.Role == llm.User {
			out = append(out, rec.Message.Text())
		}
	}
	return out
}

// A scheduled run of the chat's own agent delivers into the chat from its
// own session. The user's next message is answered knowing what was sent,
// and only that turn shows it.
func TestScheduledDeliveryNotedForNextTurn(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	withNotes(t, r)
	ctx := context.Background()
	r.g.Handle(ctx, dm("m1", "hello"))
	waitFor(t, "first reply", hasText(r.a, "echo: hello"))

	sched, err := r.g.Runner.NewSession(ctx, "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := json.Marshal(chat100)
	if _, err := r.g.Runner.SubmitTurn(ctx, agent.Turn{ID: "turn_sched", Session: sched.ID, Text: "inbox has 3 new mails",
		Origin: origin, Background: true, Source: agent.SourceSchedule, Task: `scheduled task "inbox"`}); err != nil {
		t.Fatal(err)
	}
	r.g.Wake()
	waitJob(t, r.q, "dlv_turn_sched")

	r.g.Handle(ctx, dm("m2", "how did you do that?"))
	waitFor(t, "reply with the note", hasText(r.a, "how did you do that?"))
	r.g.Handle(ctx, dm("m3", "thanks"))
	waitFor(t, "third reply", hasText(r.a, "echo: thanks"))

	users := userTexts(t, r, chatSession(t, r))
	if len(users) != 3 {
		t.Fatalf("user messages: %q", users)
	}
	want := `[Earlier in this chat, at `
	who := `your scheduled task "inbox" sent: "echo: inbox has 3 new mails"]`
	if !strings.Contains(users[1], want) || !strings.Contains(users[1], who) {
		t.Fatalf("second message lacks the note: %q", users[1])
	}
	if i, j := strings.Index(users[1], who), strings.Index(users[1], "how did you do that?"); i > j {
		t.Fatalf("the note should come before the user's words: %q", users[1])
	}
	for _, u := range []string{users[0], users[2]} {
		if strings.Contains(u, "Earlier in this chat") {
			t.Fatalf("note shown in the wrong turn: %q", u)
		}
	}
	// The scheduled session itself gets no notes, and the chat's own
	// replies left none.
	for _, u := range userTexts(t, r, sched.ID) {
		if strings.Contains(u, "Earlier in this chat") {
			t.Fatalf("scheduled session got a note: %q", u)
		}
	}
	if notes, _ := r.g.State.ClaimChatNotes(ctx, chat100.key(), "probe", time.Now().Add(-time.Hour)); len(notes) != 0 {
		t.Fatalf("notes left over: %+v", notes)
	}
}

// Another agent asks a question in this chat (a home post); the user's
// "yes" reaches the chat's agent with that question in view.
func TestHomePostFromAnotherAgentNoted(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	withNotes(t, r)
	ctx := context.Background()
	eng := *r.g.Runner.Agents["habakkuk"]
	eng.Name, eng.Home, eng.ReportHome = "engineering", "tg:100", agent.ReportResults
	r.g.Runner.Agents["engineering"] = &eng

	r.g.Handle(ctx, dm("m1", "hi"))
	waitFor(t, "first reply", hasText(r.a, "echo: hi"))
	n := agent.HomeNews{Job: "card_1", Agent: "engineering", Session: "ses_eng", Home: "tg:100",
		Kind: "card", Source: agent.SourceBoard, Task: "PR 12", Final: "Tests pass. Merge it?"}
	if err := r.g.ReportHome(ctx, n); err != nil {
		t.Fatal(err)
	}
	waitJob(t, r.q, homePostID("card_1", false))
	r.g.Handle(ctx, dm("m2", "yes"))
	waitFor(t, "answer", hasText(r.a, "\n\nyes"))

	users := userTexts(t, r, chatSession(t, r))
	if len(users) != 2 || !strings.Contains(users[1], `agent engineering sent: "**Done:** PR 12`) ||
		!strings.Contains(users[1], "Merge it?") {
		t.Fatalf("user messages: %q", users)
	}
}

// Notes go only to the chat that got the message, and `habakkuk send` is
// named as the sender.
func TestNotesStayInTheirChat(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	withNotes(t, r)
	ctx := context.Background()
	if err := r.g.SendText(ctx, "send_other", Address{Account: "tg", Conversation: "200"}, "for chat 200"); err != nil {
		t.Fatal(err)
	}
	if err := r.g.SendText(ctx, "send_here", chat100, "for chat 100"); err != nil {
		t.Fatal(err)
	}
	// A script schedule delivering its output names itself.
	script := agent.WithPoster(ctx, agent.Poster{Agent: "habakkuk", What: `scheduled task "digest"`})
	origin, _ := json.Marshal(chat100)
	if err := r.g.Deliver(script, "dlv_script", origin, "digest output"); err != nil {
		t.Fatal(err)
	}
	waitJob(t, r.q, "send_other")
	waitJob(t, r.q, "send_here")
	waitJob(t, r.q, "dlv_script")
	r.g.Handle(ctx, dm("m1", "what was that?"))
	waitFor(t, "reply", hasText(r.a, "what was that?"))
	users := userTexts(t, r, chatSession(t, r))
	if len(users) != 1 || !strings.Contains(users[0], "`habakkuk send` sent: \"for chat 100\"") ||
		!strings.Contains(users[0], `your scheduled task "digest" sent: "digest output"`) ||
		strings.Contains(users[0], "for chat 200") {
		t.Fatalf("user message: %q", users)
	}
}

// A turn that claimed its notes and died before writing its message shows
// the same notes when it runs again; no other turn takes them meanwhile.
func TestNotesSurviveACrashedTurn(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	withNotes(t, r)
	ctx := context.Background()
	r.g.Handle(ctx, dm("m1", "hi"))
	waitFor(t, "first reply", hasText(r.a, "echo: hi"))
	sid := chatSession(t, r)
	if err := r.g.SendText(ctx, "send_1", chat100, "Merge it?"); err != nil {
		t.Fatal(err)
	}
	waitJob(t, r.q, "send_1")

	m := dm("m2", "yes")
	first := r.g.TakeNotes(ctx, sid, TurnID(m)) // the attempt that crashed
	if !strings.Contains(first, "Merge it?") {
		t.Fatalf("claim = %q", first)
	}
	if other := r.g.TakeNotes(ctx, sid, "turn_other"); other != "" {
		t.Fatalf("another turn took claimed notes: %q", other)
	}
	r.g.Handle(ctx, m) // the retry: same job id
	waitFor(t, "reply", hasText(r.a, "\n\nyes"))
	users := userTexts(t, r, sid)
	if len(users) != 2 || !strings.Contains(users[1], `sent: "Merge it?"`) {
		t.Fatalf("user messages: %q", users)
	}
	if again := r.g.TakeNotes(ctx, sid, "turn_later"); again != "" {
		t.Fatalf("a seen note came back: %q", again)
	}
}

func TestRenderNotesCaps(t *testing.T) {
	now := time.Date(2026, 11, 9, 19, 0, 0, 0, time.UTC)
	var notes []state.ChatNote
	for i := range 10 {
		notes = append(notes, state.ChatNote{Agent: "ops", What: fmt.Sprintf("scheduled task %q", "t"),
			Text: fmt.Sprintf("note %d ", i) + strings.Repeat("x", 3000), At: now.Add(time.Duration(i-10) * time.Minute)})
	}
	notes[0].At = now.Add(-30 * time.Hour)
	out := renderNotes(notes, "ops", now, time.UTC)
	if n := len([]rune(out)); n > maxNotesShown+200 {
		t.Fatalf("notes are %d runes", n)
	}
	lines := strings.Split(out, "\n")
	if !strings.HasPrefix(lines[0], "[") || !strings.Contains(lines[0], "older messages sent in this chat are not shown") {
		t.Fatalf("no dropped line: %q", lines[0])
	}
	if !strings.Contains(out, "note 9 ") || strings.Contains(out, "note 0 ") {
		t.Fatal("the newest notes should be kept")
	}
	if strings.Index(out, "note 8 ") > strings.Index(out, "note 9 ") {
		t.Fatal("notes should read oldest first")
	}
	if !strings.Contains(out, " …\"]") {
		t.Fatal("a long note should be clipped")
	}
	old := noteLine(notes[0], "boss", now, time.UTC)
	if !strings.HasPrefix(old, "[Earlier in this chat, on Sun 2026-11-08 at 13:00, agent ops's scheduled task \"t\" sent:") {
		t.Fatalf("old note: %q", old[:120])
	}
	if got := noteSender(state.ChatNote{}, "ops"); got != "Habakkuk" {
		t.Fatalf("anonymous sender = %q", got)
	}
	if got := noteSender(state.ChatNote{Agent: "ops"}, "ops"); got != "you (from another session)" {
		t.Fatalf("own sender = %q", got)
	}
}
