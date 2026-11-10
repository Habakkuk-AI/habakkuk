package tui

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// fakeBackend records what the UI asks of it.
type fakeBackend struct {
	mu        sync.Mutex
	sessions  []SessionInfo
	recs      map[string][]session.Record
	submitted []string
	commands  []string
	canceled  int
	busy      bool
	evs       map[string]chan events.Event
	approvers map[string]tools.Approver
	followUp  bool
	exported  string
}

func newFake() *fakeBackend {
	return &fakeBackend{recs: map[string][]session.Record{}, evs: map[string]chan events.Event{}, approvers: map[string]tools.Approver{}}
}

func (f *fakeBackend) Agents() []string { return []string{"coder", "habakkuk"} }

func (f *fakeBackend) NewSession(_ context.Context, agent string) (SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := SessionInfo{ID: "s" + string(rune('0'+len(f.sessions)+1)), Agent: agent, UpdatedAt: time.Now()}
	f.sessions = append(f.sessions, s)
	return s, nil
}

func (f *fakeBackend) Session(_ context.Context, id string) (SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions {
		if s.ID == id {
			return s, nil
		}
	}
	return SessionInfo{}, errors.New("no such session")
}

func (f *fakeBackend) RecentSessions(context.Context, int) ([]SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sessions), nil
}

func (f *fakeBackend) Transcript(_ context.Context, id string) ([]session.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recs[id], nil
}

func (f *fakeBackend) Busy(context.Context, string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy
}

func (f *fakeBackend) Submit(_ context.Context, _, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitted = append(f.submitted, text)
	f.busy = true
	return nil
}

func (f *fakeBackend) Cancel(context.Context, string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canceled++
	return 1
}

func (f *fakeBackend) Subscribe(id string) (<-chan events.Event, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan events.Event, 64)
	f.evs[id] = ch
	return ch, func() {}
}

func (f *fakeBackend) Attach(id string, a tools.Approver) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approvers[id] = a
	return func() {}
}

func (f *fakeBackend) Status(context.Context, string) Status {
	return Status{Model: "anthropic/test-model", Calls: 2, In: 12300, Out: 456, Cost: "$0.04"}
}

func (f *fakeBackend) Command(_ context.Context, _, name, arg string) (CommandResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, strings.TrimSpace(name+" "+arg))
	return CommandResult{Reply: "ran " + name, Started: name == "/compact"}, nil
}

func (f *fakeBackend) FollowUp(context.Context, string) bool { return f.followUp }

func (f *fakeBackend) Export(_ context.Context, id, path string) (string, error) {
	f.exported = id
	return "/tmp/" + id + ".md", nil
}

// harness drives a Model the way the program does: each command runs, and
// what it returns goes back into Update. Commands that block (ticks, event
// waits with nothing queued) are abandoned after a short wait.
type harness struct {
	t     *testing.T
	m     *Model
	be    *fakeBackend
	br    *bridge
	clock time.Time
	quit  bool
}

func newHarness(t *testing.T, o Options) *harness {
	t.Helper()
	h := &harness{t: t, be: newFake(), br: newBridge(), clock: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	o.Theme = NewTheme(true, true)
	o.Now = func() time.Time { return h.clock }
	if o.Agent == "" {
		o.Agent = "habakkuk"
	}
	h.m = New(h.be, o, h.br)
	h.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	h.run(h.m.Init())
	t.Cleanup(func() { close(h.br.done); h.m.closeSession() })
	return h
}

func (h *harness) send(msg tea.Msg) {
	h.t.Helper()
	_, cmd := h.m.Update(msg)
	h.run(cmd)
}

func (h *harness) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	h.runAll([]tea.Cmd{cmd})
}

// runAll runs cmds at once and handles, in order, the messages of those
// that finish within a short wait.
func (h *harness) runAll(cmds []tea.Cmd) {
	results := make([]chan tea.Msg, len(cmds))
	for i, c := range cmds {
		results[i] = make(chan tea.Msg, 1)
		if c == nil {
			results[i] <- nil
			continue
		}
		go func() { results[i] <- c() }()
	}
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	expired := false
	var msgs []tea.Msg
	for _, ch := range results {
		if expired {
			select {
			case m := <-ch:
				msgs = append(msgs, m)
			default:
			}
			continue
		}
		select {
		case m := <-ch:
			msgs = append(msgs, m)
		case <-timer.C:
			expired = true
		}
	}
	for _, msg := range msgs {
		switch msg := msg.(type) {
		case nil, spinner.TickMsg:
		case tea.BatchMsg:
			h.runAll(msg)
		case tea.QuitMsg:
			h.quit = true
		default:
			h.send(msg)
		}
	}
}

func (h *harness) key(keys ...string) {
	h.t.Helper()
	for _, k := range keys {
		h.send(keyMsg(k))
	}
}

func keyMsg(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "alt+enter":
		return tea.KeyMsg{Type: tea.KeyEnter, Alt: true}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "ctrl+o":
		return tea.KeyMsg{Type: tea.KeyCtrlO}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

func (h *harness) typ(text string) {
	h.t.Helper()
	for _, r := range text {
		h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// emit hands events of the shown session to the model, as its event
// subscription would.
func (h *harness) emit(evs ...events.Event) {
	h.t.Helper()
	for _, e := range evs {
		e.Session = h.m.sess.ID
		h.send(eventMsg{session: h.m.sess.ID, ev: e})
	}
}

func (h *harness) view() string { return ansi.Strip(h.m.View()) }

func kinds(es []*entry) []entryKind {
	out := make([]entryKind, len(es))
	for i, e := range es {
		out[i] = e.kind
	}
	return out
}

func TestStartOpensNewSession(t *testing.T) {
	h := newHarness(t, Options{})
	if h.m.sess.ID != "s1" || h.m.sess.Agent != "habakkuk" {
		t.Fatalf("session = %+v", h.m.sess)
	}
	if h.be.approvers["s1"] == nil {
		t.Fatal("no approver attached")
	}
	v := h.view()
	for _, want := range []string{"habakkuk", "anthropic/test-model", "s1", "12.3k in", "456 out", "$0.04"} {
		if !strings.Contains(v, want) {
			t.Errorf("status bar lacks %q:\n%s", want, v)
		}
	}
}

// TestEmptyChatSuggestsStarters: a new chat is not a blank screen; it
// offers things to try, and they go once the conversation starts.
func TestEmptyChatSuggestsStarters(t *testing.T) {
	h := newHarness(t, Options{})
	if v := h.view(); !strings.Contains(v, "Every weekday at 8, send me a short brief of my day.") {
		t.Fatalf("no starters in an empty chat:\n%s", v)
	}
	h.typ("hello there")
	h.key("enter")
	if v := h.view(); strings.Contains(v, "Say hi, or try one of these") {
		t.Fatalf("starters stayed after the first message:\n%s", v)
	}
}

func TestSendAndStream(t *testing.T) {
	h := newHarness(t, Options{})
	h.typ("hello there")
	h.key("enter")
	if got := h.be.submitted; len(got) != 1 || got[0] != "hello there" {
		t.Fatalf("submitted %q", got)
	}
	if !h.m.busy || h.m.input.Value() != "" {
		t.Fatalf("busy=%v input=%q", h.m.busy, h.m.input.Value())
	}
	start := h.clock
	h.emit(
		events.Event{Kind: events.Thinking, Text: "let me "},
		events.Event{Kind: events.Thinking, Text: "look"},
		events.Event{Kind: events.Text, Text: "Checking "},
		events.Event{Kind: events.Text, Text: "the **files**."},
		events.Event{Kind: events.ToolStart, ToolID: "t1", Tool: "exec", Text: "exec ls", Input: json.RawMessage(`{"command":"ls -la"}`)},
	)
	h.clock = start.Add(1500 * time.Millisecond)
	h.emit(
		events.Event{Kind: events.ToolDone, ToolID: "t1", Tool: "exec", Text: "exec: a.txt b.txt"},
		events.Event{Kind: events.Text, Text: "Found two files."},
	)
	want := []entryKind{kindUser, kindThinking, kindAssistant, kindTool, kindAssistant}
	if got := kinds(h.m.entries); !slices.Equal(got, want) {
		t.Fatalf("entries %v, want %v", got, want)
	}
	if e := h.m.entries[1]; e.text != "let me look" || e.streaming {
		t.Errorf("thinking %+v", e)
	}
	if e := h.m.entries[2]; e.text != "Checking the **files**." {
		t.Errorf("assistant %q", e.text)
	}
	tool := h.m.entries[3]
	if tool.status != toolOK || tool.preview != "a.txt b.txt" || tool.end.Sub(tool.start) != 1500*time.Millisecond {
		t.Errorf("tool %+v", tool)
	}
	if !h.m.entries[4].streaming {
		t.Error("last text should still be streaming")
	}
	h.emit(events.Event{Kind: events.TurnDone, Text: "Found two files."})
	if h.m.entries[4].streaming || len(h.m.entries) != 5 {
		t.Fatalf("after turn done: %v", kinds(h.m.entries))
	}
	v := h.view()
	for _, want := range []string{"› hello there", "thinking (1 line", "Checking the **files**.", "✓ exec ls -la  1.5s", "└ a.txt b.txt", "Found two files."} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	// The turn is over once the backend says so.
	h.be.busy = false
	h.send(pollMsg{session: "s1", gen: h.m.pollGen})
	if h.m.busy {
		t.Error("still busy after the backend went idle")
	}
}

func TestFinalTextWithoutStreaming(t *testing.T) {
	h := newHarness(t, Options{})
	h.typ("hi")
	h.key("enter")
	h.emit(events.Event{Kind: events.TurnDone, Text: "Hello!"})
	if got := kinds(h.m.entries); !slices.Equal(got, []entryKind{kindUser, kindAssistant}) || h.m.entries[1].text != "Hello!" {
		t.Fatalf("entries %v", got)
	}
}

func TestToolExpandsWithFullOutput(t *testing.T) {
	h := newHarness(t, Options{})
	h.typ("go")
	h.key("enter")
	h.emit(
		events.Event{Kind: events.ToolStart, ToolID: "t1", Tool: "read_file", Input: json.RawMessage(`{"path":"notes.md"}`)},
		events.Event{Kind: events.ToolDone, ToolID: "t1", Tool: "read_file", Text: "read_file: line one"},
	)
	h.be.recs["s1"] = []session.Record{{Message: llm.Message{Role: llm.User, Blocks: []llm.Block{
		{Type: llm.ToolResult, ToolUseID: "t1", Text: "line one\nline two\nline three"}}}}}
	h.key("ctrl+o")
	v := h.view()
	for _, want := range []string{"┌ input", `"path": "notes.md"`, "┌ output", "│ line three"} {
		if !strings.Contains(v, want) {
			t.Errorf("expanded view lacks %q:\n%s", want, v)
		}
	}
}

func TestEnterAltEnterAndHistory(t *testing.T) {
	h := newHarness(t, Options{})
	h.typ("first")
	h.key("alt+enter")
	h.typ("line")
	if v := h.m.input.Value(); v != "first\nline" {
		t.Fatalf("input %q", v)
	}
	if v := h.view(); !strings.Contains(v, "│ first") || !strings.Contains(v, "│ line") {
		t.Fatalf("the input box does not show both lines:\n%s", v)
	}
	h.key("enter")
	h.typ("second")
	h.key("enter")
	if !slices.Equal(h.be.submitted, []string{"first\nline", "second"}) {
		t.Fatalf("submitted %q", h.be.submitted)
	}
	h.typ("draft")
	h.key("up")
	if v := h.m.input.Value(); v != "second" {
		t.Fatalf("up recalled %q", v)
	}
	h.key("up")
	if v := h.m.input.Value(); v != "first\nline" {
		t.Fatalf("second up recalled %q", v)
	}
	h.key("down")
	h.key("down")
	if v := h.m.input.Value(); v != "draft" {
		t.Fatalf("down back to draft gave %q", v)
	}
}

func TestAutocomplete(t *testing.T) {
	h := newHarness(t, Options{})
	h.typ("/mo")
	if len(h.m.popup) != 1 || h.m.popup[0].value != "/model " {
		t.Fatalf("popup %+v", h.m.popup)
	}
	if !strings.Contains(h.view(), "/model [provider/model|default]") {
		t.Error("popup not drawn")
	}
	h.key("tab")
	if v := h.m.input.Value(); v != "/model " {
		t.Fatalf("tab gave %q", v)
	}
	h.m.input.Reset()
	h.m.afterInput()

	h.typ("/th")
	h.key("tab")
	h.typ("h")
	if len(h.m.popup) != 1 || h.m.popup[0].label != "high" {
		t.Fatalf("argument popup %+v", h.m.popup)
	}
	h.key("enter")
	if !slices.Equal(h.be.commands, []string{"/think high"}) {
		t.Fatalf("commands %q", h.be.commands)
	}
	if last := h.m.entries[len(h.m.entries)-1]; last.text != "ran /think" {
		t.Errorf("reply %q", last.text)
	}

	// Several matches: arrows move, Esc closes until the input changes.
	h.typ("/s")
	if len(h.m.popup) < 3 {
		t.Fatalf("popup for /s: %+v", h.m.popup)
	}
	h.key("down")
	if h.m.popupSel != 1 {
		t.Errorf("selection %d", h.m.popupSel)
	}
	h.key("esc")
	if h.m.popup != nil {
		t.Error("esc left the popup open")
	}
	h.typ("t")
	if len(h.m.popup) == 0 {
		t.Error("popup did not come back after typing")
	}
}

func TestLocalCommands(t *testing.T) {
	h := newHarness(t, Options{})
	h.typ("/help")
	h.key("enter")
	if last := h.m.entries[len(h.m.entries)-1]; !strings.Contains(last.text, "/sessions") {
		t.Fatalf("help: %q", last.text)
	}
	h.typ("/new")
	h.key("enter")
	if h.m.sess.ID != "s2" {
		t.Fatalf("/new opened %q", h.m.sess.ID)
	}
	h.typ("/agent coder")
	h.key("enter")
	if h.m.sess.Agent != "coder" || h.m.sess.ID != "s3" {
		t.Fatalf("/agent gave %+v", h.m.sess)
	}
	h.typ("/export")
	h.key("enter")
	if h.be.exported != "s3" {
		t.Fatalf("exported %q", h.be.exported)
	}
	h.typ("//literal")
	h.key("enter")
	if got := h.be.submitted; len(got) != 1 || got[0] != "/literal" {
		t.Fatalf("submitted %q", got)
	}
	h.typ("/quit")
	h.key("enter")
	if !h.quit {
		t.Fatal("/quit did not quit")
	}
}

func TestCtrlCCancelsThenQuits(t *testing.T) {
	h := newHarness(t, Options{})
	h.typ("work")
	h.key("enter")
	h.key("ctrl+c")
	if h.be.canceled != 1 || h.quit {
		t.Fatalf("canceled=%d quit=%v", h.be.canceled, h.quit)
	}
	h.emit(events.Event{Kind: events.TurnError, Text: "context canceled"})
	for _, e := range h.m.entries {
		if e.kind == kindError {
			t.Fatalf("cancel showed an error: %q", e.text)
		}
	}
	h.key("ctrl+c")
	if !h.quit {
		t.Fatal("second Ctrl+C did not quit")
	}
}

func TestCtrlCIdleNeedsTwoPresses(t *testing.T) {
	h := newHarness(t, Options{})
	h.typ("text")
	h.key("ctrl+c")
	if h.quit || h.m.input.Value() != "" {
		t.Fatalf("first Ctrl+C: quit=%v input=%q", h.quit, h.m.input.Value())
	}
	h.clock = h.clock.Add(3 * time.Second) // too late to count as the second
	h.key("ctrl+c")
	if h.quit {
		t.Fatal("quit on a lone Ctrl+C")
	}
	h.key("ctrl+c")
	if !h.quit {
		t.Fatal("did not quit")
	}
}

func TestEscEscRewindsAndRetries(t *testing.T) {
	h := newHarness(t, Options{})
	h.typ("what is 2+2")
	h.key("enter")
	h.emit(events.Event{Kind: events.TurnDone, Text: "5"})
	h.be.busy = false
	h.send(pollMsg{session: "s1", gen: h.m.pollGen})

	h.key("esc", "esc")
	if !h.m.editing || h.m.input.Value() != "what is 2+2" {
		t.Fatalf("editing=%v input=%q", h.m.editing, h.m.input.Value())
	}
	h.key("enter")
	if got := h.be.submitted[len(h.be.submitted)-1]; got != retryPrefix+"what is 2+2" {
		t.Fatalf("resent %q", got)
	}

	// Edited, it goes as a new message.
	h.emit(events.Event{Kind: events.TurnDone, Text: "4"})
	h.be.busy = false
	h.send(pollMsg{session: "s1", gen: h.m.pollGen})
	h.key("esc", "esc")
	h.typ("?")
	h.key("enter")
	if got := h.be.submitted[len(h.be.submitted)-1]; got != "what is 2+2?" {
		t.Fatalf("edited resend %q", got)
	}
}

func TestApprovalFlow(t *testing.T) {
	h := newHarness(t, Options{})
	msgs := make(chan tea.Msg, 4)
	h.br.setSend(func(m tea.Msg) { msgs <- m })
	ap := h.be.approvers["s1"]

	ask := func() chan bool {
		res := make(chan bool, 1)
		go func() {
			ok, err := ap.Approve(context.Background(), tools.ApprovalRequest{Tool: "exec", Summary: "rm -rf build"})
			res <- ok && err == nil
		}()
		return res
	}
	res := ask()
	h.send(<-msgs)
	if len(h.m.approvals) != 1 {
		t.Fatal("no approval prompt")
	}
	v := h.view()
	for _, want := range []string{"? Allow exec rm -rf build", "Allow once (a)", "Allow for this session (s)", "Deny (d)"} {
		if !strings.Contains(v, want) {
			t.Errorf("prompt lacks %q:\n%s", want, v)
		}
	}
	h.typ("x") // ignored while the prompt waits
	if h.m.input.Value() != "" {
		t.Error("typing went to the input during an approval")
	}
	h.key("right", "enter") // allow for the session
	if !<-res {
		t.Fatal("not allowed")
	}
	if len(h.m.approvals) != 0 {
		t.Fatal("prompt still shown")
	}
	// Remembered for the session: no second prompt.
	if !<-ask() {
		t.Fatal("second call was not allowed")
	}
	select {
	case m := <-msgs:
		t.Fatalf("asked again: %#v", m)
	default:
	}

	ok, _ := ap.Approve(context.Background(), tools.ApprovalRequest{Tool: "exec"})
	if !ok {
		t.Fatal("always-allow forgot")
	}
	res2 := make(chan bool, 1)
	go func() {
		ok, _ := ap.Approve(context.Background(), tools.ApprovalRequest{Tool: "write_file", Summary: "x"})
		res2 <- ok
	}()
	h.send(<-msgs)
	h.key("d")
	if <-res2 {
		t.Fatal("deny allowed the call")
	}
}

func TestApprovalWithdrawnWhenTurnStops(t *testing.T) {
	h := newHarness(t, Options{})
	msgs := make(chan tea.Msg, 4)
	h.br.setSend(func(m tea.Msg) { msgs <- m })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.be.approvers["s1"].Approve(ctx, tools.ApprovalRequest{Tool: "exec"})
		done <- err
	}()
	h.send(<-msgs)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	h.send(<-msgs)
	if len(h.m.approvals) != 0 {
		t.Fatal("prompt not withdrawn")
	}
}

func TestClarifyChoices(t *testing.T) {
	h := newHarness(t, Options{})
	msgs := make(chan tea.Msg, 4)
	h.br.setSend(func(m tea.Msg) { msgs <- m })
	asker := h.be.approvers["s1"].(tools.Asker)
	res := make(chan string, 1)
	go func() {
		ans, _ := asker.Ask(context.Background(), []tools.Question{{Question: "Which color?", Choices: []string{"red", "blue", "green"}}})
		res <- ans
	}()
	h.send(<-msgs)
	v := h.view()
	for _, want := range []string{"? Which color?", "› a) red", "b) blue"} {
		if !strings.Contains(v, want) {
			t.Errorf("question lacks %q:\n%s", want, v)
		}
	}
	h.key("down", "enter")
	if got := <-res; got != "blue" {
		t.Fatalf("answer %q", got)
	}

	// A typed answer wins over the choices.
	go func() {
		ans, _ := asker.Ask(context.Background(), []tools.Question{{Question: "Name?"}, {Question: "Age?"}})
		res <- ans
	}()
	h.send(<-msgs)
	h.typ("Ada, 36")
	h.key("enter")
	if got := <-res; got != "Ada, 36" {
		t.Fatalf("typed answer %q", got)
	}

	// Esc skips.
	go func() {
		ans, _ := asker.Ask(context.Background(), []tools.Question{{Question: "Sure?"}})
		res <- ans
	}()
	h.send(<-msgs)
	h.key("esc")
	if got := <-res; got != "" {
		t.Fatalf("skipped answer %q", got)
	}
}

func TestResumePicker(t *testing.T) {
	fake := newFake()
	fake.sessions = []SessionInfo{
		{ID: "old1", Agent: "habakkuk", Title: "Plan the trip", UpdatedAt: time.Now().Add(-2 * time.Hour)},
		{ID: "old2", Agent: "coder", Title: "Fix the build", UpdatedAt: time.Now().Add(-time.Hour)},
	}
	fake.recs["old2"] = []session.Record{
		{Message: llm.Message{Role: llm.User, Blocks: []llm.Block{{Type: llm.Text, Text: "[Fri 2026-11-06 10:00 UTC]\nwhy does it fail?"}}}},
		{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{
			{Type: llm.Text, Text: "Let me check."},
			{Type: llm.ToolUse, ToolUseID: "c1", ToolName: "exec", Input: json.RawMessage(`{"command":"go build"}`)}}}},
		{Message: llm.Message{Role: llm.User, Blocks: []llm.Block{{Type: llm.ToolResult, ToolUseID: "c1", Text: "undefined: x", IsError: true}}}},
	}
	hh := &harness{t: t, be: fake, br: newBridge(), clock: time.Now()}
	o := Options{Resume: true, Agent: "habakkuk", Theme: NewTheme(false, true), Now: func() time.Time { return hh.clock }}
	hh.m = New(fake, o, hh.br)
	hh.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	hh.run(hh.m.Init())
	t.Cleanup(func() { close(hh.br.done) })
	if hh.m.picker == nil {
		t.Fatal("no picker")
	}
	v := hh.view()
	if !strings.Contains(v, "Plan the trip") || !strings.Contains(v, "Fix the build") {
		t.Fatalf("picker:\n%s", v)
	}
	hh.typ("build")
	if items := hh.m.picker.visible(); len(items) != 1 {
		t.Fatalf("filter left %d items", len(items))
	}
	hh.key("enter")
	if hh.m.sess.ID != "old2" || hh.m.picker != nil {
		t.Fatalf("opened %+v", hh.m.sess)
	}
	want := []entryKind{kindUser, kindAssistant, kindTool, kindNotice}
	if got := kinds(hh.m.entries); !slices.Equal(got, want) {
		t.Fatalf("history %v, want %v", got, want)
	}
	if e := hh.m.entries[0]; e.text != "why does it fail?" {
		t.Errorf("user text %q", e.text)
	}
	if e := hh.m.entries[2]; e.status != toolFailed || e.output != "undefined: x" {
		t.Errorf("tool %+v", e)
	}
	hh.key("up")
	if v := hh.m.input.Value(); v != "why does it fail?" {
		t.Errorf("history recall %q", v)
	}
}

func TestStartupPickerEscStartsFresh(t *testing.T) {
	fake := newFake()
	fake.sessions = []SessionInfo{{ID: "old", Agent: "habakkuk", Title: "x"}}
	hh := &harness{t: t, be: fake, br: newBridge(), clock: time.Now()}
	hh.m = New(fake, Options{Resume: true, Agent: "habakkuk", Theme: NewTheme(true, true)}, hh.br)
	hh.send(tea.WindowSizeMsg{Width: 80, Height: 20})
	hh.run(hh.m.Init())
	t.Cleanup(func() { close(hh.br.done) })
	hh.key("esc")
	if hh.m.picker != nil || hh.m.sess.ID != "s2" {
		t.Fatalf("esc gave session %q picker=%v", hh.m.sess.ID, hh.m.picker != nil)
	}
}

func TestEventsForOtherSessionsAreDropped(t *testing.T) {
	h := newHarness(t, Options{})
	h.send(eventMsg{session: "elsewhere", ev: events.Event{Kind: events.Text, Text: "nope"}})
	if len(h.m.entries) != 0 {
		t.Fatalf("entries %v", kinds(h.m.entries))
	}
	reply := make(chan decision, 1)
	h.send(approvalMsg{session: "elsewhere", id: "x", reply: reply})
	if <-reply != deny || len(h.m.approvals) != 0 {
		t.Fatal("approval for another session was not denied")
	}
}

func TestSteerFollowUp(t *testing.T) {
	h := newHarness(t, Options{})
	h.typ("long task")
	h.key("enter")
	h.typ("/steer use python")
	h.key("enter")
	if !slices.Equal(h.be.commands, []string{"/steer use python"}) || !h.m.steered {
		t.Fatalf("commands %q steered=%v", h.be.commands, h.m.steered)
	}
	h.be.followUp = true
	h.emit(events.Event{Kind: events.TurnDone, Text: "done"})
	if h.m.steered || !h.m.busy {
		t.Fatalf("steered=%v busy=%v after follow-up", h.m.steered, h.m.busy)
	}
}

func TestMouseClickExpandsThinking(t *testing.T) {
	h := newHarness(t, Options{})
	h.typ("q")
	h.key("enter")
	h.emit(events.Event{Kind: events.Thinking, Text: "a\nb\nc"}, events.Event{Kind: events.Text, Text: "answer"})
	h.emit(events.Event{Kind: events.TurnDone, Text: "answer"})
	h.view()
	think := h.m.entries[1]
	y := h.m.offsets[1] - h.m.vp.YOffset
	h.send(tea.MouseMsg{X: 2, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if !think.expanded {
		t.Fatal("click did not expand the thinking")
	}
	if v := h.view(); !strings.Contains(v, "▾ thinking") {
		t.Fatalf("expanded view:\n%s", v)
	}
}

// The clarify tool asks through the UI's approver when no chat is involved.
func TestClarifyToolAsksTheUI(t *testing.T) {
	h := newHarness(t, Options{})
	msgs := make(chan tea.Msg, 4)
	h.br.setSend(func(m tea.Msg) { msgs <- m })
	env := &tools.Env{Session: "s1", Approver: h.be.approvers["s1"]}
	out := make(chan string, 1)
	go func() {
		res, err := channel.ClarifyTool(nil).Run(context.Background(), env,
			json.RawMessage(`{"questions":[{"question":"Tabs or spaces?","choices":["tabs","spaces"]}]}`))
		if err != nil {
			res = "error: " + err.Error()
		}
		out <- res
	}()
	h.send(<-msgs)
	h.key("down", "enter")
	if got := <-out; got != "The user answered:\nspaces" {
		t.Fatalf("tool result %q", got)
	}
	// Without the UI there is nobody to ask.
	_, err := channel.ClarifyTool(nil).Run(context.Background(), &tools.Env{}, json.RawMessage(`{"questions":[{"question":"x"}]}`))
	if err == nil {
		t.Fatal("clarify without a chat or UI did not fail")
	}
}
