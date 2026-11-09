package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/usage"
)

// Options configure a Model.
type Options struct {
	Session string // open this session
	Agent   string // the agent for a new session
	Resume  bool   // start with a picker of recent sessions
	Version string
	Theme   Theme
	Context context.Context // for backend calls; Background when nil
	Now     func() time.Time
}

const (
	doubleEsc     = 600 * time.Millisecond
	quitWindow    = 2 * time.Second
	streamRender  = 80 * time.Millisecond // how often streaming Markdown is re-rendered
	maxInputLines = 8
	popupRows     = 8
)

// Model is the terminal UI's state. Its methods run on the program's
// goroutine; commands it returns call the Backend elsewhere and report back
// with messages tagged by session, so a late answer for a session no longer
// shown is dropped.
type Model struct {
	be   Backend
	opts Options
	t    Theme
	br   *bridge
	ctx  context.Context
	now  func() time.Time

	width, height int
	vp            viewport.Model
	input         textarea.Model
	spin          spinner.Model
	md            *markdown

	sess    SessionInfo
	status  Status
	entries []*entry
	offsets []int             // first content line of each entry, for clicks
	toolIdx map[string]*entry // live and loaded tool cards by call id
	stream  *entry            // the assistant or thinking entry text is arriving into

	evCh          <-chan events.Event
	stop          chan struct{}
	unsub, detach func()

	busy, spinning, canceling bool
	turnStart                 time.Time
	textSeen                  bool // assistant text arrived since the last tool call
	steered, compacting       bool
	pollGen                   int

	approvals []*approvalPrompt
	questions []*clarifyPrompt
	picker    *picker
	popup     []completion
	popupSel  int
	popupOff  string // the input the popup was dismissed for

	history  []string
	histPos  int
	draft    string
	editing  bool
	editOrig string

	lastEsc, lastCtrlC time.Time
	hint               string
	hintUntil          time.Time

	expandAll, showThinking bool
	dirty, quitting         bool
	initCmd                 tea.Cmd // set when Run opened the session before starting
}

// New builds a Model. Messages from worker goroutines (approvals and
// questions) reach it through br.
func New(be Backend, o Options, br *bridge) *Model {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Context == nil {
		o.Context = context.Background()
	}
	if br == nil {
		br = newBridge()
	}
	if !o.Theme.ok {
		o.Theme = NewTheme(true, true)
	}
	ta := textarea.New()
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.Placeholder = "Message Habakkuk. / for commands, Alt+Enter for a new line"
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j", "shift+enter"))
	ta.FocusedStyle.CursorLine = ta.FocusedStyle.Text
	ta.FocusedStyle.Placeholder = o.Theme.Dim
	ta.Cursor.SetMode(cursor.CursorStatic)
	ta.SetHeight(1)
	ta.Focus()
	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(o.Theme.Accent))
	vp := viewport.New(0, 0)
	vp.KeyMap = viewport.KeyMap{}
	vp.MouseWheelEnabled = false
	return &Model{be: be, opts: o, t: o.Theme, br: br, ctx: o.Context, now: o.Now,
		vp: vp, input: ta, spin: sp, toolIdx: map[string]*entry{}, dirty: true}
}

// Messages.
type (
	sessionMsg struct {
		info SessionInfo
		note string
		err  error
	}
	sessionsMsg struct {
		list    []SessionInfo
		startup bool
		err     error
	}
	transcriptMsg struct {
		session string
		recs    []session.Record
		err     error
	}
	outputsMsg struct {
		session string
		outs    map[string]llm.Block
	}
	statusMsg struct {
		session string
		st      Status
	}
	eventMsg struct {
		session string
		ev      events.Event
	}
	submittedMsg struct {
		session string
		err     error
	}
	commandMsg struct {
		session, name string
		res           CommandResult
		err           error
	}
	noticeMsg struct {
		text string
		err  error
	}
	cancelMsg struct {
		session string
		n       int
	}
	pollMsg struct {
		session string
		gen     int
	}
	busyMsg struct {
		session string
		gen     int
		busy    bool
	}
)

func (m *Model) Init() tea.Cmd {
	switch {
	case m.initCmd != nil:
		return m.initCmd
	case m.opts.Resume:
		return m.listSessions(true)
	case m.opts.Session != "":
		id := m.opts.Session
		return func() tea.Msg {
			info, err := m.be.Session(m.ctx, id)
			return sessionMsg{info: info, note: "Resumed this session.", err: err}
		}
	}
	return m.newSession(m.opts.Agent, "")
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
	case tea.KeyMsg:
		cmd = m.key(msg)
	case tea.MouseMsg:
		cmd = m.mouse(msg)
	case spinner.TickMsg:
		if !m.busy && !m.toolsRunning() {
			m.spinning = false
			break
		}
		m.spin, cmd = m.spin.Update(msg)
		if m.toolsRunning() {
			m.dirty = true
		}
	case eventMsg:
		cmd = m.event(msg)
	case approvalMsg:
		if msg.session != m.sess.ID {
			msg.reply <- deny
			break
		}
		m.approvals = append(m.approvals, &approvalPrompt{approvalMsg: msg, sel: allowOnce})
		if te := m.toolIdx[msg.toolID]; te != nil {
			te.status = toolWaiting
			m.dirty = true
		}
	case clarifyMsg:
		if msg.session != m.sess.ID {
			msg.reply <- ""
			break
		}
		m.questions = append(m.questions, &clarifyPrompt{clarifyMsg: msg})
	case promptClosedMsg:
		m.approvals = slices.DeleteFunc(m.approvals, func(p *approvalPrompt) bool { return p.id == msg.id })
		m.questions = slices.DeleteFunc(m.questions, func(p *clarifyPrompt) bool { return p.id == msg.id })
	case sessionMsg:
		if msg.err != nil {
			m.addError("Couldn't open the session: " + msg.err.Error())
			break
		}
		cmd = m.open(msg.info, msg.note)
	case sessionsMsg:
		cmd = m.sessionsLoaded(msg)
	case transcriptMsg:
		m.transcriptLoaded(msg)
	case outputsMsg:
		m.outputsLoaded(msg)
	case statusMsg:
		if msg.session == m.sess.ID {
			m.status = msg.st
		}
	case submittedMsg:
		if msg.session != m.sess.ID {
			break
		}
		if msg.err != nil {
			m.addError("Couldn't send that: " + msg.err.Error())
		}
		cmd = m.poll(time.Second)
	case commandMsg:
		cmd = m.commandDone(msg)
	case noticeMsg:
		if msg.err != nil {
			m.addError(msg.err.Error())
		} else {
			m.add(&entry{kind: kindNotice, text: msg.text})
		}
	case cancelMsg:
		if msg.session != m.sess.ID {
			break
		}
		if msg.n == 0 {
			m.canceling = false
			m.add(&entry{kind: kindNotice, text: "Nothing was running."})
		} else {
			m.add(&entry{kind: kindNotice, text: "Stopped."})
		}
		cmd = m.poll(150 * time.Millisecond)
	case pollMsg:
		if msg.session != m.sess.ID || msg.gen != m.pollGen {
			break
		}
		sid, gen := msg.session, msg.gen
		cmd = func() tea.Msg { return busyMsg{session: sid, gen: gen, busy: m.be.Busy(m.ctx, sid)} }
	case busyMsg:
		if msg.session != m.sess.ID || msg.gen != m.pollGen {
			break
		}
		if msg.busy {
			cmd = tea.Batch(m.markBusy(), m.poll(time.Second))
		} else {
			m.setIdle()
		}
	}
	return m, cmd
}

// Sessions.

func (m *Model) newSession(agent, note string) tea.Cmd {
	if agent == "" {
		agent = m.sess.Agent
	}
	if agent == "" {
		if list := m.be.Agents(); len(list) > 0 {
			agent = list[0]
		}
	}
	return func() tea.Msg {
		info, err := m.be.NewSession(m.ctx, agent)
		return sessionMsg{info: info, note: note, err: err}
	}
}

func (m *Model) listSessions(startup bool) tea.Cmd {
	return func() tea.Msg {
		list, err := m.be.RecentSessions(m.ctx, 50)
		return sessionsMsg{list: list, startup: startup, err: err}
	}
}

func (m *Model) sessionsLoaded(msg sessionsMsg) tea.Cmd {
	if msg.err != nil {
		m.addError("Couldn't list sessions: " + msg.err.Error())
		if msg.startup {
			return m.newSession(m.opts.Agent, "")
		}
		return nil
	}
	if len(msg.list) == 0 {
		if msg.startup {
			return m.newSession(m.opts.Agent, "No earlier sessions, so this is a fresh one.")
		}
		m.add(&entry{kind: kindNotice, text: "There are no other sessions yet."})
		return nil
	}
	m.picker = sessionPicker(msg.list, m.now(), msg.startup)
	return nil
}

// open shows a session: it subscribes to its events, attaches the approver
// and loads the transcript.
func (m *Model) open(info SessionInfo, note string) tea.Cmd {
	m.closeSession()
	m.sess, m.status = info, Status{}
	m.entries, m.toolIdx, m.stream = nil, map[string]*entry{}, nil
	m.busy, m.canceling, m.steered, m.compacting, m.editing = false, false, false, false, false
	m.picker = nil
	m.evCh, m.unsub = m.be.Subscribe(info.ID)
	m.stop = make(chan struct{})
	m.detach = m.be.Attach(info.ID, newApprover(info.ID, m.br))
	if note != "" {
		m.add(&entry{kind: kindNotice, text: note})
	}
	m.vp.GotoTop()
	m.dirty = true
	return tea.Batch(m.waitEvent(), m.loadTranscript(), m.fetchStatus(), m.poll(0))
}

// closeSession stops listening to the shown session and answers any prompt
// still open for it: approvals are denied, questions go unanswered.
func (m *Model) closeSession() {
	if m.stop != nil {
		close(m.stop)
		m.stop = nil
	}
	if m.unsub != nil {
		m.unsub()
		m.unsub = nil
	}
	if m.detach != nil {
		m.detach()
		m.detach = nil
	}
	for _, p := range m.approvals {
		p.reply <- deny
	}
	for _, q := range m.questions {
		q.reply <- ""
	}
	m.approvals, m.questions = nil, nil
}

func (m *Model) waitEvent() tea.Cmd {
	ch, stop, sid := m.evCh, m.stop, m.sess.ID
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case e := <-ch:
			return eventMsg{session: sid, ev: e}
		case <-stop:
			return nil
		}
	}
}

func (m *Model) loadTranscript() tea.Cmd {
	sid := m.sess.ID
	return func() tea.Msg {
		recs, err := m.be.Transcript(m.ctx, sid)
		return transcriptMsg{session: sid, recs: recs, err: err}
	}
}

func (m *Model) transcriptLoaded(msg transcriptMsg) {
	if msg.session != m.sess.ID {
		return
	}
	if msg.err != nil {
		m.addError("Couldn't read the transcript: " + msg.err.Error())
		return
	}
	hist, typed := historyEntries(msg.recs)
	for _, e := range hist {
		if e.kind == kindTool && m.toolIdx[e.toolID] == nil {
			m.toolIdx[e.toolID] = e
		}
	}
	m.entries = append(hist, m.entries...)
	m.history = append(typed, m.history...)
	m.histPos = len(m.history)
	m.dirty = true
	m.vp.GotoBottom()
}

func (m *Model) fetchStatus() tea.Cmd {
	sid := m.sess.ID
	return func() tea.Msg { return statusMsg{session: sid, st: m.be.Status(m.ctx, sid)} }
}

func (m *Model) fetchOutputs() tea.Cmd {
	sid := m.sess.ID
	return func() tea.Msg {
		recs, err := m.be.Transcript(m.ctx, sid)
		if err != nil {
			return nil
		}
		return outputsMsg{session: sid, outs: toolOutputs(recs)}
	}
}

func (m *Model) outputsLoaded(msg outputsMsg) {
	if msg.session != m.sess.ID {
		return
	}
	for id, b := range msg.outs {
		te := m.toolIdx[id]
		if te == nil {
			continue
		}
		te.output = b.Text
		if te.status != toolRunning && te.status != toolWaiting || te.end.IsZero() {
			te.status = toolOK
			if b.IsError {
				te.status = toolFailed
			}
		}
		m.dirty = true
	}
}

// poll asks the backend whether the session is busy after d. Only the
// latest poll counts, so polls never pile up.
func (m *Model) poll(d time.Duration) tea.Cmd {
	m.pollGen++
	sid, gen := m.sess.ID, m.pollGen
	if sid == "" {
		return nil
	}
	return tea.Tick(d, func(time.Time) tea.Msg { return pollMsg{session: sid, gen: gen} })
}

func (m *Model) markBusy() tea.Cmd {
	if !m.busy {
		m.busy, m.turnStart, m.textSeen = true, m.now(), false
	}
	if m.spinning {
		return nil
	}
	m.spinning = true
	return m.spin.Tick
}

func (m *Model) setIdle() {
	m.busy, m.canceling = false, false
	m.endStream()
}

func (m *Model) toolsRunning() bool {
	for _, e := range m.toolIdx {
		if e.status == toolRunning || e.status == toolWaiting {
			return true
		}
	}
	return false
}

// Events.

func (m *Model) event(msg eventMsg) tea.Cmd {
	if msg.session != m.sess.ID {
		return nil
	}
	cmds := []tea.Cmd{m.waitEvent()}
	e := msg.ev
	switch e.Kind {
	case events.Thinking:
		if e.Text != "" {
			m.streamInto(kindThinking, e.Text)
		}
		cmds = append(cmds, m.markBusy())
	case events.Text:
		m.streamInto(kindAssistant, e.Text)
		m.textSeen = true
		cmds = append(cmds, m.markBusy())
	case events.ToolStart:
		m.endStream()
		te := &entry{kind: kindTool, toolID: e.ToolID, tool: e.Tool, input: e.Input, status: toolRunning, start: m.now()}
		if te.tool == "" {
			te.tool, _, _ = strings.Cut(e.Text, " ")
		}
		if e.ToolID != "" {
			m.toolIdx[e.ToolID] = te
		}
		// The approval travels apart from the events and may have come first.
		if slices.ContainsFunc(m.approvals, func(p *approvalPrompt) bool { return p.toolID != "" && p.toolID == e.ToolID }) {
			te.status = toolWaiting
		}
		m.add(te)
		m.textSeen = false
		cmds = append(cmds, m.markBusy())
	case events.ToolDone:
		te := m.toolIdx[e.ToolID]
		if te == nil {
			break
		}
		te.end, te.status = m.now(), toolOK
		if e.IsError {
			te.status = toolFailed
		}
		te.preview = strings.TrimPrefix(e.Text, te.tool+": ")
		m.dirty = true
	case events.Retry:
		m.add(&entry{kind: kindNotice, text: "retrying: " + e.Text})
	case events.Review:
		m.add(&entry{kind: kindNotice, text: "reviewer: " + e.Text})
	case events.TurnDone:
		m.endStream()
		switch {
		case m.compacting:
			m.compacting = false
			if e.Text != "" {
				m.add(&entry{kind: kindNotice, text: e.Text})
			}
		case !m.textSeen && strings.TrimSpace(e.Text) != "":
			m.add(&entry{kind: kindAssistant, text: e.Text})
		}
		m.turnStart, m.textSeen = m.now(), false
		cmds = append(cmds, m.fetchStatus(), m.fetchOutputs(), m.poll(150*time.Millisecond))
		if m.steered {
			m.steered = false
			sid := m.sess.ID
			cmds = append(cmds, func() tea.Msg {
				return commandMsg{session: sid, name: "/steer", res: CommandResult{Started: m.be.FollowUp(m.ctx, sid)}}
			})
		}
	case events.TurnError:
		m.endStream()
		m.compacting = false
		if !m.canceling {
			m.addError(e.Text)
		}
		cmds = append(cmds, m.fetchStatus(), m.poll(150*time.Millisecond))
	}
	return tea.Batch(cmds...)
}

func (m *Model) streamInto(kind entryKind, text string) {
	if m.stream == nil || m.stream.kind != kind {
		m.endStream()
		m.stream = &entry{kind: kind, streaming: true}
		m.add(m.stream)
	}
	m.stream.text += text
	m.stream.stale = true
	m.dirty = true
}

func (m *Model) endStream() {
	if m.stream == nil {
		return
	}
	m.stream.streaming = false
	if m.stream.kind == kindThinking && strings.TrimSpace(m.stream.text) == "" {
		m.entries = slices.DeleteFunc(m.entries, func(e *entry) bool { return e == m.stream })
	}
	m.stream = nil
	m.dirty = true
}

func (m *Model) add(e *entry) {
	m.entries = append(m.entries, e)
	m.dirty = true
}

func (m *Model) addError(text string) { m.add(&entry{kind: kindError, text: text}) }

func (m *Model) setHint(text string) {
	m.hint, m.hintUntil = text, m.now().Add(quitWindow+time.Second)
}

// Sending.

// submit queues text as a turn of the shown session.
func (m *Model) submit(text string) tea.Cmd {
	sid := m.sess.ID
	if sid == "" {
		m.addError("No session is open.")
		return nil
	}
	if m.busy {
		m.add(&entry{kind: kindNotice, text: "Queued: it starts when the current turn is done."})
	}
	return tea.Batch(m.markBusy(), func() tea.Msg {
		return submittedMsg{session: sid, err: m.be.Submit(m.ctx, sid, text)}
	})
}

func (m *Model) enter() tea.Cmd {
	v := m.input.Value()
	if len(m.questions) > 0 {
		return m.answerQuestion(v)
	}
	text := strings.TrimSpace(v)
	if text == "" {
		return nil
	}
	m.input.Reset()
	m.popup, m.popupOff = nil, ""
	m.pushHistory(text)
	if strings.HasPrefix(text, "/") && !strings.HasPrefix(text, "//") {
		m.editing = false
		return m.runCommand(text)
	}
	if strings.HasPrefix(text, "//") {
		text = text[1:]
	}
	if m.editing {
		m.editing = false
		if text == m.editOrig {
			m.add(&entry{kind: kindUser, text: text})
			m.add(&entry{kind: kindNotice, text: "(asked again)"})
			return m.submit(retryPrefix + text)
		}
	}
	m.add(&entry{kind: kindUser, text: text})
	return m.submit(text)
}

func (m *Model) pushHistory(text string) {
	if n := len(m.history); n == 0 || m.history[n-1] != text {
		m.history = append(m.history, text)
	}
	m.histPos, m.draft = len(m.history), ""
}

// recall walks the input history: dir -1 is older, +1 newer.
func (m *Model) recall(dir int) {
	if len(m.history) == 0 {
		return
	}
	if m.histPos == len(m.history) && dir < 0 {
		m.draft = m.input.Value()
	}
	pos := min(max(m.histPos+dir, 0), len(m.history))
	if pos == m.histPos {
		return
	}
	m.histPos = pos
	if pos == len(m.history) {
		m.setInput(m.draft)
	} else {
		m.setInput(m.history[pos])
	}
	m.afterInput()
}

// rewind puts the last message the user typed back in the input; sending
// it unchanged runs it again, as /retry does.
func (m *Model) rewind() {
	if m.busy {
		m.setHint("Stop the running turn first (Ctrl+C), then Esc Esc")
		return
	}
	for i := len(m.entries) - 1; i >= 0; i-- {
		if e := m.entries[i]; e.kind == kindUser {
			m.editing, m.editOrig = true, e.text
			m.setInput(e.text)
			m.afterInput()
			m.setHint("Editing your last message: Enter sends it again, Esc cancels")
			return
		}
	}
	m.setHint("There's no message to edit yet")
}

func (m *Model) answerQuestion(v string) tea.Cmd {
	q := m.questions[0]
	ans := strings.TrimSpace(v)
	if ans == "" {
		ch := q.choices()
		if len(ch) == 0 {
			return nil
		}
		ans = ch[q.sel]
	}
	q.reply <- ans
	m.questions = m.questions[1:]
	m.input.Reset()
	m.add(&entry{kind: kindUser, text: ans})
	return nil
}

func (m *Model) decide(d decision) {
	p := m.approvals[0]
	p.reply <- d
	m.approvals = m.approvals[1:]
	if te := m.toolIdx[p.toolID]; te != nil && te.status == toolWaiting {
		te.status = toolRunning
		m.dirty = true
	}
	if d == deny {
		m.add(&entry{kind: kindNotice, text: "Denied " + p.req.Tool + "."})
	}
}

// interrupt is Ctrl+C: the first stops a running turn (or clears the
// input), a second within a moment quits.
func (m *Model) interrupt() tea.Cmd {
	now := m.now()
	if !m.lastCtrlC.IsZero() && now.Sub(m.lastCtrlC) < quitWindow {
		return m.quit()
	}
	m.lastCtrlC = now
	switch {
	case m.picker != nil && m.sess.ID != "":
		m.picker = nil
		m.setHint("Press Ctrl+C again to quit")
	case m.busy && !m.canceling:
		m.setHint("Stopping. Press Ctrl+C again to quit")
		return m.cancel()
	case m.input.Value() != "":
		m.input.Reset()
		m.editing = false
		m.afterInput()
		m.setHint("Cleared. Press Ctrl+C again to quit")
	default:
		m.setHint("Press Ctrl+C again to quit")
	}
	return nil
}

func (m *Model) cancel() tea.Cmd {
	for len(m.approvals) > 0 {
		m.decide(deny)
	}
	for _, q := range m.questions {
		q.reply <- ""
	}
	m.questions = nil
	m.canceling = true
	sid := m.sess.ID
	return func() tea.Msg { return cancelMsg{session: sid, n: m.be.Cancel(m.ctx, sid)} }
}

func (m *Model) quit() tea.Cmd {
	m.quitting = true
	m.closeSession()
	return tea.Quit
}

// Commands.

func (m *Model) runCommand(text string) tea.Cmd {
	name, arg, _ := strings.Cut(text, " ")
	arg = strings.TrimSpace(arg)
	c, ok := lookupCommand(name)
	if !ok {
		m.add(&entry{kind: kindNotice, text: "There's no command " + name + ". /help lists them; start a message with // to send it as text."})
		return nil
	}
	sid := m.sess.ID
	switch c.name {
	case "/help":
		m.add(&entry{kind: kindNotice, text: helpText()})
		return nil
	case "/new":
		return m.newSession(m.sess.Agent, "Started a fresh session.")
	case "/clear":
		m.entries, m.stream = nil, nil
		m.vp.GotoTop()
		m.dirty = true
		return nil
	case "/sessions":
		return m.listSessions(false)
	case "/agent":
		if arg == "" {
			m.picker = agentPicker(m.be.Agents(), m.sess.Agent)
			return nil
		}
		if !slices.Contains(m.be.Agents(), arg) {
			m.add(&entry{kind: kindNotice, text: fmt.Sprintf("There's no agent %q. /agents lists them.", arg)})
			return nil
		}
		return m.newSession(arg, "You're now talking to "+arg+", in a fresh session.")
	case "/agents":
		var b strings.Builder
		b.WriteString("Agents:")
		for _, a := range m.be.Agents() {
			b.WriteString("\n  " + a)
			if a == m.sess.Agent {
				b.WriteString(" (this session)")
			}
		}
		b.WriteString("\nSwitch with /agent <name>.")
		m.add(&entry{kind: kindNotice, text: b.String()})
		return nil
	case "/stop":
		if !m.busy {
			m.add(&entry{kind: kindNotice, text: "Nothing is running."})
			return nil
		}
		return m.cancel()
	case "/queue":
		if arg == "" {
			m.add(&entry{kind: kindNotice, text: "Usage: /queue <message to run after the current turn>"})
			return nil
		}
		m.add(&entry{kind: kindUser, text: arg})
		return m.submit(arg)
	case "/export":
		return func() tea.Msg {
			path, err := m.be.Export(m.ctx, sid, arg)
			if err != nil {
				return noticeMsg{err: fmt.Errorf("couldn't export: %w", err)}
			}
			return noticeMsg{text: "Wrote " + path}
		}
	case "/status":
		m.add(&entry{kind: kindNotice, text: m.statusText()})
		return nil
	case "/quit":
		return m.quit()
	}
	if sid == "" {
		m.addError("No session is open.")
		return nil
	}
	switch c.name {
	case "/btw", "/steer":
		if arg != "" {
			m.add(&entry{kind: kindUser, text: c.name + " " + arg})
		}
		if c.name == "/steer" && m.busy && arg != "" {
			m.steered = true
		}
	}
	return func() tea.Msg {
		res, err := m.be.Command(m.ctx, sid, c.name, arg)
		return commandMsg{session: sid, name: c.name, res: res, err: err}
	}
}

func (m *Model) commandDone(msg commandMsg) tea.Cmd {
	if msg.err != nil {
		m.addError(msg.name + ": " + msg.err.Error())
		return nil
	}
	if msg.res.Reply != "" {
		m.add(&entry{kind: kindAssistant, text: msg.res.Reply})
	}
	if msg.session != m.sess.ID || !msg.res.Started {
		return nil
	}
	if msg.name == "/compact" {
		m.compacting = true
	}
	return tea.Batch(m.markBusy(), m.poll(time.Second))
}

func (m *Model) statusText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Agent: %s\nSession: %s", m.sess.Agent, m.sess.ID)
	if m.sess.Title != "" {
		fmt.Fprintf(&b, " (%s)", m.sess.Title)
	}
	if m.status.Model != "" {
		fmt.Fprintf(&b, "\nModel: %s", m.status.Model)
	}
	if m.status.Calls > 0 {
		fmt.Fprintf(&b, "\nUsage: %d call(s), %s in, %s out, %s", m.status.Calls, usage.Tokens(m.status.In), usage.Tokens(m.status.Out), m.status.Cost)
	}
	if m.busy {
		fmt.Fprintf(&b, "\nWorking for %s", m.now().Sub(m.turnStart).Round(time.Second))
	}
	return b.String()
}

// Keys.

func (m *Model) key(k tea.KeyMsg) tea.Cmd {
	s := k.String()
	if s == "ctrl+c" {
		return m.interrupt()
	}
	if m.picker != nil {
		return m.pickerKey(k)
	}
	switch s {
	case "pgup":
		m.vp.PageUp()
		return nil
	case "pgdown":
		m.vp.PageDown()
		return nil
	case "ctrl+o":
		m.expandAll = !m.expandAll
		m.dirty = true
		if m.expandAll {
			return m.fetchOutputs()
		}
		return nil
	case "ctrl+t":
		m.showThinking = !m.showThinking
		m.dirty = true
		return nil
	}
	if len(m.approvals) > 0 {
		m.approvalKey(s)
		return nil
	}
	if len(m.popup) > 0 {
		switch s {
		case "up", "ctrl+p":
			m.popupSel = (m.popupSel + len(m.popup) - 1) % len(m.popup)
			return nil
		case "down", "ctrl+n":
			m.popupSel = (m.popupSel + 1) % len(m.popup)
			return nil
		case "tab":
			m.accept(false)
			return nil
		case "enter":
			return m.accept(true)
		case "esc":
			m.popupOff, m.popup = m.input.Value(), nil
			return nil
		}
	}
	if q := m.question(); q != nil && m.input.Value() == "" {
		if n := len(q.choices()); n > 0 {
			switch s {
			case "up":
				q.sel = (q.sel + n - 1) % n
				return nil
			case "down":
				q.sel = (q.sel + 1) % n
				return nil
			}
		}
	}
	switch s {
	case "esc":
		return m.escape()
	case "enter":
		return m.enter()
	case "tab":
		return nil
	case "up":
		if m.input.Line() == 0 {
			m.recall(-1)
			return nil
		}
	case "down":
		if m.input.Line() >= m.input.LineCount()-1 {
			m.recall(1)
			return nil
		}
	}
	var cmd tea.Cmd
	m.input.SetHeight(maxInputLines) // so the edit never scrolls a box about to grow
	m.input, cmd = m.input.Update(k)
	m.afterInput()
	return cmd
}

// setInput replaces the input. The box is made full height first: the text
// area scrolls to keep the cursor in view, and would stay scrolled once the
// box grows to fit.
func (m *Model) setInput(v string) {
	m.input.SetHeight(maxInputLines)
	m.input.SetValue(v)
}

func (m *Model) question() *clarifyPrompt {
	if len(m.questions) == 0 {
		return nil
	}
	return m.questions[0]
}

func (m *Model) escape() tea.Cmd {
	if len(m.questions) > 0 {
		q := m.questions[0]
		q.reply <- ""
		m.questions = m.questions[1:]
		m.add(&entry{kind: kindNotice, text: "Skipped the question."})
		return nil
	}
	if m.editing {
		m.editing = false
		m.input.Reset()
		m.afterInput()
		return nil
	}
	now := m.now()
	if !m.lastEsc.IsZero() && now.Sub(m.lastEsc) < doubleEsc {
		m.lastEsc = time.Time{}
		m.rewind()
		return nil
	}
	m.lastEsc = now
	return nil
}

var approvalChoices = []decision{allowOnce, allowSession, deny}

func (m *Model) approvalKey(s string) {
	p := m.approvals[0]
	i := slices.Index(approvalChoices, p.sel)
	switch s {
	case "a", "y", "Y":
		m.decide(allowOnce)
	case "s", "S":
		m.decide(allowSession)
	case "d", "n", "N", "esc":
		m.decide(deny)
	case "left", "shift+tab", "h":
		p.sel = approvalChoices[(i+len(approvalChoices)-1)%len(approvalChoices)]
	case "right", "tab", "l":
		p.sel = approvalChoices[(i+1)%len(approvalChoices)]
	case "enter":
		m.decide(p.sel)
	}
}

// accept takes the selected completion; with run, a command that needs no
// argument runs at once.
func (m *Model) accept(run bool) tea.Cmd {
	c := m.popup[min(m.popupSel, len(m.popup)-1)]
	if run && c.run {
		m.setInput(c.value)
		return m.enter()
	}
	m.setInput(c.value)
	m.input.CursorEnd()
	m.afterInput()
	return nil
}

// afterInput refreshes the completion popup and the input's height.
func (m *Model) afterInput() {
	v := m.input.Value()
	if v != m.popupOff {
		m.popupOff = ""
	}
	if m.popupOff != "" || len(m.approvals) > 0 {
		m.popup = nil
	} else {
		prev := ""
		if len(m.popup) > 0 {
			prev = m.popup[min(m.popupSel, len(m.popup)-1)].value
		}
		m.popup = m.complete(v)
		m.popupSel = max(0, slices.IndexFunc(m.popup, func(c completion) bool { return c.value == prev }))
	}
	m.layout()
}

func (m *Model) mouse(ev tea.MouseMsg) tea.Cmd {
	if ev.Action != tea.MouseActionPress {
		return nil
	}
	if m.picker != nil {
		switch ev.Button {
		case tea.MouseButtonWheelUp:
			m.picker.move(-1)
		case tea.MouseButtonWheelDown:
			m.picker.move(1)
		}
		return nil
	}
	switch ev.Button {
	case tea.MouseButtonWheelUp:
		m.vp.ScrollUp(3)
	case tea.MouseButtonWheelDown:
		m.vp.ScrollDown(3)
	case tea.MouseButtonLeft:
		if ev.Y < 0 || ev.Y >= m.vp.Height {
			return nil
		}
		e := m.entryAt(m.vp.YOffset + ev.Y)
		if e == nil || (e.kind != kindTool && e.kind != kindThinking) {
			return nil
		}
		e.expanded = !e.expanded
		m.dirty = true
		if e.kind == kindTool && e.expanded && e.output == "" {
			return m.fetchOutputs()
		}
	}
	return nil
}

// entryAt finds the entry drawn on a content line.
func (m *Model) entryAt(line int) *entry {
	i, found := slices.BinarySearch(m.offsets, line)
	if !found {
		i--
	}
	if i < 0 || i >= len(m.entries) {
		return nil
	}
	return m.entries[i]
}

// Layout and drawing.

func (m *Model) resize(w, h int) {
	m.width, m.height = w, h
	if m.md == nil || m.md.width != w {
		m.md = newMarkdown(m.t, w)
	}
	m.dirty = true
	m.layout()
}

func (m *Model) layout() {
	if m.width == 0 {
		return
	}
	m.input.SetWidth(max(m.width-4, 10))
	lines := 0
	for l := range strings.SplitSeq(m.input.Value(), "\n") {
		lines += max(1, (ansi.StringWidth(l)+m.width-5)/max(m.width-4, 1))
	}
	m.input.SetHeight(min(max(lines, 1), maxInputLines))
	used := 1 + m.input.Height() + 2 + height(m.popupView()) + height(m.panelView())
	atBottom := m.vp.AtBottom()
	m.vp.Width = m.width
	m.vp.Height = max(m.height-used, 1)
	if atBottom {
		m.vp.GotoBottom()
	}
}

func height(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// refresh rebuilds the transcript when it changed.
func (m *Model) refresh() {
	if !m.dirty || m.width == 0 {
		return
	}
	r := &renderer{t: m.t, md: m.md, width: m.width, now: m.now(), frame: m.spin.View(), all: m.expandAll, think: m.showThinking}
	var b strings.Builder
	m.offsets = m.offsets[:0]
	line := 0
	for i, e := range m.entries {
		s := r.render(e)
		if i > 0 {
			b.WriteString("\n\n")
		}
		m.offsets = append(m.offsets, line)
		b.WriteString(s)
		line += strings.Count(s, "\n") + 2
	}
	atBottom := m.vp.AtBottom()
	m.vp.SetContent(b.String())
	if atBottom {
		m.vp.GotoBottom()
	}
	m.dirty = r.pending
}

func (m *Model) View() string {
	if m.quitting {
		return ""
	}
	if m.width == 0 {
		return "Starting Habakkuk…"
	}
	m.layout()
	m.refresh()
	parts := make([]string, 0, 5)
	if m.picker != nil {
		parts = append(parts, m.picker.view(m.t, m.width, m.vp.Height))
	} else {
		parts = append(parts, m.vp.View())
	}
	if p := m.popupView(); p != "" {
		parts = append(parts, p)
	}
	if p := m.panelView(); p != "" {
		parts = append(parts, p)
	}
	parts = append(parts, m.inputView(), m.statusView())
	return strings.Join(parts, "\n")
}

func (m *Model) popupView() string {
	if len(m.popup) == 0 {
		return ""
	}
	start := max(0, min(m.popupSel-popupRows/2, len(m.popup)-popupRows))
	end := min(start+popupRows, len(m.popup))
	width := 0
	for _, c := range m.popup[start:end] {
		width = max(width, ansi.StringWidth(c.label))
	}
	lines := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		c := m.popup[i]
		row := " " + c.label + strings.Repeat(" ", width-ansi.StringWidth(c.label)) + "  "
		row = ansi.Truncate(row+m.t.Dim.Render(c.help), m.width-1, "…")
		if i == m.popupSel {
			row = m.t.Selected.Render(ansi.Strip(row))
		}
		lines = append(lines, row)
	}
	return strings.Join(lines, "\n")
}

// panelView is the approval or question waiting on the user.
func (m *Model) panelView() string {
	w := m.width
	if len(m.approvals) > 0 {
		p := m.approvals[0]
		var b strings.Builder
		b.WriteString(m.t.Warn.Render("? Allow "+p.req.Tool) + " " + wrap(oneLine(p.req.Summary), w-len(p.req.Tool)-10, "  "))
		if p.req.Review != "" {
			b.WriteString("\n" + m.t.Dim.Render("  reviewer unsure: "+wrap(p.req.Review, w-20, "  ")))
		}
		b.WriteString("\n  ")
		for i, d := range approvalChoices {
			label := map[decision]string{allowOnce: " Allow once (a) ", allowSession: " Allow for this session (s) ", deny: " Deny (d) "}[d]
			if d == p.sel {
				label = m.t.Selected.Render(label)
			}
			if i > 0 {
				b.WriteString("  ")
			}
			b.WriteString(label)
		}
		if n := len(m.approvals) - 1; n > 0 {
			b.WriteString(m.t.Dim.Render(fmt.Sprintf("  (+%d more)", n)))
		}
		return b.String()
	}
	q := m.question()
	if q == nil {
		return ""
	}
	var b strings.Builder
	if len(q.questions) == 1 {
		b.WriteString(m.t.Warn.Render("? ") + wrap(q.questions[0].Question, w-2, "  "))
		for i, c := range q.choices() {
			row := fmt.Sprintf("%c) %s", 'a'+i%26, c)
			if i == q.sel && m.input.Value() == "" {
				b.WriteString("\n  " + m.t.Selected.Render("› "+row))
			} else {
				b.WriteString("\n    " + row)
			}
		}
	} else {
		b.WriteString(m.t.Warn.Render("? A few questions:"))
		for i, qq := range q.questions {
			fmt.Fprintf(&b, "\n  %d. %s", i+1, wrap(qq.Question, w-6, "     "))
			for j, c := range qq.Choices {
				fmt.Fprintf(&b, "\n     %c) %s", 'a'+j%26, c)
			}
		}
	}
	hint := "Type your answer, Enter sends · Esc skips"
	if len(q.choices()) > 0 {
		hint = "↑/↓ pick, Enter sends, or type an answer · Esc skips"
	}
	b.WriteString("\n" + m.t.Dim.Render("  "+hint))
	return b.String()
}

func (m *Model) inputView() string {
	color := m.t.Border
	if len(m.approvals) > 0 {
		color = m.t.Dim
	}
	border := color.GetForeground()
	box := m.inputStyle().BorderForeground(border).Width(max(m.width-2, 10))
	return box.Render(m.input.View())
}

func (m *Model) statusView() string {
	sid := m.sess.ID
	left := " " + m.sess.Agent
	if m.status.Model != "" {
		left += " · " + m.status.Model
	}
	if sid != "" {
		left += " · " + sid
	}
	var right []string
	if m.hint != "" && m.now().Before(m.hintUntil) {
		right = append(right, m.hint)
	} else if m.editing {
		right = append(right, "editing last message")
	}
	if m.busy {
		what := "working"
		if m.canceling {
			what = "stopping"
		}
		right = append(right, m.spin.View()+" "+what+" "+m.now().Sub(m.turnStart).Round(time.Second).String())
	}
	if m.status.In+m.status.Out > 0 {
		u := usage.Tokens(m.status.In) + " in · " + usage.Tokens(m.status.Out) + " out"
		if m.status.Cost != "" && m.status.Cost != "?" {
			u += " · " + m.status.Cost
		}
		right = append(right, u)
	}
	r := strings.Join(right, " │ ") + " "
	room := m.width - ansi.StringWidth(r) - 1
	if room < 10 {
		r, room = "", m.width
	}
	left = ansi.Truncate(left, room, "…")
	pad := max(m.width-ansi.StringWidth(left)-ansi.StringWidth(r), 1)
	style := m.t.Status
	if m.busy {
		style = m.t.StatusBusy
	}
	return style.Render(ansi.Truncate(left+strings.Repeat(" ", pad)+r, m.width, ""))
}

func (m *Model) inputStyle() lipgloss.Style {
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
}
