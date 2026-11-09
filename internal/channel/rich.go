package channel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// Rich messages: buttons for approvals and clarify choices, reactions on
// the user's message while a turn runs, and polls. Every one is optional
// per adapter; without it the chat gets plain text, as before.

// callbackPrefix starts the data of every button the gateway posts:
// "fy|<nonce>|<index>". The nonce names one prompt, so a press on an old
// prompt can never answer a newer one.
const callbackPrefix = "fy|"

// buttonHandler answers a press of button i. It returns a short notice
// for the chat, or "".
type buttonHandler func(i int) string

// offer registers a prompt's buttons and returns them with their data, and
// a func that withdraws them once the prompt is settled.
func (g *Gateway) offer(labels []Button, h buttonHandler) ([][]Button, func()) {
	var b [6]byte
	rand.Read(b[:])
	nonce := hex.EncodeToString(b[:])
	g.buttons.Store(nonce, h)
	rows := make([][]Button, 0, len(labels))
	var row []Button
	for i, l := range labels {
		l.Data = fmt.Sprintf("%s%s|%d", callbackPrefix, nonce, i)
		l.Text = buttonLabel(l.Text)
		row = append(row, l)
		// Approve and Deny sit side by side; choices go one per row, where
		// long labels fit.
		if len(labels) > 2 || i == len(labels)-1 {
			rows = append(rows, row)
			row = nil
		}
	}
	return rows, func() { g.buttons.Delete(nonce) }
}

// buttonLabel fits a label in every platform's limit (Slack takes 75).
func buttonLabel(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 70 {
		s = string([]rune(s)[:69]) + "…"
	}
	if s == "" {
		s = "?"
	}
	return s
}

// sendChoice posts a prompt, with buttons where the adapter has them and
// as plain text otherwise (or when posting the buttons fails).
func (g *Gateway) sendChoice(ctx context.Context, to Address, markdown string, rows [][]Button) error {
	a := g.Adapters[to.Account]
	if a == nil {
		return errNoAdapter
	}
	if bs, ok := a.(ButtonSender); ok && len(rows) > 0 && utf8.RuneCountInString(markdown) <= chunkLimit(a) {
		err := bs.SendButtons(ctx, to, render(a, markdown), rows)
		if err == nil {
			return nil
		}
		g.Log.Warn("posting buttons failed; sending the prompt as text", "channel", to.Account, "err", err)
	}
	return g.send(ctx, to, markdown)
}

// callback handles a button press. Only senders who may talk to the bot
// can press; a press on a prompt that is no longer waiting says so.
func (g *Gateway) callback(ctx context.Context, ch *config.Channel, m Message, to Address) {
	if ok, err := g.allowed(ctx, ch, m); err != nil || !ok {
		return
	}
	// A guest's press could be the owner's pending approval: refuse it.
	if _, waiting := g.pending.Load(to.key()); waiting && !g.owner(ctx, ch, m) {
		return
	}
	to.ReplyTo = ""
	rest, ok := strings.CutPrefix(m.Callback, callbackPrefix)
	if !ok {
		return
	}
	nonce, idx, _ := strings.Cut(rest, "|")
	var i int
	if _, err := fmt.Sscan(idx, &i); err != nil {
		return
	}
	h, ok := g.buttons.LoadAndDelete(nonce)
	if !ok {
		g.reply(to, g.t(to, "button.expired"))
		return
	}
	if notice := h.(buttonHandler)(i); notice != "" {
		g.reply(to, notice)
	}
}

// approvalButtons are the Approve and Deny buttons of an approval prompt.
func (g *Gateway) approvalButtons(to Address) ([][]Button, func()) {
	buttons := []Button{{Text: g.t(to, "button.approve"), Style: ButtonPrimary}, {Text: g.t(to, "button.deny"), Style: ButtonDanger}}
	return g.offer(buttons, func(i int) string {
		g.resolveApproval(to, i == 0)
		if i == 0 {
			return g.t(to, "approval.approved")
		}
		return g.t(to, "approval.denied")
	})
}

// choiceButtons are buttons for one clarify question's choices; a press
// answers the question with the choice's text.
func (g *Gateway) choiceButtons(to Address, choices []string) ([][]Button, func()) {
	labels := make([]Button, len(choices))
	for i, c := range choices {
		labels[i] = Button{Text: strings.TrimSpace(c)}
	}
	return g.offer(labels, func(i int) string {
		if i < 0 || i >= len(choices) {
			return ""
		}
		if g.answerQuestion(to, choices[i]) {
			return g.t(to, "button.answer", buttonLabel(choices[i]))
		}
		return ""
	})
}

// Reactions on the user's message while a turn runs.
const (
	ReactWorking = "👀"
	ReactDone    = "👍"
	ReactFailed  = "😢"
)

// react sets the bot's reaction on the message a turn answers, on channels
// with reactions on. Failures only cost the reaction.
func (g *Gateway) react(ctx context.Context, to Address, emoji string) {
	ch := g.Channels[to.Account]
	if ch == nil || !ch.Reactions || to.ReplyTo == "" {
		return
	}
	r, ok := g.Adapters[to.Account].(Reactor)
	if !ok {
		return
	}
	if err := r.React(context.WithoutCancel(ctx), to, to.ReplyTo, emoji); err != nil {
		g.Log.Debug("reaction failed", "channel", to.Account, "err", err)
	}
}

// reactEnd marks how a turn ended: done, failed for good, or (stopped or
// interrupted, to be resumed) no reaction left behind.
func (g *Gateway) reactEnd(ctx context.Context, j *jobs.Job, to *Address, err error) {
	if to == nil {
		return
	}
	switch {
	case err == nil:
		g.react(ctx, *to, ReactDone)
	case errors.As(err, new(jobs.Permanent)) || j.Attempts >= j.MaxAttempts:
		g.react(ctx, *to, ReactFailed)
	case ctx.Err() != nil || errors.Is(err, jobs.ErrLeaseLost):
		g.react(ctx, *to, "") // stopped, or resumed later (which reacts again)
	}
}

// poll

const (
	maxPollQuestion = 300
	maxPollOption   = 55 // Discord's limit; Telegram allows 100
)

type pollTool struct{ g *Gateway }

// Risk is network: the poll goes to the chat.
func (pollTool) Risk() tools.Risk { return tools.RiskNetwork }
func (pollTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "poll",
		Description: `Post a poll in the current chat (2 to 10 options), e.g. to let a group pick a time or a place. Where the platform has native polls (Telegram, Discord) people vote with a tap; elsewhere the poll is a numbered list they answer in text. ` +
			`You are not told the votes; ask people to tell you, or read the replies. Only works in chat conversations.`,
		Schema: json.RawMessage(`{"type":"object","properties":{
			"question":{"type":"string"},
			"options":{"type":"array","minItems":2,"maxItems":10,"items":{"type":"string"}},
			"multiple":{"type":"boolean","description":"allow more than one answer"}
		},"required":["question","options"]}`),
	}
}

func (t pollTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	var in struct {
		Question string   `json:"question"`
		Options  []string `json:"options"`
		Multiple bool     `json:"multiple"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	in.Question = strings.TrimSpace(in.Question)
	var opts []string
	for _, o := range in.Options {
		if o = strings.TrimSpace(o); o != "" {
			opts = append(opts, o)
		}
	}
	switch {
	case in.Question == "":
		return "", errors.New("question is required")
	case utf8.RuneCountInString(in.Question) > maxPollQuestion:
		return "", fmt.Errorf("keep the question under %d characters", maxPollQuestion)
	case len(opts) < 2 || len(opts) > 10:
		return "", errors.New("give 2 to 10 options")
	}
	g := t.g
	to, ok := g.origin(ctx, env.Session)
	if !ok {
		return "", errors.New("poll only works in a chat conversation; list the options in your reply instead")
	}
	to.ReplyTo = ""
	a := g.Adapters[to.Account]
	if a == nil {
		return "", errNoAdapter
	}
	native := true
	for _, o := range opts {
		if utf8.RuneCountInString(o) > maxPollOption {
			native = false
		}
	}
	if p, ok := a.(Poller); ok && native && len(opts) <= p.MaxPollOptions() {
		err := p.SendPoll(ctx, to, in.Question, opts, in.Multiple)
		if err == nil {
			return fmt.Sprintf("Posted a poll with %d options.", len(opts)), nil
		}
		g.Log.Warn("native poll failed; posting it as text", "channel", to.Account, "err", err)
	}
	var b strings.Builder
	b.WriteString("**Poll:** " + in.Question + "\n")
	for i, o := range opts {
		fmt.Fprintf(&b, "\n%d. %s", i+1, o)
	}
	if in.Multiple {
		b.WriteString("\n\nReply with the numbers of your choices.")
	} else {
		b.WriteString("\n\nReply with the number of your choice.")
	}
	if err := g.send(ctx, to, b.String()); err != nil {
		return "", err
	}
	return fmt.Sprintf("Posted the poll as a numbered list (%d options); answers will come as messages.", len(opts)), nil
}
