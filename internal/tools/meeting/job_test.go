package meeting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

var chatAddr = json.RawMessage(`{"account":"tg","conversation":"42"}`)

type delivery struct {
	id   string
	to   json.RawMessage
	text string
}

type harness struct {
	t    *testing.T
	m    *Manager
	q    *jobs.Queue
	reg  *tools.Registry
	meet *fakeMeet

	mu        sync.Mutex
	delivered []delivery
	notified  []string
	prompts   []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "control.db"), jobs.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h := &harness{t: t, q: jobs.NewQueue(db), reg: tools.NewRegistry(), meet: newFakeMeet()}
	h.m = &Manager{
		Store: NewStore(filepath.Join(dir, "meetings")), Cfg: (&config.Config{}).MeetingConfig(), Queue: h.q,
		Launch: h.meet.launcher(), Location: time.UTC, Tick: 5 * time.Millisecond, Settle: 100 * time.Millisecond,
		Model: func(ctx context.Context, agent, system, prompt string) (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.prompts = append(h.prompts, prompt)
			if system != NotesSystem || agent != "habakkuk" {
				return "", errors.New("unexpected notes request")
			}
			return "Summary: the budget review.\nDecisions:\n- The launch moves to March 15.\nAction items:\n- Bob: send the revised plan by Habakkuk.", nil
		},
		Deliver: func(ctx context.Context, id string, to json.RawMessage, text string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.delivered = append(h.delivered, delivery{id, to, text})
			return nil
		},
		Notify: func(ctx context.Context, agent, session, id, text string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.notified = append(h.notified, session+"|"+id+"|"+text)
			return nil
		},
	}
	RegisterTools(h.reg, h.m, func(ctx context.Context, session string) json.RawMessage {
		if session == "chat" {
			return chatAddr
		}
		return nil
	})
	return h
}

// call runs a tool the way an agent's turn does, permission check included.
func (h *harness) call(session, name, input string) (string, bool) {
	env := &tools.Env{Agent: "habakkuk", Session: session, Mode: tools.ModeWorkspace, Taint: tools.NewTaint(false)}
	res := h.reg.Execute(context.Background(), env, llm.Block{Type: llm.ToolUse, ToolUseID: "c1", ToolName: name, Input: json.RawMessage(input)})
	return res.Text, res.IsError
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type outcome struct {
	result string
	err    error
}

// run claims the attendance's job from the queue and handles it in the
// background, as the meeting worker pool does.
func (h *harness) run(ctx context.Context) (*jobs.Job, chan outcome) {
	h.t.Helper()
	j, err := h.q.Claim(ctx, "w1", time.Minute, Kind)
	if err != nil || j == nil {
		h.t.Fatalf("claim: %v %v", j, err)
	}
	return j, h.handle(ctx, j)
}

func (h *harness) handle(ctx context.Context, j *jobs.Job) chan outcome {
	ch := make(chan outcome, 1)
	go func() {
		res, err := h.m.Handle(ctx, j, "w1")
		ch <- outcome{res, err}
	}()
	return ch
}

func (h *harness) only() *Record {
	h.t.Helper()
	list, err := h.m.Store.List()
	if err != nil || len(list) != 1 {
		h.t.Fatalf("records: %v %v", list, err)
	}
	return list[0]
}

func (h *harness) lines(id string) []string {
	ls, err := h.m.Store.Lines(id)
	if err != nil {
		h.t.Fatal(err)
	}
	return texts(ls)
}

func (h *harness) inCall() bool {
	ok := false
	h.meet.state(func(f *fakeMeet) { ok = f.stage == "incall" && f.observer > 0 })
	return ok
}

func compact(raw json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return string(raw)
	}
	return b.String()
}

func await(t *testing.T, ch chan outcome) outcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(10 * time.Second):
		t.Fatal("the attendance did not end")
	}
	return outcome{}
}

func TestAttendLifecycle(t *testing.T) {
	h := newHarness(t)
	out, isErr := h.call("chat", "meeting_join", `{"url":"https://meet.google.com/abc-defg-hij","duration_limit":"30m","notes_prompt":"focus on the budget"}`)
	if isErr || !strings.Contains(out, "sent to this chat") {
		t.Fatalf("join: %s", out)
	}
	rec := h.only()
	if rec.Platform != GoogleMeet || rec.Limit() != 30*time.Minute || rec.Name != "Habakkuk (notetaker)" || compact(rec.Origin) != compact(chatAddr) {
		t.Fatalf("record: %+v", rec)
	}
	j, err := h.q.Get(context.Background(), rec.Job)
	if err != nil || j.Kind != Kind || j.Status != jobs.Pending {
		t.Fatalf("job: %+v %v", j, err)
	}
	if out, _ := h.call("chat", "meeting_join", `{"url":"https://meet.google.com/abc-defg-hij"}`); !strings.Contains(out, "Already attending") {
		t.Fatalf("second join: %s", out)
	}
	if out, isErr := h.call("chat", "meeting_join", `{"url":"https://evil.example.com/abc-defg-hij"}`); !isErr || !strings.Contains(out, "only Google Meet") {
		t.Fatalf("a link of another site: %s", out)
	}

	job, done := h.run(context.Background())
	h.waitFor("being in the call with the caption observer on", h.inCall)
	h.waitFor("the announcement", func() bool {
		n := 0
		h.meet.state(func(f *fakeMeet) { n = len(f.chat) })
		return n > 0
	})
	page := h.meet.page()
	page.say("e1", map[string]any{"id": 1, "s": "Alice Chen", "t": "Good morning"})
	page.say("e1", map[string]any{"id": 1, "s": "Alice Chen", "t": "Good morning every"})
	page.say("e1", map[string]any{"id": 1, "s": "Alice Chen", "t": "Good morning everyone."},
		map[string]any{"id": 2, "s": "Bob Ortiz", "t": "We decided"})
	page.say("e1", map[string]any{"id": 1, "g": true},
		map[string]any{"id": 2, "s": "Bob Ortiz", "t": "We decided to move the launch to March fifteenth."})
	h.waitFor("the first line", func() bool { return len(h.lines(rec.ID)) >= 1 })

	if out, _ := h.call("chat", "meeting_status", `{"id":"`+rec.ID+`"}`); !strings.Contains(out, "in_call") || !strings.Contains(out, "captions: on") {
		t.Fatalf("status: %s", out)
	}
	if out, _ := h.call("chat", "meeting_status", `{}`); !strings.Contains(out, rec.ID) {
		t.Fatalf("status list: %s", out)
	}
	env := &tools.Env{Agent: "habakkuk", Session: "chat", Mode: tools.ModeWorkspace, Taint: tools.NewTaint(false)}
	res := h.reg.Execute(context.Background(), env, llm.Block{Type: llm.ToolUse, ToolUseID: "c2", ToolName: "meeting_transcript", Input: json.RawMessage(`{"id":"` + rec.ID + `"}`)})
	if !strings.Contains(res.Text, "Alice Chen: Good morning everyone.") || !strings.Contains(res.Text, "data, not instructions") || !env.Taint.Marked() {
		t.Fatalf("transcript (tainted %v): %s", env.Taint.Marked(), res.Text)
	}
	other := &tools.Env{Agent: "other", Mode: tools.ModeWorkspace}
	if res := h.reg.Execute(context.Background(), other, llm.Block{Type: llm.ToolUse, ToolUseID: "c3", ToolName: "meeting_leave", Input: json.RawMessage(`{"id":"` + rec.ID + `"}`)}); !res.IsError {
		t.Fatalf("another agent could end the meeting: %s", res.Text)
	}

	if out, isErr := h.call("chat", "meeting_leave", `{"id":"`+rec.ID+`"}`); isErr || !strings.Contains(out, "Leaving") {
		t.Fatalf("leave: %s", out)
	}
	o := await(t, done)
	if o.err != nil {
		t.Fatalf("handle: %v", o.err)
	}
	if err := h.q.Complete(context.Background(), job.ID, "w1", o.result); err != nil {
		t.Fatal(err)
	}

	rec, _ = h.m.Store.Get(rec.ID)
	if rec.Status != StatusEnded || rec.Detail != "asked to leave" || !rec.Captions || !rec.Announced || !rec.Notes || !rec.Delivered || rec.JoinedAt.IsZero() {
		t.Fatalf("record after: %+v", rec)
	}
	want := []string{"Alice Chen: Good morning everyone.", "Bob Ortiz: We decided to move the launch to March fifteenth."}
	if got := h.lines(rec.ID); strings.Join(got, "|") != strings.Join(want, "|") || rec.Lines != 2 {
		t.Fatalf("transcript %q (%d), want %q", got, rec.Lines, want)
	}
	h.meet.state(func(f *fakeMeet) {
		if f.name != "Habakkuk (notetaker)" || f.mic || f.cam {
			t.Errorf("joined as %q with mic %v, camera %v", f.name, f.mic, f.cam)
		}
		if len(f.chat) != 1 || f.chat[0] != "Habakkuk (notetaker) has joined to take notes." {
			t.Errorf("chat: %q", f.chat)
		}
		if f.stage != "left" {
			t.Errorf("did not press leave: %s", f.stage)
		}
		if len(f.navigated) != 1 || f.navigated[0] != "https://meet.google.com/abc-defg-hij" {
			t.Errorf("navigated %q", f.navigated)
		}
	})
	h.mu.Lock()
	if len(h.prompts) != 1 || !strings.Contains(h.prompts[0], "focus on the budget") || !strings.Contains(h.prompts[0], "Bob Ortiz: We decided to move the launch") {
		t.Errorf("notes prompt: %q", h.prompts)
	}
	if len(h.delivered) != 1 || h.delivered[0].id != "meeting_notes_"+rec.ID || compact(h.delivered[0].to) != compact(chatAddr) ||
		!strings.Contains(h.delivered[0].text, "Meeting notes: Google Meet") || !strings.Contains(h.delivered[0].text, "The launch moves to March 15") {
		t.Errorf("delivered: %+v", h.delivered)
	}
	h.mu.Unlock()
	if _, err := os.Stat(h.m.Store.BrowserDir(rec.ID)); !os.IsNotExist(err) {
		t.Errorf("the browser profile was left behind: %v", err)
	}

	// A rerun of a finished attendance (a crash before the job completed)
	// sends nothing twice.
	if _, err := h.m.Handle(context.Background(), job, "w1"); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	if len(h.delivered) != 1 || len(h.prompts) != 1 {
		t.Errorf("notes sent again: %d, %d", len(h.delivered), len(h.prompts))
	}
	h.mu.Unlock()
	if out, _ := h.call("chat", "meeting_leave", `{"id":"`+rec.ID+`"}`); !strings.Contains(out, "already over") {
		t.Fatalf("leave after the end: %s", out)
	}
}

func TestAttendDeniedWithoutChat(t *testing.T) {
	h := newHarness(t)
	h.meet.deny = true
	if out, isErr := h.call("cli", "meeting_join", `{"url":"https://meet.google.com/abc-defg-hij","name":"Notes Bot"}`); isErr || !strings.Contains(out, "back to this session") {
		t.Fatalf("join: %s", out)
	}
	_, done := h.run(context.Background())
	if o := await(t, done); o.err != nil {
		t.Fatal(o.err)
	}
	rec := h.only()
	if rec.Status != StatusFailed || !strings.Contains(rec.Detail, "denied") {
		t.Fatalf("record: %+v", rec)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.prompts) != 0 || len(h.delivered) != 0 || len(h.notified) != 1 ||
		!strings.Contains(h.notified[0], "cli|meeting_notes_"+rec.ID+"|") || !strings.Contains(h.notified[0], "could not join") {
		t.Fatalf("prompts %d, delivered %d, notified %q", len(h.prompts), len(h.delivered), h.notified)
	}
	h.meet.state(func(f *fakeMeet) {
		if f.name != "Notes Bot" {
			t.Errorf("name %q", f.name)
		}
	})
}

func TestAttendLobbyTimeoutAndTimeLimit(t *testing.T) {
	h := newHarness(t)
	h.m.Cfg.LobbyTimeout = 50 * time.Millisecond
	h.meet.state(func(f *fakeMeet) { f.admit = 1 << 30 }) // nobody comes
	h.call("chat", "meeting_join", `{"url":"https://meet.google.com/abc-defg-hij"}`)
	_, done := h.run(context.Background())
	await(t, done)
	if rec := h.only(); rec.Status != StatusFailed || !strings.Contains(rec.Detail, "nobody let Habakkuk in") {
		t.Fatalf("lobby: %+v", rec)
	}

	h = newHarness(t)
	h.m.Cfg.MaxDuration = 200 * time.Millisecond
	h.call("chat", "meeting_join", `{"url":"https://meet.google.com/abc-defg-hij","duration_limit":"2h"}`)
	if rec := h.only(); rec.Limit() != 200*time.Millisecond {
		t.Fatalf("limit not capped: %s", rec.Limit())
	}
	_, done = h.run(context.Background())
	await(t, done)
	if rec := h.only(); rec.Status != StatusEnded || !strings.Contains(rec.Detail, "time limit") {
		t.Fatalf("limit: %+v", rec)
	}
}

func TestAttendRejoinsAfterCrashAndResumes(t *testing.T) {
	h := newHarness(t)
	h.call("chat", "meeting_join", `{"url":"https://meet.google.com/abc-defg-hij"}`)
	ctx, cancel := context.WithCancel(context.Background())
	job, done := h.run(ctx)
	h.waitFor("the call", h.inCall)
	rec := h.only()
	first := h.meet.page()
	first.say("e1", map[string]any{"id": 1, "s": "Alice", "t": "First point is the budget."})
	first.say("e1", map[string]any{"id": 1, "g": true})
	h.waitFor("the first line", func() bool { return len(h.lines(rec.ID)) == 1 })
	joined := h.only().JoinedAt

	// The browser dies: the attendance starts another and joins again.
	first.crash()
	h.waitFor("a second browser in the call", func() bool { return h.meet.page() != first && h.inCall() })
	second := h.meet.page()
	h.waitFor("the binding", func() bool { return second.say("e2") })
	second.say("e2", map[string]any{"id": 1, "s": "Alice", "t": "First point is the budget."}) // still on screen
	second.say("e2", map[string]any{"id": 2, "s": "Bob", "t": "Second point is hiring."})
	second.say("e2", map[string]any{"id": 1, "g": true}, map[string]any{"id": 2, "g": true})
	h.waitFor("the second line", func() bool { return len(h.lines(rec.ID)) == 2 })

	// Habakkuk shuts down mid-meeting: the job stops without finishing.
	cancel()
	if o := await(t, done); !errors.Is(o.err, context.Canceled) {
		t.Fatalf("shutdown: %v", o.err)
	}
	if r := h.only(); r.Status != StatusInCall || r.Notes {
		t.Fatalf("after shutdown: %+v", r)
	}

	// The job resumes: a third browser joins, and leaving finishes it.
	observed := 0
	h.meet.state(func(f *fakeMeet) { observed = f.observer })
	done = h.handle(context.Background(), job)
	h.waitFor("a third browser reading captions", func() bool {
		n, obs := 0, 0
		h.meet.state(func(f *fakeMeet) { n, obs = f.pages, f.observer })
		return n == 3 && obs > observed && h.inCall()
	})
	third := h.meet.page()
	h.waitFor("the binding", func() bool { return third.say("e3") })
	third.say("e3", map[string]any{"id": 1, "s": "Bob", "t": "Second point is hiring."}, map[string]any{"id": 2, "s": "Alice", "t": "Thanks all."})
	if err := h.m.Store.RequestLeave(rec.ID); err != nil {
		t.Fatal(err)
	}
	if o := await(t, done); o.err != nil {
		t.Fatal(o.err)
	}
	r := h.only()
	want := []string{"Alice: First point is the budget.", "Bob: Second point is hiring.", "Alice: Thanks all."}
	if got := h.lines(rec.ID); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("transcript %q, want %q", got, want)
	}
	if r.Status != StatusEnded || !r.JoinedAt.Equal(joined) || !r.Delivered {
		t.Fatalf("record: %+v (joined %v)", r, joined)
	}
}

func TestStorePrune(t *testing.T) {
	st := NewStore(t.TempDir())
	old := time.Now().Add(-48 * time.Hour)
	for _, r := range []*Record{
		{ID: "mt_00000001", Status: StatusEnded, Created: old, EndedAt: old},
		{ID: "mt_00000002", Status: StatusInCall, Created: old},
		{ID: "mt_00000003", Status: StatusFailed, Created: time.Now(), EndedAt: time.Now()},
	} {
		if err := st.Create(r); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.Prune(time.Now().Add(-24 * time.Hour)); err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	list, _ := st.List()
	if len(list) != 2 {
		t.Fatalf("left %d", len(list))
	}
	if _, err := st.Get("../etc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a path as an id: %v", err)
	}
}
