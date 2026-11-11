package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

const maxQuestions = 5

type question struct {
	Question string   `json:"question"`
	Choices  []string `json:"choices"`
}

// clarifyTool asks the user questions in the chat and waits for the answer.
// It shares the approval machinery: one prompt at a time per conversation,
// the next ordinary message is the answer, and the same timeout applies.
type clarifyTool struct{ g *Gateway }

// ClarifyTool returns the clarify tool for processes with no gateway (a nil
// g): it can then only ask a terminal UI attached to the session.
func ClarifyTool(g *Gateway) tools.Tool { return clarifyTool{g} }

// Risk is read: asking the user changes nothing.
func (clarifyTool) Risk() tools.Risk { return tools.RiskRead }
func (clarifyTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "clarify",
		Description: `Ask the user 1 to 5 questions in the chat and wait for the answer, when you cannot go on sensibly without it (an ambiguous request, a choice only they can make, missing details). ` +
			`Give "choices" when the answer is one of a few options. The user's next message is returned as the answer; if no answer comes within the wait, go on with your best judgment and say what you assumed. ` +
			`Don't ask what you can find out yourself. Only works in chat conversations.`,
		Schema: json.RawMessage(`{"type":"object","properties":{
			"questions":{"type":"array","minItems":1,"maxItems":5,"items":{"type":"object","properties":{
				"question":{"type":"string"},
				"choices":{"type":"array","items":{"type":"string"},"description":"optional answers to pick from"}
			},"required":["question"]}}
		},"required":["questions"]}`),
	}
}

func (t clarifyTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	var in struct {
		Questions []question `json:"questions"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	qs := in.Questions[:0]
	for _, q := range in.Questions {
		if q.Question = strings.TrimSpace(q.Question); q.Question != "" {
			qs = append(qs, q)
		}
	}
	switch {
	case len(qs) == 0:
		return "", errors.New("give at least one question")
	case len(qs) > maxQuestions:
		return "", fmt.Errorf("ask at most %d questions at once", maxQuestions)
	}
	if asker, ok := env.Approver.(tools.Asker); ok { // a terminal UI attached to the session
		tq := make([]tools.Question, len(qs))
		for i, q := range qs {
			tq[i] = tools.Question(q)
		}
		answer, err := asker.Ask(ctx, tq)
		return ClarifyResult(answer), err
	}
	g := t.g
	if g == nil {
		return "", errors.New("clarify only works in a chat conversation or the terminal UI and this session is neither; ask your questions in your reply instead")
	}
	to, ok := g.origin(ctx, env.Session)
	if !ok {
		return "", errors.New("clarify only works in a chat conversation and this session is not one; ask your questions in your reply instead")
	}
	to.ReplyTo = ""
	var choices []string
	if len(qs) == 1 && len(qs[0].Choices) > 1 && len(qs[0].Choices) <= 10 {
		choices = qs[0].Choices // one question: its choices can be buttons
	}
	answer, err := g.ask(ctx, to, formatQuestions(qs), choices)
	if err != nil {
		return "", err
	}
	return ClarifyResult(answer), nil
}

// ClarifyResult is what the model is told after asking: the answer, or to go
// on without one.
func ClarifyResult(answer string) string {
	if answer == "" {
		return "The user did not answer in time. Go on with your best judgment and state the assumptions you made."
	}
	return "The user answered:\n" + answer
}

func formatQuestions(qs []question) string {
	var b strings.Builder
	if len(qs) == 1 {
		b.WriteString("**Question:** " + qs[0].Question)
		writeChoices(&b, qs[0].Choices)
	} else {
		b.WriteString("**A few questions:**")
		for i, q := range qs {
			fmt.Fprintf(&b, "\n\n%d. %s", i+1, q.Question)
			writeChoices(&b, q.Choices)
		}
	}
	b.WriteString("\n\nReply in one message.")
	return b.String()
}

func writeChoices(b *strings.Builder, choices []string) {
	for i, c := range choices {
		if i >= 26 {
			break
		}
		fmt.Fprintf(b, "\n   %c) %s", 'a'+i, strings.TrimSpace(c))
	}
}

// ask posts a prompt and waits for the next ordinary message in the
// conversation, or a press of one of choices' buttons where the chat has
// them. It returns "" when nobody answers in time.
func (g *Gateway) ask(ctx context.Context, to Address, prompt string, choices []string) (string, error) {
	release, ok := g.holdChat(ctx, to)
	if !ok {
		return "", ctx.Err()
	}
	defer release()

	answer := make(chan string, 1)
	g.questions.Store(to.key(), answer)
	defer g.questions.Delete(to.key())

	timeout := g.ApprovalTimeout
	if timeout == 0 {
		timeout = 15 * time.Minute
	}
	var rows [][]Button
	if len(choices) > 0 {
		var withdraw func()
		rows, withdraw = g.choiceButtons(to, choices)
		defer withdraw()
	}
	if err := g.sendChoice(ctx, to, prompt, rows); err != nil {
		return "", fmt.Errorf("could not post the question: %w", err)
	}
	select {
	case a := <-answer:
		return a, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(timeout):
		g.reply(to, g.t(to, "clarify.timeout"))
		return "", nil
	}
}

// answerQuestion consumes a reply to a pending clarify question. Commands
// still get through, so /stop works while a question waits.
func (g *Gateway) answerQuestion(to Address, text string) bool {
	ch, ok := g.questions.Load(to.key())
	if !ok || strings.HasPrefix(strings.TrimSpace(text), "/") {
		return false
	}
	select {
	case ch.(chan string) <- text:
		return true
	default:
		return false // already answered; this one is a new message
	}
}
