package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/state"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// editor is a recording adapter that can also post, edit and delete, and
// keeps one ordered log of everything it did.
type editor struct {
	*recorder
	mu   sync.Mutex
	log  []string
	next int
}

func (e *editor) Send(ctx context.Context, to Address, text, replyTo string) error {
	e.mu.Lock()
	e.log = append(e.log, "send "+text)
	e.mu.Unlock()
	return e.recorder.Send(ctx, to, text, replyTo)
}
func (e *editor) Post(_ context.Context, _ Address, text string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.next++
	id := fmt.Sprint(e.next)
	e.log = append(e.log, "post "+id+" "+text)
	return id, nil
}
func (e *editor) Edit(_ context.Context, _ Address, id, text string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, "edit "+id+" "+text)
	return nil
}
func (e *editor) Delete(_ context.Context, _ Address, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, "delete "+id)
	return nil
}
func (e *editor) entries() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.log)
}

// stepModel answers with a function of the request, after an optional delay.
type stepModel struct {
	mu    sync.Mutex
	delay time.Duration
	reqs  []llm.Request
	fn    func(req llm.Request, last llm.Message) *llm.Response
}

func (m *stepModel) Send(ctx context.Context, _ []llm.Target, req llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	m.mu.Lock()
	m.reqs = append(m.reqs, req)
	delay := m.delay
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(delay):
	}
	return m.fn(req, req.Messages[len(req.Messages)-1]), nil
}

func toolCall(id, name, input string) *llm.Response {
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{
		{Type: llm.ToolUse, ToolUseID: id, ToolName: name, Input: json.RawMessage(input)},
	}}, StopReason: llm.StopToolUse}
}

func newStateRig(t *testing.T, ch *config.Channel) *rig {
	t.Helper()
	return openStateRig(t, t.TempDir(), ch)
}

// openStateRig is openRig with state.db, also in dir.
func openStateRig(t *testing.T, dir string, ch *config.Channel, prepare ...func(*rig)) *rig {
	t.Helper()
	st, err := state.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	withState := func(r *rig) {
		r.g.State = st
		r.g.Runner.Overlay = state.Overlay{Store: st, Personas: r.g.Personas}
	}
	return openRig(t, dir, ch, append([]func(*rig){withState}, prepare...)...)
}

func send(r *rig, id, text string) { r.g.Handle(context.Background(), dm(id, text)) }

func TestHelpListsCommands(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	send(r, "m1", "/help")
	waitFor(t, "help", hasText(r.a, "/compact"))
	help := r.a.texts()[0]
	for _, c := range []string{"/retry", "/btw", "/steer", "/queue", "/think", "/personality", "/goal", "/export", "/whoami", "/agents", "/agent "} {
		if !strings.Contains(help, c) {
			t.Errorf("/help lacks %s", c)
		}
	}
}

func TestRetryRerunsTheLastUserMessage(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	send(r, "m1", "what is two plus two")
	waitFor(t, "first reply", hasText(r.a, "echo: what is two plus two"))
	send(r, "m2", "/retry")
	waitFor(t, "retry reply", hasText(r.a, "try this message again; answer it afresh.]\nwhat is two plus two"))
	send(r, "m3", "/retry") // the retry itself is not what gets retried
	waitFor(t, "second retry", func() bool { return len(r.a.texts()) == 3 })
	if last := r.a.texts()[2]; strings.Count(last, "try this message again") != 1 {
		t.Fatalf("retried a retry: %q", last)
	}
}

func TestThinkAndPersonalityChangeTheNextTurn(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	send(r, "m1", "/think max")
	waitFor(t, "think ack", hasText(r.a, "set to max"))
	send(r, "m2", "/personality pirate")
	waitFor(t, "unknown persona", hasText(r.a, `I don't know the persona "pirate"`))
	send(r, "m3", "/personality concise")
	waitFor(t, "persona ack", hasText(r.a, "Persona set to concise"))
	send(r, "m4", "hi")
	waitFor(t, "reply", hasText(r.a, "echo: hi"))
	r.model.mu.Lock()
	req := r.model.last
	r.model.mu.Unlock()
	if req.Effort != "max" || !strings.Contains(req.System, "concise persona") {
		t.Fatalf("effort %q, system %q", req.Effort, req.System)
	}
	send(r, "m5", "/think")
	waitFor(t, "think status", hasText(r.a, "Thinking effort: max"))
	send(r, "m6", "/think loud")
	waitFor(t, "bad level", hasText(r.a, "Pick one of"))
}

func TestSteerWhileBusyBecomesAFollowUp(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	r.model.delay = 400 * time.Millisecond
	send(r, "m1", "write the report")
	waitFor(t, "turn running", func() bool { b, _ := r.q.Busy(context.Background(), r.session(t), ""); return b })
	send(r, "m2", "/steer use metric units")
	waitFor(t, "steer ack", hasText(r.a, "I'll take that in"))
	waitFor(t, "follow-up", hasText(r.a, "Guidance the user sent while you were working"))
	if !hasText(r.a, "use metric units")() {
		t.Fatal("guidance lost")
	}
}

func TestSteerWhenIdleIsAMessage(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	send(r, "m1", "/steer just do it")
	waitFor(t, "reply", hasText(r.a, "echo: just do it"))
	if !hasText(r.a, "Nothing is running")() {
		t.Fatal("no notice that the steer became a message")
	}
}

func TestQueueRunsAfterTheCurrentTurn(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	r.model.delay = 300 * time.Millisecond
	send(r, "m1", "first")
	waitFor(t, "turn running", func() bool { b, _ := r.q.Busy(context.Background(), r.session(t), ""); return b })
	send(r, "m2", "/queue second")
	waitFor(t, "queued ack", hasText(r.a, "Queued."))
	waitFor(t, "both replies", hasText(r.a, "echo: second"))
	texts := r.a.texts()
	if slices.Index(texts, "echo: first") > slices.Index(texts, "echo: second") {
		t.Fatalf("out of order: %q", texts)
	}
}

func TestGoalRunsUntilBudget(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	r.model.reply = "made progress"
	send(r, "m1", "/goal -turns 2 tidy the workspace")
	waitFor(t, "goal stopped", hasText(r.a, "Goal stopped after 2 turns"))
	n := 0
	for _, s := range r.a.texts() {
		if s == "made progress" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d goal turns, want 2: %q", n, r.a.texts())
	}
	if g, _ := r.g.State.Goal(context.Background(), r.session(t)); g != nil {
		t.Fatal("goal not cleared")
	}
}

func TestGoalEndsWhenDone(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	r.model.reply = "All tidy.\n" + GoalDone
	send(r, "m1", "/goal tidy the workspace")
	waitFor(t, "goal reached", hasText(r.a, "Goal reached: tidy the workspace"))
	r.model.mu.Lock()
	calls, last := r.model.calls, r.model.last
	r.model.mu.Unlock()
	if calls != 1 || !strings.Contains(last.Messages[0].Text(), "[Standing goal: turn 1 of at most 10]") {
		t.Fatalf("calls %d, prompt %q", calls, last.Messages[0].Text())
	}
	send(r, "m2", "/goal")
	waitFor(t, "status", hasText(r.a, "No standing goal"))
}

func TestBtwStaysOutOfTheMainConversation(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	send(r, "m1", "the code word is heron")
	waitFor(t, "reply", hasText(r.a, "echo: the code word is heron"))
	main := r.session(t)
	send(r, "m2", "/btw what is the code word?")
	waitFor(t, "side answer", hasText(r.a, "Question: what is the code word?"))
	if !hasText(r.a, "[user] [")() || !hasText(r.a, "the code word is heron")() {
		t.Fatalf("side question lacks the context: %q", r.a.texts())
	}
	recs, _ := r.g.Runner.Sessions.Messages(context.Background(), main)
	if len(recs) != 2 || r.session(t) != main {
		t.Fatalf("main conversation changed: %d messages", len(recs))
	}
}

func TestExportSendsTheTranscript(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	send(r, "m1", "remember the heron")
	waitFor(t, "reply", hasText(r.a, "echo: remember the heron"))
	send(r, "m2", "/export")
	waitFor(t, "file", func() bool { return len(r.a.sentFiles()) == 1 })
	f := r.a.sentFiles()[0]
	if !strings.HasPrefix(f.Name, "conversation-ses_") || f.MediaType != "text/markdown" ||
		!strings.Contains(f.Data, "## User") || !strings.Contains(f.Data, "remember the heron") || !strings.Contains(f.Data, "## habakkuk") {
		t.Fatalf("export = %+v", f)
	}
}

func TestWhoamiAndAgentSwitch(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	spec := *r.g.Runner.Agents["habakkuk"]
	spec.Name = "scout"
	r.g.Runner.Agents["scout"] = &spec
	send(r, "m1", "/whoami")
	waitFor(t, "whoami", hasText(r.a, "You: Ay (7)"))
	if !hasText(r.a, "listed in allow_from")() || !hasText(r.a, "Agent: habakkuk")() {
		t.Fatalf("whoami = %q", r.a.texts())
	}
	send(r, "m2", "/agents")
	waitFor(t, "agents", hasText(r.a, "- scout, fake/m"))
	send(r, "m3", "/agent nobody")
	waitFor(t, "unknown", hasText(r.a, `There's no agent "nobody"`))
	send(r, "m4", "/agent scout")
	waitFor(t, "switched", hasText(r.a, "You're now talking to scout"))
	send(r, "m5", "hello scout")
	waitFor(t, "reply", hasText(r.a, "echo: hello scout"))
	conv, _ := r.g.Store.Conversation(context.Background(), Address{Account: "tg", Conversation: "100"})
	if conv.Agent != "scout" {
		t.Fatalf("conversation agent = %s", conv.Agent)
	}
	send(r, "m6", "/agent default")
	waitFor(t, "back", hasText(r.a, "You're now talking to habakkuk"))
}

func TestClarifyAsksAndWaits(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	r.g.Runner.Models = &stepModel{fn: func(_ llm.Request, last llm.Message) *llm.Response {
		for _, b := range last.Blocks {
			if b.Type == llm.ToolResult {
				return text("got: " + b.Text)
			}
		}
		return toolCall("q1", "clarify", `{"questions":[{"question":"Which colour?","choices":["red","blue"]}]}`)
	}}
	send(r, "m1", "paint it")
	waitFor(t, "question", hasText(r.a, "**Question:** Which colour?\n   a) red\n   b) blue"))
	send(r, "m2", "blue please")
	waitFor(t, "answer used", hasText(r.a, "got: The user answered:\nblue please"))
}

func TestProgressDraftIsReplacedByTheReply(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}, Progress: config.ProgressDetailed})
	ed := &editor{recorder: r.a}
	r.g.Adapters["tg"] = ed
	r.g.ProgressAfter, r.g.ProgressEvery = 30*time.Millisecond, 20*time.Millisecond
	model := &stepModel{delay: 150 * time.Millisecond, fn: func(_ llm.Request, last llm.Message) *llm.Response {
		for _, b := range last.Blocks {
			if b.Type == llm.ToolResult {
				return text("all done")
			}
		}
		return toolCall("c1", "list_dir", `{"path":"."}`)
	}}
	r.g.Runner.Models = model
	send(r, "m1", "look around")
	waitFor(t, "reply", hasText(r.a, "all done"))
	log := ed.entries()
	post := slices.IndexFunc(log, func(s string) bool { return strings.HasPrefix(s, "post 1 ") })
	del := slices.Index(log, "delete 1")
	reply := slices.Index(log, "send all done")
	if post < 0 || del < 0 || post > del || del > reply {
		t.Fatalf("want post, then delete, then the reply: %q", log)
	}
	if !slices.ContainsFunc(log, func(s string) bool { return strings.Contains(s, "→ list_dir `.`") }) {
		t.Fatalf("no tool line in the draft: %q", log)
	}
	if d, _ := r.g.State.Draft(context.Background(), TurnID(dm("m1", ""))); d != nil {
		t.Fatal("draft still recorded after the reply")
	}
}

func TestProgressOff(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}, Progress: config.ProgressOff})
	ed := &editor{recorder: r.a}
	r.g.Adapters["tg"] = ed
	r.g.ProgressAfter = 10 * time.Millisecond
	r.model.delay = 100 * time.Millisecond
	send(r, "m1", "hello")
	waitFor(t, "reply", hasText(r.a, "echo: hello"))
	for _, e := range ed.entries() {
		if strings.HasPrefix(e, "post") {
			t.Fatalf("progress off but a draft was posted: %q", ed.entries())
		}
	}
}

// A silent turn removes its draft and sends nothing.
func TestSilentTurnRemovesItsDraft(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ed := &editor{recorder: r.a}
	r.g.Adapters["tg"] = ed
	r.g.ProgressAfter = 10 * time.Millisecond
	r.model.delay = 150 * time.Millisecond
	r.model.reply = Silent
	send(r, "m1", "anything new?")
	waitFor(t, "draft removed", func() bool { return slices.Contains(ed.entries(), "delete 1") })
	time.Sleep(50 * time.Millisecond)
	if n := len(r.a.texts()); n != 0 {
		t.Fatalf("a silent turn sent %d messages", n)
	}
}

type hookLog struct {
	mu     sync.Mutex
	events []string
}

func (h *hookLog) Fire(_ context.Context, event, subject string, _ any) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event+":"+subject)
	return ""
}

func TestMessageHooks(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	hl := &hookLog{}
	r.g.Runner.Hooks = hl
	send(r, "m1", "hi")
	waitFor(t, "reply", hasText(r.a, "echo: hi"))
	waitFor(t, "message_out", func() bool {
		hl.mu.Lock()
		defer hl.mu.Unlock()
		return slices.Contains(hl.events, "message_out:tg")
	})
	hl.mu.Lock()
	defer hl.mu.Unlock()
	for _, want := range []string{"message_in:tg", "session_new:habakkuk", "turn_start:habakkuk", "turn_end:habakkuk"} {
		if !slices.Contains(hl.events, want) {
			t.Errorf("no %s in %q", want, hl.events)
		}
	}
}

// session is the session bound to the test DM.
func (r *rig) session(t *testing.T) string {
	t.Helper()
	conv, err := r.g.Store.Conversation(context.Background(), Address{Account: "tg", Conversation: "100"})
	if err != nil || conv == nil {
		t.Fatalf("no conversation: %v", err)
	}
	return conv.SessionID
}
