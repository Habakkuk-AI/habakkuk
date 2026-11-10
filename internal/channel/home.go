package channel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// Home chats. An agent with `home: CHANNEL:CONVERSATION` tells its owner
// there about work that started elsewhere: the gateway posts a short result
// when such a turn finishes (report_home: results), and a line when it
// starts (all), and the agent can post itself with message_home. Posts are
// ordinary durable deliveries with ids derived from the turn or the tool
// call, so a retried turn or tool call posts once. A post never starts a
// turn, so reports cannot loop.

// Aliaser is implemented by adapters whose conversations have aliases,
// such as whatsappagent's "creator". Resolve returns the conversation an
// alias stands for right now, or false when it is not an alias or is not
// known yet.
type Aliaser interface {
	Resolve(conversation string) (string, bool)
}

const (
	// homeResultLimit is how much of a final reply a home report quotes.
	homeResultLimit = 1200
	// maxHomeText caps a message_home post: a message, not a document.
	maxHomeText = 4000
	// statusLimit is the longest message_home text a turn that read
	// external content may send.
	statusLimit = 280
)

// home parses an agent's home target and checks its channel is running.
func (g *Gateway) home(target string) (Address, error) {
	to, err := ParseTarget(target)
	if err != nil {
		return Address{}, err
	}
	if g.Adapters[to.Account] == nil {
		return Address{}, fmt.Errorf("%w %q", errNoAdapter, to.Account)
	}
	return to, nil
}

// atHome reports whether a reply address (JSON, as turns carry it) is the
// home chat. Without a thread in the home, any thread of its conversation
// counts as home; an alias in the home (wagent:creator) matches the
// conversation it stands for.
func (g *Gateway) atHome(home Address, raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var at Address
	if json.Unmarshal(raw, &at) != nil || at.Account != home.Account {
		return false
	}
	if home.Thread != "" && at.Thread != home.Thread {
		return false
	}
	if at.Conversation == home.Conversation {
		return true
	}
	if al, ok := g.Adapters[home.Account].(Aliaser); ok {
		if conv, ok := al.Resolve(home.Conversation); ok && conv == at.Conversation {
			return true
		}
	}
	return false
}

// ReportHome posts a turn's news to its agent's home chat, unless the turn
// came from that chat (its reply goes there already) or, for a delegated
// task, the turn that delegated it did. It implements agent.HomeReporter.
func (g *Gateway) ReportHome(ctx context.Context, n agent.HomeNews) error {
	home, err := g.home(n.Home)
	if err != nil {
		return err
	}
	if g.atHome(home, n.Origin) || g.atHome(home, n.ParentOrigin) {
		return nil // no report to the chat the work came from
	}
	var text string
	if n.Started {
		text = "Working on: " + orTask(n.Task)
		if who := g.requester(n); who != "" {
			text += "\n_Asked by " + who + "._"
		}
	} else {
		final := strings.TrimSpace(n.Final)
		if final == Silent {
			return nil
		}
		text = g.resultText(n, final)
	}
	p := deliverPayload{To: home, Text: text, Agent: n.Agent, Session: n.Session}
	if n.Source == agent.SourceSchedule {
		p.What = n.Task
	}
	return g.enqueue(ctx, homePostID(n.Job, n.Started), p)
}

// resultText is a finished turn's home report: the task, its final reply
// clipped to a few paragraphs, and where the full reply went.
func (g *Gateway) resultText(n agent.HomeNews, final string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Done:** %s", orTask(n.Task))
	clipped := final
	if utf8.RuneCountInString(final) > homeResultLimit {
		clipped = clip(final, homeResultLimit)
	}
	if clipped != "" {
		b.WriteString("\n\n" + clipped)
	}
	who := g.requester(n)
	switch {
	case clipped != final && who != "":
		fmt.Fprintf(&b, "\n\n_Shortened here; the full result went to %s._", who)
	case clipped != final:
		fmt.Fprintf(&b, "\n\n_Shortened here; the full result is in session %s._", n.Session)
	case who != "":
		fmt.Fprintf(&b, "\n\n_The full result went to %s._", who)
	}
	return b.String()
}

// requester names who asked for the work and got its full result, or ""
// when nobody else did (a schedule that delivers nowhere).
func (g *Gateway) requester(n agent.HomeNews) string {
	switch {
	case n.Kind == agent.KindSubturn && n.ParentAgent != "":
		return "agent " + n.ParentAgent
	case n.Kind == agent.KindSubturn:
		return "the agent that delegated it"
	case n.Source == agent.SourceBoard:
		return "the board"
	case n.Source == agent.SourceA2A:
		return "the A2A client"
	}
	var at Address
	if len(n.Origin) > 0 && json.Unmarshal(n.Origin, &at) == nil && at.Account != "" {
		return "the " + at.Account + " chat"
	}
	return ""
}

func orTask(task string) string {
	if task == "" {
		return "a task"
	}
	return task
}

// clip shortens s to at most n runes, at a paragraph, line or word break
// when one falls in the last third, and marks the cut.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n])
	for _, sep := range []string{"\n\n", "\n", " "} {
		if i := strings.LastIndex(cut, sep); i >= len(cut)*2/3 {
			cut = cut[:i]
			break
		}
	}
	return strings.TrimRight(cut, " \n") + " …"
}

// homePostID is the delivery id of a turn's start or result post.
func homePostID(job string, started bool) string {
	key := job + "\x00done"
	if started {
		key = job + "\x00start"
	}
	sum := sha256.Sum256([]byte(key))
	return "home_" + hex.EncodeToString(sum[:12])
}

// messageHomeTool posts to the agent's own home chat.
//
// Its risk is read: like a reply, it reaches only the owner's own chat, so
// it runs in every permission mode without asking. What it must not become
// is a channel for text planted in a web page or an email to reach the
// owner looking like the agent's own words. So once external content has
// entered the turn (the turn is tainted), it sends only a short status:
// at most 280 characters, no links and no code. Longer findings go in the
// final reply, which the owner reads as the answer to what they asked.
type messageHomeTool struct{ g *Gateway }

func (messageHomeTool) Risk() tools.Risk { return tools.RiskRead }
func (messageHomeTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        agent.MessageHomeName,
		Description: `Post a message (Markdown) to your home chat: the chat where your owner talks to you directly. Use it to tell the owner about progress or results of work that did not start in that chat, such as a task another agent delegated to you, a board card or a scheduled run, so they hear about it in your chat as well as through whoever asked. Do not use it to answer in the conversation you are in: your reply does that. If report_home is on, a short version of your final reply is posted there when such work finishes, so do not repeat it; use this for progress, decisions you need from the owner, or results worth more than a summary. After you have read web pages, search results or other external content in this turn, only a short status (up to 280 characters, no links or code) can be sent.`,
		Schema: json.RawMessage(`{"type":"object","properties":{
			"text":{"type":"string","description":"the message, in Markdown"}
		},"required":["text"]}`),
	}
}

func (t messageHomeTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	text := strings.TrimSpace(in.Text)
	g := t.g
	spec := g.Runner.Agents[env.Agent]
	switch {
	case spec == nil || spec.Home == "":
		return "", errors.New("you have no home chat (the agent's home: setting); tell the user in your reply instead")
	case text == "":
		return "", errors.New("text is required")
	case utf8.RuneCountInString(text) > maxHomeText:
		return "", fmt.Errorf("the message is %d characters; keep it under %d and put the rest in your reply", utf8.RuneCountInString(text), maxHomeText)
	}
	home, err := g.home(spec.Home)
	if err != nil {
		return "", err
	}
	if g.atHome(home, agent.OriginFrom(ctx)) {
		return "", errors.New("this conversation is your home chat; just reply here")
	}
	if env.Taint.Marked() {
		if why := notStatus(text); why != "" {
			return "", fmt.Errorf("%w: this turn has read external content (web pages, search results, tool output from outside, or an unvetted message), so message_home sends only a short status: %s. Put the details in your final reply instead", tools.ErrDenied, why)
		}
	}
	p := deliverPayload{To: home, Text: text, Agent: env.Agent, Session: env.Session, What: agent.PosterFrom(ctx).What}
	if err := g.enqueue(ctx, homeMessageID(env.Session, tools.CallID(ctx), text), p); err != nil {
		return "", err
	}
	return "Posted to your home chat (" + spec.Home + ").", nil
}

// notStatus says why text is more than a short status, or "".
func notStatus(text string) string {
	lower := strings.ToLower(text)
	switch {
	case utf8.RuneCountInString(text) > statusLimit:
		return fmt.Sprintf("it is %d characters, over %d", utf8.RuneCountInString(text), statusLimit)
	case strings.Contains(lower, "http://") || strings.Contains(lower, "https://") || strings.Contains(lower, "www.") ||
		strings.Contains(text, "](") || strings.Contains(lower, "mailto:"):
		return "it contains a link"
	case strings.Contains(text, "`"):
		return "it contains code"
	}
	return ""
}

// homeMessageID makes one post per tool call, like send_file's ids: the
// call id is fixed once the model made the call.
func homeMessageID(sessionID, callID, text string) string {
	key := sessionID + "\x00" + callID
	if callID == "" {
		key += "\x00" + text
	}
	sum := sha256.Sum256([]byte(key))
	return "homemsg_" + hex.EncodeToString(sum[:12])
}
