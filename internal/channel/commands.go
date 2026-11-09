package channel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/core/state"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/ops/i18n"
)

// Command is a chat command. The gateway's built-ins (/new, /stop,
// /status, /compact, /help) are handled in gateway.go; everything else is
// listed in commands(), which /help prints.
type Command struct {
	Name    string // with the slash, e.g. "/retry"
	Aliases []string
	Args    string // shown in /help, e.g. "<question>"
	Help    string
	Run     func(ctx context.Context, c *Call)
}

// Call is one invocation of a command.
type Call struct {
	G       *Gateway
	Channel *config.Channel
	To      Address
	Pick    string  // the agent the conversation should use
	Args    string  // the text after the command name
	Msg     Message // the message carrying the command
}

// Reply answers in the chat.
func (c *Call) Reply(markdown string) { c.G.reply(c.To, markdown) }

// commands lists the chat commands beyond the built-ins, in /help order.
func (g *Gateway) commands() []Command {
	return []Command{
		{Name: "/retry", Help: "run my last message again", Run: g.cmdRetry},
		{Name: "/btw", Args: "<question>", Help: "a side question, answered without adding it to this conversation", Run: g.cmdBtw},
		{Name: "/steer", Args: "<text>", Help: "guidance for the task I'm working on, taken in before my next step", Run: g.cmdSteer},
		{Name: "/queue", Args: "<text>", Help: "a message to start on once the current task is done", Run: g.cmdQueue},
		{Name: "/think", Args: "low|medium|high|xhigh|max|default", Help: "how hard I think in this conversation", Run: g.cmdThink},
		{Name: "/personality", Aliases: []string{"/persona"}, Args: "[name|off]", Help: "switch my persona for this conversation", Run: g.cmdPersonality},
		{Name: "/goal", Args: "[-turns N] <text> | off", Help: "a standing goal I keep working on, turn after turn, until it is done", Run: g.cmdGoal},
		{Name: "/export", Help: "send this conversation as a Markdown file", Run: g.cmdExport},
		{Name: "/whoami", Help: "who you are to me, and where we are talking", Run: g.cmdWhoami},
		{Name: "/agents", Help: "list the agents you can talk to", Run: g.cmdAgents},
		{Name: "/agent", Args: "<name>|default", Help: "switch this conversation to another agent", Run: g.cmdAgent},
		{Name: "/remember", Args: "<text>", Help: "save something to my long-term memory, word for word", Run: g.cmdRemember},
		{Name: "/forget", Args: "<what or slug>", Help: "remove a memory (it goes to the memory trash)", Run: g.cmdForget},
		{Name: "/memory", Help: "the latest changes to my memory, profile, identity and skills, with ids", Run: g.cmdMemory},
		{Name: "/undo-memory", Args: "<id>", Help: "put back what one memory change replaced (ids from /memory)", Run: g.cmdUndoMemory},
		{Name: "/fork", Help: "carry on in a copy of this conversation, to try another direction", Run: g.cmdFork},
		{Name: "/rewind", Args: "[N]", Help: "carry on from before your last N messages (the original is kept)", Run: g.cmdRewind},
		{Name: "/title", Args: "[text]", Help: "show or set this conversation's title", Run: g.cmdTitle},
		{Name: "/subagents", Aliases: []string{"/tasks"}, Args: "[stop ID]", Help: "list the sub-agent tasks running for this conversation, or stop one", Run: g.cmdSubagents},
	}
}

// commandHelp lists the commands for /help, each described in lang when
// the catalog has it (help.<name>) and in English otherwise.
func (g *Gateway) commandHelp(lang string) string {
	var b strings.Builder
	for _, c := range g.commands() {
		b.WriteString("\n" + c.Name)
		if c.Args != "" {
			b.WriteString(" " + c.Args)
		}
		help := c.Help
		if key := "help." + strings.TrimPrefix(c.Name, "/"); i18n.Has(key) {
			help = i18n.T(lang, key)
		}
		b.WriteString(": " + help)
	}
	return b.String()
}

// runCommand runs a registered command. It returns false for names it does
// not know, which then go to the agent as ordinary text.
func (g *Gateway) runCommand(ctx context.Context, ch *config.Channel, to Address, pick, text string) bool {
	first, args, _ := strings.Cut(strings.TrimSpace(text), " ")
	if i := strings.IndexAny(first, "\n"); i >= 0 { // "/btw\nquestion"
		first, args = first[:i], first[i+1:]+" "+args
	}
	name, _, _ := strings.Cut(strings.ToLower(first), "@")
	for _, c := range g.commands() {
		if c.Name == name || slices.Contains(c.Aliases, name) {
			m, _ := ctx.Value(messageKey{}).(Message)
			if ownerOnly[c.Name] && m.Account != "" && !g.owner(ctx, ch, m) {
				g.reply(to, c.Name+": only the owner can do that here.")
				return true
			}
			c.Run(ctx, &Call{G: g, Channel: ch, To: to, Pick: pick, Args: strings.TrimSpace(args), Msg: m})
			return true
		}
	}
	return false
}

// ownerOnly are the commands guests (senders let in by allow_from "*")
// may not run: they write trusted memory or move the chat to another agent.
var ownerOnly = map[string]bool{"/remember": true, "/forget": true, "/agent": true}

// messageKey carries the inbound message to the command it holds.
type messageKey struct{}

func withMessage(ctx context.Context, m Message) context.Context {
	return context.WithValue(ctx, messageKey{}, m)
}

// pickAgent is the agent a message should go to: the one the sender picked
// (web chat), else the one chosen for this conversation with /agent.
func (g *Gateway) pickAgent(ctx context.Context, to Address, picked string) string {
	if picked != "" || g.State == nil {
		return picked
	}
	name, err := g.State.ConversationAgent(ctx, to.key())
	if err != nil {
		g.Log.Warn("read conversation agent failed", "err", err)
	}
	return name
}

// session returns the conversation's session, starting one when there is
// none yet.
func (c *Call) session(ctx context.Context) (*Conversation, bool) {
	conv, err := c.G.conversation(ctx, c.Channel, c.To, c.Pick, false)
	if err != nil {
		c.G.Log.Error("open conversation failed", "err", err)
		c.Reply(c.T("conv.open_failed"))
		return nil, false
	}
	return conv, true
}

func (c *Call) needState() bool {
	if c.G.State == nil {
		c.Reply(c.T("cmd.unavailable"))
		return false
	}
	return true
}

// submit queues text as a turn of the conversation's session, as an
// ordinary message would be. id makes a redelivered command run once.
func (c *Call) submit(ctx context.Context, conv *Conversation, id, text string) bool {
	if !c.Msg.Direct && c.Msg.SenderName != "" {
		text = c.Msg.SenderName + ": " + text
	}
	origin, _ := json.Marshal(c.To)
	if _, err := c.G.Runner.SubmitTurn(ctx, agent.Turn{ID: id, Session: conv.SessionID, Text: text, Origin: origin}); err != nil {
		c.G.Log.Error("queue turn failed", "err", err)
		c.Reply(c.T("cmd.take_failed"))
		return false
	}
	if err := c.G.Store.Touch(ctx, c.To); err != nil {
		c.G.Log.Warn("touch conversation failed", "err", err)
	}
	if c.G.Wake != nil {
		c.G.Wake()
	}
	return true
}

// jobID derives a job id from the command's message, so a redelivered
// command maps to the same job.
func (c *Call) jobID(prefix string) string {
	if c.Msg.ID == "" {
		return "" // no platform id: a fresh job
	}
	return prefix + strings.TrimPrefix(TurnID(c.Msg), "turn_")
}

func (c *Call) busy(ctx context.Context, conv *Conversation) bool {
	busy, err := c.G.Queue.Busy(ctx, conv.SessionID, "")
	return err == nil && busy
}

// /retry

func (g *Gateway) cmdRetry(ctx context.Context, c *Call) {
	conv, err := g.Store.Conversation(ctx, c.To)
	if err != nil || conv == nil {
		c.Reply(c.T("retry.nothing"))
		return
	}
	recs, err := g.Runner.Sessions.Messages(ctx, conv.SessionID)
	if err != nil {
		c.Reply(c.T("conv.read_failed"))
		return
	}
	text, ok := lastUserText(recs)
	if !ok {
		c.Reply(c.T("retry.nothing"))
		return
	}
	c.submit(ctx, conv, c.jobID("retry_"), "[The user asked you to try this message again; answer it afresh.]\n"+text)
}

// synthetic marks user messages Habakkuk wrote itself, which /retry skips.
var synthetic = []string{"[The user asked you to try", "[Standing goal", "[Guidance", "[Side question", "[Scheduled task", "[Task from another agent", agent.BackgroundPrefix}

// lastUserText is the newest message the user typed, without its time
// stamp and task list.
func lastUserText(recs []session.Record) (string, bool) {
	for i := len(recs) - 1; i >= 0; i-- {
		m := recs[i].Message
		if m.Role != llm.User || len(m.Blocks) == 0 || m.Blocks[0].Type != llm.Text {
			continue
		}
		text := m.Blocks[0].Text
		if strings.HasPrefix(text, "[") { // drop the "[Mon 2006-01-02 15:04 MST]" stamp line
			if _, rest, ok := strings.Cut(text, "\n"); ok {
				text = rest
			}
		}
		text, _, _ = strings.Cut(text, state.TaskListMarker)
		text = strings.TrimSpace(text)
		if text == "" || slices.ContainsFunc(synthetic, func(p string) bool { return strings.HasPrefix(text, p) }) {
			continue
		}
		return text, true
	}
	return "", false
}

// /btw

const maxBtwContext = 40000

func (g *Gateway) cmdBtw(ctx context.Context, c *Call) {
	if c.Args == "" {
		c.Reply(c.T("btw.usage"))
		return
	}
	agentName := c.Pick
	var history string
	if conv, err := g.Store.Conversation(ctx, c.To); err == nil && conv != nil {
		agentName = conv.Agent
		history = g.renderContext(ctx, conv.SessionID, maxBtwContext)
	}
	if _, ok := g.Runner.Agents[agentName]; !ok {
		agentName = c.Channel.AgentFor(c.To.Conversation)
	}
	sess, err := g.Runner.NewSession(ctx, agentName)
	if err != nil {
		g.Log.Error("side session failed", "err", err)
		c.Reply(c.T("btw.side_failed"))
		return
	}
	text := "[Side question. The user asked this in passing while your main conversation with them goes on. Answer it briefly. This exchange is not added to the main conversation.]"
	if history != "" {
		text += "\n\n<main_conversation>\n" + history + "\n</main_conversation>"
	}
	text += "\n\nQuestion: " + c.Args
	origin, _ := json.Marshal(c.To)
	// Its own session, so it runs beside the main conversation, not after it.
	if _, err := g.Runner.SubmitTurn(ctx, agent.Turn{ID: c.jobID("btw_"), Session: sess.ID, Text: text, Origin: origin}); err != nil {
		g.Log.Error("queue side question failed", "err", err)
		c.Reply(c.T("btw.queue_failed"))
		return
	}
	if g.Wake != nil {
		g.Wake()
	}
}

// renderContext renders what the model currently sees of a session (the
// latest summary and the messages after it) as plain text, keeping the
// newest maxChars.
func (g *Gateway) renderContext(ctx context.Context, sessionID string, maxChars int) string {
	recs, err := g.Runner.Sessions.Messages(ctx, sessionID)
	if err != nil {
		return ""
	}
	from := 0
	var parts []string
	if c, err := g.Runner.Sessions.LatestCompaction(ctx, sessionID); err == nil && c != nil {
		from = c.KeepFrom
		parts = append(parts, "[summary of earlier messages] "+c.Summary)
	}
	for _, r := range recs {
		if r.Seq < from {
			continue
		}
		for _, b := range r.Message.Blocks {
			switch b.Type {
			case llm.Text:
				if strings.TrimSpace(b.Text) != "" {
					parts = append(parts, fmt.Sprintf("[%s] %s", r.Message.Role, shortenKeep(b.Text, 4000)))
				}
			case llm.ToolUse:
				parts = append(parts, fmt.Sprintf("[assistant called %s] %s", b.ToolName, shortenKeep(string(b.Input), 300)))
			case llm.ToolResult:
				parts = append(parts, "[tool result] "+shortenKeep(b.Text, 600))
			}
		}
	}
	size, i := 0, len(parts)
	for i > 0 && size+len(parts[i-1]) < maxChars {
		i--
		size += len(parts[i]) + 1
	}
	return strings.Join(parts[i:], "\n")
}

// shortenKeep cuts s to n runes, keeping line breaks.
func shortenKeep(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// /steer and /queue

func (g *Gateway) cmdSteer(ctx context.Context, c *Call) {
	if c.Args == "" {
		c.Reply(c.T("steer.usage"))
		return
	}
	if !c.needState() {
		return
	}
	conv, ok := c.session(ctx)
	if !ok {
		return
	}
	if !c.busy(ctx, conv) {
		if c.submit(ctx, conv, c.jobID("turn_"), c.Args) {
			c.Reply(c.T("steer.as_message"))
		}
		return
	}
	if err := g.State.AddSteer(ctx, conv.SessionID, c.Args); err != nil {
		g.Log.Error("queue steering failed", "err", err)
		c.Reply(c.T("steer.failed"))
		return
	}
	c.Reply(c.T("steer.done"))
}

func (g *Gateway) cmdQueue(ctx context.Context, c *Call) {
	if c.Args == "" {
		c.Reply(c.T("queue.usage"))
		return
	}
	conv, ok := c.session(ctx)
	if !ok {
		return
	}
	busy := c.busy(ctx, conv)
	// Turns of one session run one at a time in order, so a queued message
	// simply waits for the turn in progress.
	if c.submit(ctx, conv, c.jobID("turn_"), c.Args) && busy {
		c.Reply(c.T("queue.done"))
	}
}

// /think and /personality

var efforts = []string{"low", "medium", "high", "xhigh", "max"}

func (g *Gateway) cmdThink(ctx context.Context, c *Call) {
	if !c.needState() {
		return
	}
	conv, ok := c.session(ctx)
	if !ok {
		return
	}
	level := strings.ToLower(c.Args)
	if level == "" {
		st, _ := g.State.Settings(ctx, conv.SessionID)
		cur := st.Effort
		if cur == "" {
			cur = c.T("think.agent_default")
			if spec, ok := g.Runner.Agents[conv.Agent]; ok && spec.Effort != "" {
				cur += " (" + spec.Effort + ")"
			}
		}
		c.Reply(c.T("think.current", cur, strings.Join(efforts, "|")))
		return
	}
	if level == "default" || level == "off" || level == "reset" {
		level = ""
	} else if !slices.Contains(efforts, level) {
		c.Reply(c.T("think.pick", strings.Join(efforts, ", ")))
		return
	}
	if err := g.State.SetEffort(ctx, conv.SessionID, level); err != nil {
		c.Reply(c.T("change.failed"))
		return
	}
	if level == "" {
		c.Reply(c.T("think.reset"))
	} else {
		c.Reply(c.T("think.set", level))
	}
}

func (g *Gateway) cmdPersonality(ctx context.Context, c *Call) {
	if !c.needState() {
		return
	}
	conv, ok := c.session(ctx)
	if !ok {
		return
	}
	name := strings.ToLower(c.Args)
	if name == "" {
		st, _ := g.State.Settings(ctx, conv.SessionID)
		cur := st.Personality
		if cur == "" {
			cur = c.T("persona.none")
		}
		c.Reply(c.T("persona.current", cur, strings.Join(g.Personas.List(), ", ")))
		return
	}
	if name == "off" || name == "default" || name == "none" {
		name = ""
	} else if _, err := g.Personas.Text(name); err != nil {
		c.Reply(c.T("persona.unknown", name, strings.Join(g.Personas.List(), ", ")))
		return
	}
	if err := g.State.SetPersonality(ctx, conv.SessionID, name); err != nil {
		c.Reply(c.T("change.failed"))
		return
	}
	if name == "" {
		c.Reply(c.T("persona.off"))
	} else {
		c.Reply(c.T("persona.set", name))
	}
}

// /goal

const (
	// GoalDone and GoalBlocked end a goal turn's reply when the goal is met
	// or the agent needs the user.
	GoalDone    = "GOAL DONE"
	GoalBlocked = "GOAL BLOCKED"

	defaultGoalTurns = 10
	maxGoalTurns     = 100
)

func (g *Gateway) cmdGoal(ctx context.Context, c *Call) {
	if !c.needState() {
		return
	}
	conv, ok := c.session(ctx)
	if !ok {
		return
	}
	args := c.Args
	switch strings.ToLower(args) {
	case "":
		goal, _ := g.State.Goal(ctx, conv.SessionID)
		if goal == nil {
			c.Reply(c.T("goal.none"))
		} else {
			c.Reply(c.T("goal.current", goal.Text, goal.Turns, goal.Budget))
		}
		return
	case "off", "stop", "clear", "cancel":
		if had, _ := g.State.ClearGoal(ctx, conv.SessionID); had {
			c.Reply(c.T("goal.cleared"))
		} else {
			c.Reply(c.T("goal.no_goal"))
		}
		return
	}
	budget := defaultGoalTurns
	if rest, ok := strings.CutPrefix(args, "-turns "); ok {
		n, text, _ := strings.Cut(strings.TrimSpace(rest), " ")
		v, err := strconv.Atoi(n)
		if err != nil || v < 1 || v > maxGoalTurns {
			c.Reply(c.T("goal.turns_range", maxGoalTurns))
			return
		}
		budget, args = v, strings.TrimSpace(text)
	}
	if args == "" {
		c.Reply(c.T("goal.usage"))
		return
	}
	origin, _ := json.Marshal(c.To)
	if err := g.State.SetGoal(ctx, conv.SessionID, args, budget, string(origin)); err != nil {
		c.Reply(c.T("goal.set_failed"))
		return
	}
	goal := &state.Goal{Text: args, Budget: budget}
	if c.busy(ctx, conv) {
		c.Reply(c.T("goal.set_later", budget))
		return // the turn in progress picks the goal up when it ends
	}
	if c.submit(ctx, conv, c.jobID("goal_"), goalPrompt(goal)) {
		c.Reply(c.T("goal.set_now", budget))
	}
}

func goalPrompt(goal *state.Goal) string {
	return fmt.Sprintf(`[Standing goal: turn %d of at most %d]
Goal: %s

Keep working toward this goal: take the next concrete steps now rather than describing them, and say briefly what you did. When the goal is fully achieved and you have verified it, end your reply with the line %s. If you cannot go on without the user (a decision, access or information), say exactly what you need and end your reply with the line %s.`,
		goal.Turns+1, goal.Budget, goal.Text, GoalDone, GoalBlocked)
}

// afterTurn runs once a channel turn's reply is queued: guidance that came
// in after the turn's last tool call becomes a follow-up turn, and a session
// with a standing goal is prompted to continue it. Job ids derive from the
// finished turn, so a rerun of the turn queues nothing twice.
func (g *Gateway) afterTurn(ctx context.Context, j *jobs.Job, p agent.TurnPayload, to Address, final string) {
	if g.State == nil || p.Compact {
		return
	}
	turnID := j.ID
	notes, err := g.State.TakeSteers(ctx, p.Session)
	if err != nil {
		g.Log.Warn("read steering failed", "err", err)
	}
	if len(notes) > 0 {
		text := "[Guidance the user sent while you were working, which arrived after your last step. Act on it now.]\n" + strings.Join(notes, "\n\n")
		g.followUp(ctx, "steer_"+turnID, p.Session, to, text)
		return // the goal, if any, continues after that turn
	}
	goal, err := g.State.Goal(ctx, p.Session)
	if err != nil || goal == nil {
		return
	}
	if goal, err = g.State.CountGoalTurn(ctx, p.Session, turnID); err != nil || goal == nil {
		return
	}
	to.ReplyTo = ""
	switch {
	case strings.Contains(final, GoalDone):
		g.endGoal(ctx, turnID, p.Session, to, g.t(to, "goal.reached", goal.Text))
	case strings.Contains(final, GoalBlocked):
		g.endGoal(ctx, turnID, p.Session, to, g.t(to, "goal.paused"))
	case goal.Turns >= goal.Budget:
		g.endGoal(ctx, turnID, p.Session, to, g.t(to, "goal.stopped", goal.Turns, goal.Text))
	default:
		if busy, err := g.Queue.Busy(ctx, p.Session, turnID); err != nil || busy {
			return // another turn is queued; the goal continues after it
		}
		g.followUp(ctx, "goal_"+turnID, p.Session, to, goalPrompt(goal))
	}
}

func (g *Gateway) endGoal(ctx context.Context, turnID, sessionID string, to Address, notice string) {
	if _, err := g.State.ClearGoal(ctx, sessionID); err != nil {
		g.Log.Warn("clear goal failed", "err", err)
	}
	if err := g.enqueueDelivery(ctx, "goal_end_"+turnID, to, notice); err != nil {
		g.Log.Warn("queue goal notice failed", "err", err)
	}
}

func (g *Gateway) followUp(ctx context.Context, id, sessionID string, to Address, text string) {
	origin, _ := json.Marshal(to)
	if _, err := g.Runner.SubmitTurn(ctx, agent.Turn{ID: id, Session: sessionID, Text: text, Origin: origin}); err != nil {
		g.Log.Error("queue follow-up turn failed", "err", err)
		return
	}
	if g.Wake != nil {
		g.Wake()
	}
}

// /export

func (g *Gateway) cmdExport(ctx context.Context, c *Call) {
	conv, err := g.Store.Conversation(ctx, c.To)
	if err != nil || conv == nil {
		c.Reply(c.T("export.nothing"))
		return
	}
	fs, ok := g.Adapters[c.To.Account].(FileSender)
	if !ok || g.Blobs == nil {
		c.Reply(c.T("export.no_files"))
		return
	}
	sess, err := g.Runner.Sessions.Get(ctx, conv.SessionID)
	if err != nil {
		c.Reply(c.T("conv.read_failed"))
		return
	}
	recs, err := g.Runner.Sessions.Messages(ctx, conv.SessionID)
	if err != nil {
		c.Reply(c.T("conv.read_failed"))
		return
	}
	data := []byte(ExportMarkdown(sess, recs, g.now()))
	if int64(len(data)) > min(fs.MaxFileSize(), maxSendFile) {
		c.Reply(c.T("export.too_large", sess.ID))
		return
	}
	f := &outFile{Name: "conversation-" + sess.ID + ".md", MediaType: "text/markdown", Size: int64(len(data))}
	if f.Ref, err = g.Blobs.Put(data); err != nil {
		c.Reply(c.T("export.prepare_failed"))
		return
	}
	to := c.To
	to.ReplyTo = ""
	id := c.jobID("export_")
	if id == "" {
		sum := sha256.Sum256([]byte(sess.ID + "\x00" + f.Ref))
		id = "export_" + hex.EncodeToString(sum[:12])
	}
	if err := g.enqueue(ctx, id, deliverPayload{To: to, Text: c.T("export.caption", len(recs)), File: f}); err != nil {
		c.Reply(c.T("export.queue_failed"))
	}
}

// ExportMarkdown renders a transcript as a Markdown document.
func ExportMarkdown(sess *session.Session, recs []session.Record, now time.Time) string {
	return session.ExportMarkdown(sess, recs, now)
}

// /whoami, /agents, /agent

func (g *Gateway) cmdWhoami(ctx context.Context, c *Call) {
	m := c.Msg
	var b strings.Builder
	who := m.Sender
	if m.SenderName != "" {
		who = m.SenderName + " (" + m.Sender + ")"
	}
	b.WriteString(c.T("whoami.you", who) + "\n")
	if m.SenderAlt != "" {
		b.WriteString(c.T("whoami.alias", m.SenderAlt) + "\n")
	}
	access := c.T("whoami.access_paired")
	for _, id := range c.Channel.AllowFrom {
		id = strings.TrimPrefix(id, "+")
		if id == m.Sender || (m.SenderAlt != "" && id == m.SenderAlt) {
			access = c.T("whoami.access_listed")
			break
		}
		if id == "*" {
			access = c.T("whoami.access_star")
		}
	}
	b.WriteString(c.T("whoami.access", access) + "\n")
	kind := c.T("whoami.group")
	if m.Direct {
		kind = c.T("whoami.direct")
	}
	b.WriteString(c.T("whoami.channel", c.To.Account, c.Channel.Type, kind, c.To.Conversation))
	if c.To.Thread != "" {
		b.WriteString(c.T("whoami.thread", c.To.Thread))
	}
	b.WriteString("\n")
	if conv, err := g.Store.Conversation(ctx, c.To); err == nil && conv != nil {
		b.WriteString(c.T("whoami.agent_session", conv.Agent, conv.SessionID))
	} else {
		b.WriteString(c.T("whoami.agent_nosession", g.currentAgent(ctx, c)))
	}
	c.Reply(b.String())
}

func (g *Gateway) currentAgent(ctx context.Context, c *Call) string {
	if conv, err := g.Store.Conversation(ctx, c.To); err == nil && conv != nil {
		return conv.Agent
	}
	if _, ok := g.Runner.Agents[c.Pick]; ok {
		return c.Pick
	}
	return c.Channel.AgentFor(c.To.Conversation)
}

func (g *Gateway) cmdAgents(ctx context.Context, c *Call) {
	cur := g.currentAgent(ctx, c)
	names := make([]string, 0, len(g.Runner.Agents))
	for n := range g.Runner.Agents {
		names = append(names, n)
	}
	slices.Sort(names)
	var b strings.Builder
	b.WriteString(c.T("agents.header"))
	for _, n := range names {
		mark := ""
		if n == cur {
			mark = c.T("agents.this")
		}
		model := ""
		if spec := g.Runner.Agents[n]; len(spec.Chain) > 0 {
			model = ", " + spec.Chain[0].Endpoint + "/" + spec.Chain[0].Model
		}
		fmt.Fprintf(&b, "\n- %s%s%s", n, model, mark)
	}
	b.WriteString("\n\n" + c.T("agents.switch"))
	c.Reply(b.String())
}

func (g *Gateway) cmdAgent(ctx context.Context, c *Call) {
	name := strings.TrimSpace(c.Args)
	if name == "" {
		g.cmdAgents(ctx, c)
		return
	}
	if !c.needState() {
		return
	}
	if strings.EqualFold(name, "default") {
		name = ""
	} else if _, ok := g.Runner.Agents[name]; !ok {
		c.Reply(c.T("agent.unknown", name))
		return
	}
	if err := g.State.SetConversationAgent(ctx, c.To.key(), name); err != nil {
		c.Reply(c.T("agent.switch_failed"))
		return
	}
	pick := name
	if pick == "" {
		pick = c.Channel.AgentFor(c.To.Conversation)
	}
	cur, _ := g.Store.Conversation(ctx, c.To)
	if cur != nil && cur.Agent == pick {
		c.Reply(c.T("agent.already", pick))
		return
	}
	g.resolveApproval(c.To, false)
	if _, err := g.conversation(ctx, c.Channel, c.To, pick, true); err != nil {
		c.Reply(c.T("agent.start_failed", pick))
		return
	}
	c.Reply(c.T("agent.switched", pick))
}

// Hooks for messages in and out.

func (g *Gateway) messageIn(ctx context.Context, m Message) {
	if g.Runner == nil || g.Runner.Hooks == nil {
		return
	}
	g.Runner.Fire(ctx, "message_in", m.Account, map[string]any{
		"event": "message_in", "account": m.Account, "conversation": m.Conversation, "thread": m.Thread,
		"id": m.ID, "sender": m.Sender, "sender_name": m.SenderName, "text": m.Text, "direct": m.Direct,
		"attachments": len(m.Attachments),
	})
}

func (g *Gateway) messageOut(ctx context.Context, p deliverPayload) {
	if g.Runner == nil || g.Runner.Hooks == nil {
		return
	}
	payload := map[string]any{"event": "message_out", "account": p.To.Account, "conversation": p.To.Conversation,
		"thread": p.To.Thread, "text": p.Text}
	if p.File != nil {
		payload["file"] = p.File.Name
	}
	g.Runner.Fire(ctx, "message_out", p.To.Account, payload)
}
