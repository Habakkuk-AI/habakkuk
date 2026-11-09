package tui

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// bridge carries messages from worker goroutines (approvals, questions)
// into the running program.
type bridge struct {
	mu   sync.Mutex
	send func(tea.Msg)
	done chan struct{} // closed when the program has ended
}

func newBridge() *bridge { return &bridge{done: make(chan struct{})} }

func (b *bridge) setSend(f func(tea.Msg)) {
	b.mu.Lock()
	b.send = f
	b.mu.Unlock()
}

// post delivers msg to the program; false when there is none.
func (b *bridge) post(msg tea.Msg) bool {
	b.mu.Lock()
	send := b.send
	b.mu.Unlock()
	select {
	case <-b.done:
		return false
	default:
	}
	if send == nil {
		return false
	}
	send(msg)
	return true
}

// decision is the user's answer to an approval prompt.
type decision int

const (
	deny decision = iota
	allowOnce
	allowSession
)

var promptIDs atomic.Int64

// approvalMsg asks the user to decide a tool call.
type approvalMsg struct {
	session, id, toolID string
	req                 tools.ApprovalRequest
	reply               chan decision
}

// clarifyMsg puts the clarify tool's questions to the user.
type clarifyMsg struct {
	session, id string
	questions   []tools.Question
	reply       chan string
}

// promptClosedMsg withdraws a prompt whose call gave up waiting (the turn
// was stopped).
type promptClosedMsg struct{ id string }

// approver is attached to the session the UI shows. It asks through the
// program and remembers "allow for this session" per tool. It also answers
// clarify questions (tools.Asker).
type approver struct {
	session string
	b       *bridge

	mu     sync.Mutex
	always map[string]bool
}

func newApprover(sessionID string, b *bridge) *approver {
	return &approver{session: sessionID, b: b, always: map[string]bool{}}
}

func nextPromptID() string { return "p" + strconv.FormatInt(promptIDs.Add(1), 10) }

func (a *approver) Approve(ctx context.Context, req tools.ApprovalRequest) (bool, error) {
	a.mu.Lock()
	ok := a.always[req.Tool]
	a.mu.Unlock()
	if ok {
		return true, nil
	}
	msg := approvalMsg{session: a.session, id: nextPromptID(), toolID: tools.CallID(ctx), req: req, reply: make(chan decision, 1)}
	if !a.b.post(msg) {
		return false, tools.ErrNoApprover
	}
	select {
	case d := <-msg.reply:
		if d == allowSession {
			a.mu.Lock()
			a.always[req.Tool] = true
			a.mu.Unlock()
		}
		return d != deny, nil
	case <-ctx.Done():
		a.b.post(promptClosedMsg{id: msg.id})
		return false, ctx.Err()
	case <-a.b.done:
		return false, tools.ErrNoApprover
	}
}

func (a *approver) Ask(ctx context.Context, qs []tools.Question) (string, error) {
	msg := clarifyMsg{session: a.session, id: nextPromptID(), questions: qs, reply: make(chan string, 1)}
	if !a.b.post(msg) {
		return "", nil
	}
	select {
	case ans := <-msg.reply:
		return ans, nil
	case <-ctx.Done():
		a.b.post(promptClosedMsg{id: msg.id})
		return "", ctx.Err()
	case <-a.b.done:
		return "", nil
	}
}

// approvalPrompt is an approval waiting on the user.
type approvalPrompt struct {
	approvalMsg
	sel decision
}

// clarifyPrompt is a question waiting on the user. With one question that
// has choices, the arrows pick one; otherwise the answer is typed.
type clarifyPrompt struct {
	clarifyMsg
	sel int
}

func (c *clarifyPrompt) choices() []string {
	if len(c.questions) == 1 {
		return c.questions[0].Choices
	}
	return nil
}
