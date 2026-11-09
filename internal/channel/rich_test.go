package channel

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// rich is a recording adapter with buttons, reactions and polls.
type rich struct {
	*recorder
	mu        sync.Mutex
	prompts   []string
	rows      [][][]Button
	reactions []string // "<message> <emoji>"
	polls     []string
}

func (r *rich) SendButtons(_ context.Context, _ Address, text string, rows [][]Button) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts = append(r.prompts, text)
	r.rows = append(r.rows, rows)
	return nil
}
func (r *rich) React(_ context.Context, _ Address, id, emoji string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reactions = append(r.reactions, id+" "+emoji)
	return nil
}
func (r *rich) SendPoll(_ context.Context, _ Address, q string, opts []string, multi bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.polls = append(r.polls, fmt.Sprintf("%s %v %v", q, opts, multi))
	return nil
}
func (r *rich) MaxPollOptions() int { return 10 }

func (r *rich) lastRows() [][]Button {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.rows) == 0 {
		return nil
	}
	return r.rows[len(r.rows)-1]
}

func (r *rich) snapshot() (prompts, reactions, polls []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.prompts), slices.Clone(r.reactions), slices.Clone(r.polls)
}

func newRichRig(t *testing.T, ch *config.Channel) (*rig, *rich) {
	r := newStateRig(t, ch)
	a := &rich{recorder: r.a}
	r.g.Adapters["tg"] = a
	return r, a
}

func press(r *rig, sender, data string) {
	r.g.Handle(context.Background(), Message{Account: "tg", Conversation: "100", ID: "cb", Sender: sender, Direct: true, Callback: data})
}

func TestApprovalButtons(t *testing.T) {
	r, a := newRichRig(t, &config.Channel{AllowFrom: []string{"7"}})
	send(r, "m1", "please run it")
	waitFor(t, "buttons", func() bool { return a.lastRows() != nil })
	rows := a.lastRows()
	if len(rows) != 1 || len(rows[0]) != 2 || rows[0][0].Text != "Approve" || rows[0][1].Style != ButtonDanger {
		t.Fatalf("rows = %+v", rows)
	}
	prompts, _, _ := a.snapshot()
	if !strings.Contains(prompts[0], "Approval needed") {
		t.Fatalf("prompt = %q", prompts[0])
	}
	press(r, "8", rows[0][0].Data) // not allowed: ignored
	press(r, "7", rows[0][0].Data)
	waitFor(t, "tool output", hasText(r.a, "approved-run"))
	waitFor(t, "ack", hasText(r.a, "Approved."))
	press(r, "7", rows[0][1].Data)
	waitFor(t, "stale press", hasText(r.a, "already been answered"))
}

func TestApprovalDenyButton(t *testing.T) {
	r, a := newRichRig(t, &config.Channel{AllowFrom: []string{"7"}})
	send(r, "m1", "run it")
	waitFor(t, "buttons", func() bool { return a.lastRows() != nil })
	press(r, "7", a.lastRows()[0][1].Data)
	waitFor(t, "denial", hasText(r.a, "tool said:"))
	if hasText(r.a, "approved-run\n")() {
		t.Fatal("denied command ran")
	}
}

func TestClarifyChoiceButtons(t *testing.T) {
	r, a := newRichRig(t, &config.Channel{AllowFrom: []string{"7"}})
	r.g.Runner.Models = &stepModel{fn: func(_ llm.Request, last llm.Message) *llm.Response {
		for _, b := range last.Blocks {
			if b.Type == llm.ToolResult {
				return text("got: " + b.Text)
			}
		}
		return toolCall("q1", "clarify", `{"questions":[{"question":"Which colour?","choices":["red","blue","green"]}]}`)
	}}
	send(r, "m1", "paint it")
	waitFor(t, "choices", func() bool { return a.lastRows() != nil })
	rows := a.lastRows()
	if len(rows) != 3 || rows[1][0].Text != "blue" {
		t.Fatalf("rows = %+v", rows)
	}
	press(r, "7", rows[1][0].Data)
	waitFor(t, "answer used", hasText(r.a, "got: The user answered:\nblue"))
}

func TestReactionsMarkTheTurn(t *testing.T) {
	r, a := newRichRig(t, &config.Channel{AllowFrom: []string{"7"}, Reactions: true})
	send(r, "m1", "hello")
	waitFor(t, "reply", hasText(r.a, "echo: hello"))
	waitFor(t, "done reaction", func() bool {
		_, re, _ := a.snapshot()
		return slices.Contains(re, "m1 "+ReactDone)
	})
	_, re, _ := a.snapshot()
	if re[0] != "m1 "+ReactWorking {
		t.Fatalf("reactions = %v", re)
	}
	// Off by default.
	r2, a2 := newRichRig(t, &config.Channel{AllowFrom: []string{"7"}})
	send(r2, "m1", "hello")
	waitFor(t, "reply", hasText(r2.a, "echo: hello"))
	if _, re, _ := a2.snapshot(); len(re) != 0 {
		t.Fatalf("reactions without the setting: %v", re)
	}
}

func TestPollNativeAndText(t *testing.T) {
	poll := `{"question":"Lunch?","options":["Pizza","Sushi"],"multiple":true}`
	model := &stepModel{fn: func(_ llm.Request, last llm.Message) *llm.Response {
		for _, b := range last.Blocks {
			if b.Type == llm.ToolResult {
				return text("poll: " + b.Text)
			}
		}
		return toolCall("p1", "poll", poll)
	}}
	r, a := newRichRig(t, &config.Channel{AllowFrom: []string{"7"}})
	r.g.Runner.Models = model
	send(r, "m1", "ask the group")
	waitFor(t, "native poll", hasText(r.a, "poll: Posted a poll with 2 options."))
	if _, _, polls := a.snapshot(); len(polls) != 1 || polls[0] != "Lunch? [Pizza Sushi] true" {
		t.Fatalf("polls = %v", polls)
	}
	// An adapter without polls gets a numbered list.
	r2 := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	r2.g.Runner.Models = model
	send(r2, "m1", "ask the group")
	waitFor(t, "text poll", hasText(r2.a, "**Poll:** Lunch?\n\n1. Pizza\n2. Sushi"))
	waitFor(t, "result", hasText(r2.a, "as a numbered list"))
}

func TestForkRewindAndTitleCommands(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	send(r, "m1", "first question")
	waitFor(t, "first", hasText(r.a, "echo: first question"))
	send(r, "m2", "second question")
	waitFor(t, "second", hasText(r.a, "echo: second question"))
	ctx := context.Background()
	orig, _ := r.g.Store.Conversation(ctx, Address{Account: "tg", Conversation: "100"})

	send(r, "m3", "/rewind")
	waitFor(t, "rewound", hasText(r.a, "Rewound: we carry on from before your last message"))
	conv, _ := r.g.Store.Conversation(ctx, Address{Account: "tg", Conversation: "100"})
	if conv.SessionID == orig.SessionID {
		t.Fatal("the conversation still points at the original")
	}
	recs, _ := r.g.Runner.Sessions.Messages(ctx, conv.SessionID)
	if len(recs) != 2 || !strings.Contains(recs[1].Message.Text(), "first question") {
		t.Fatalf("rewound transcript: %+v", recs)
	}
	if n, _ := r.g.Runner.Sessions.Messages(ctx, orig.SessionID); len(n) != 4 {
		t.Fatalf("original has %d messages", len(n))
	}

	send(r, "m4", "/fork")
	waitFor(t, "forked", hasText(r.a, "Forked: we carry on in a copy"))

	send(r, "m5", "/title Kitchen remodel")
	waitFor(t, "title set", hasText(r.a, "Title set: Kitchen remodel"))
	send(r, "m6", "/title")
	waitFor(t, "title shown", hasText(r.a, "Title: Kitchen remodel"))

	send(r, "m7", "/rewind 9")
	waitFor(t, "too far", hasText(r.a, "Couldn't rewind"))
	send(r, "m8", "/subagents")
	waitFor(t, "no tasks", hasText(r.a, "No sub-agent tasks are running"))
	send(r, "m9", "/subagents stop sub_nope")
	waitFor(t, "unknown task", hasText(r.a, "Couldn't stop it"))
}
