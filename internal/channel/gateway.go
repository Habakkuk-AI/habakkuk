package channel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/agent/projtrust"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/state"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/ops/i18n"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
	"github.com/habakkuk-ai/habakkuk/internal/tools/media"
)

// KindDeliver is the job kind that sends a reply to a channel.
const KindDeliver = "deliver"

// Gateway routes channel messages to agents and replies back.
//
// Inbound: a message becomes a turn job whose ID is derived from the
// platform's message ID, so a redelivered message never runs twice.
// Outbound: a finished turn enqueues a delivery job; a delivery records each
// chunk it sends, so a retry after a crash resumes instead of repeating.
type Gateway struct {
	Channels        map[string]*config.Channel
	Adapters        map[string]Adapter
	Runner          *agent.Runner
	Queue           *jobs.Queue
	Store           *Store
	Wake            func()
	Log             *slog.Logger
	ApprovalTimeout time.Duration // default 15m
	Now             func() time.Time
	Blobs           *store.Blobs
	// OnChannelState, when set, hears each account's connection state
	// changes (Supervisor.OnState); serve uses it to tell when it is ready.
	OnChannelState func(account, state string)
	// Transcribe turns a voice note into text; nil leaves voice notes
	// untranscribed.
	Transcribe func(ctx context.Context, name, mediaType string, data []byte) (string, error)
	MaxFile    int64 // largest attachment downloaded; default 25 MB
	// StillWorking is how long a turn runs before a chat whose platform has
	// no typing indicator gets one short progress notice; default 60s,
	// negative for never.
	StillWorking time.Duration
	// Speech speaks replies on channels with voice_replies; nil is off.
	Speech Speaker
	// Undo restores a session's workspace to before its latest turn that
	// changed it and says what changed; nil disables /undo.
	Undo func(ctx context.Context, sessionID string) (string, error)
	// State keeps per-session settings, goals, steering notes, task lists
	// and progress drafts (state.db); nil turns those features off.
	State *state.Store
	// Personas are the personas /personality offers.
	Personas state.Personalities
	// ProgressAfter is how long a turn runs before its progress draft is
	// posted (default 8s); ProgressEvery is the least time between edits
	// (default 3s). StreamEvery is the least time between edits of a reply
	// streamed while it is written (default 1.2s).
	ProgressAfter, ProgressEvery, StreamEvery time.Duration
	// Commands are extra slash commands ("/usage", "/model"), each given
	// the conversation's session and the text after the command. The reply
	// is sent back to the chat.
	Commands map[string]AppCommand
	// AgentCommand, when set, finds a slash command that depends on the
	// agent a chat goes to (each of its skills is one: "/<skill-name>"). It
	// is asked last, after the built-in commands and Commands, so it never
	// takes a command's place.
	AgentCommand func(agent, name string) (AppCommand, bool)
	// Memory backs /remember and /forget; nil turns them off.
	Memory MemoryCommands
	// Language is the language of Habakkuk's own chat text (config
	// language:); a channel's language overrides it. Empty is English.
	Language string
	// Redact removes secrets from text a chat is shown word for word (the
	// exact call of an approval); nil removes well-known token formats.
	Redact func(string) string
	// SaveDisplay writes a channel's progress and approvals_detail to the
	// config file ("" leaves one as it is) and returns the channel as the
	// file now has it; nil means the settings cannot be changed from here
	// (display.go).
	SaveDisplay func(account, progress, approvalsDetail string) (*config.Channel, error)

	display   sync.Map // account -> Display, once changed while running (display.go)
	pending   sync.Map // conversation key -> *pendingApproval, while an approval waits
	asked     sync.Map // conversation key -> Approval, while an approval waits
	questions sync.Map // conversation key -> chan string, while clarify waits
	approveMu sync.Map // conversation key -> chan struct{} (holdChat), one prompt at a time
	throttle  sync.Map // key -> time.Time of the last pairing notice
	nudged    sync.Map // turn job id -> true once its progress notice went out

	buttons sync.Map // prompt nonce -> buttonHandler, while its buttons wait (rich.go)
}

type deliverPayload struct {
	To   Address  `json:"to"`
	Text string   `json:"text"`           // the reply, or the file's caption
	File *outFile `json:"file,omitempty"` // set when the delivery is a file
	// Draft is the turn whose progress draft goes before this delivery.
	Draft string `json:"draft,omitempty"`
	// Who sends it, for chat notes (notes.go): the session whose turn
	// wrote it, the agent, and what sent it. A delivery from the chat's
	// own session leaves no note.
	Session string `json:"session,omitempty"`
	Agent   string `json:"agent,omitempty"`
	What    string `json:"what,omitempty"`
	// Tainted: outside content may have steered the turn that wrote it
	// (agent.Runner.Untrusted); its chat note says so.
	Tainted bool `json:"tainted,omitempty"`
}

func (g *Gateway) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Run supervises every adapter until ctx ends.
func (g *Gateway) Run(ctx context.Context) {
	sup := &Supervisor{Store: g.Store, Log: g.Log, OnState: g.OnChannelState}
	sup.defaults() // once, here: the adapters' goroutines share it and only read
	var wg sync.WaitGroup
	for name, a := range g.Adapters {
		wg.Add(1)
		go func() { defer wg.Done(); sup.Run(ctx, name, a, g.Handle) }()
	}
	wg.Wait()
}

// Handle processes one inbound message.
func (g *Gateway) Handle(ctx context.Context, m Message) {
	ch := g.Channels[m.Account]
	if ch == nil {
		return
	}
	to := Address{Account: m.Account, Conversation: m.Conversation, Thread: m.Thread, ReplyTo: m.ID}
	log := g.Log.With("channel", m.Account, "conversation", m.Conversation, "sender", m.Sender)
	if m.Callback != "" {
		g.callback(ctx, ch, m, to) // a button press
		return
	}

	ok, err := g.allowed(ctx, ch, m)
	if err != nil {
		log.Error("allowlist check failed", "err", err)
		return
	}
	if !ok {
		if m.Direct {
			g.offerPairing(ctx, m, to)
		} else {
			log.Debug("ignored message from unknown sender in a group")
		}
		return
	}
	if !m.Direct && (ch.Groups == "off" || (ch.Groups == "mention" && !m.Mentioned)) {
		return
	}
	text := strings.TrimSpace(m.Text)
	if text == "" && len(m.Attachments) == 0 {
		return
	}
	g.messageIn(ctx, m)
	owner := g.owner(ctx, ch, m)
	// Only the owner answers approvals: a guest in a group must not approve
	// the owner's pending command.
	if text != "" && (g.answerQuestion(to, text) || (owner && g.answerApproval(to, text, answerer(m)))) {
		return
	}
	pick := g.pickAgent(ctx, to, m.Agent)
	if strings.HasPrefix(text, "/") && g.command(withMessage(ctx, m), ch, to, pick, text) {
		return
	}
	conv, err := g.conversation(withMessage(ctx, m), ch, to, pick, false)
	if err != nil {
		log.Error("open conversation failed", "err", err)
		return
	}
	if !m.Direct && m.SenderName != "" {
		text = m.SenderName + ": " + text
	}
	if q := quoteLine(m.Quote); q != "" {
		text = strings.TrimSpace(q + "\n" + text)
	}
	media := g.ingest(ctx, conv.Agent, m.Attachments)
	from := to
	from.Trust = TrustGuest
	if owner {
		from.Trust = TrustOwner
	}
	origin, _ := json.Marshal(from)
	if _, err := g.Runner.SubmitTurn(ctx, agent.Turn{
		ID: TurnID(m), Session: conv.SessionID, Text: text, Media: media, Origin: origin, Typed: g.typed(m, owner),
	}); err != nil {
		log.Error("queue turn failed", "err", err)
		g.reply(to, g.t(to, "turn.queue_failed"))
		return
	}
	if err := g.Store.Touch(ctx, to); err != nil {
		log.Warn("touch conversation failed", "err", err)
	}
	if g.Wake != nil {
		g.Wake()
	}
}

// maxQuote is how much of a quoted message the agent sees.
const maxQuote = 500

// quoteLine tells the agent which message a reply quotes, or "".
func quoteLine(q *Quote) string {
	if q == nil {
		return ""
	}
	text := shorten(q.Text, maxQuote)
	if text == "" {
		return ""
	}
	who := strings.TrimSpace(q.From)
	switch {
	case q.Self:
		who = "you"
	case who == "":
		who = "an earlier message"
	}
	return fmt.Sprintf("[Replying to %s: \"%s\"]", who, text)
}

// ingest downloads a message's attachments into the blob store, saves a copy
// in the agent's workspace inbox, and transcribes voice notes. A file that
// cannot be fetched becomes a note, so the agent can tell the user.
func (g *Gateway) ingest(ctx context.Context, agentName string, atts []Attachment) []agent.Attachment {
	if len(atts) == 0 {
		return nil
	}
	limit := g.MaxFile
	if limit == 0 {
		limit = 25 << 20
	}
	inbox := ""
	if spec, ok := g.Runner.Agents[agentName]; ok {
		inbox = filepath.Join(spec.Workspace, "inbox")
	}
	var out []agent.Attachment
	for _, a := range atts {
		name := media.SafeName(a.Name)
		got := agent.Attachment{Name: name, MediaType: a.MediaType, Size: a.Size}
		if a.Size > limit {
			got.Note = fmt.Sprintf("(not downloaded: larger than %d MB)", limit>>20)
			out = append(out, got)
			continue
		}
		fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		data, err := a.Fetch(fctx)
		cancel()
		if err == nil && int64(len(data)) > limit {
			err = fmt.Errorf("larger than %d MB", limit>>20)
		}
		if err != nil {
			g.Log.Warn("attachment download failed", "name", name, "err", err)
			got.Note = "(could not be downloaded: " + shorten(err.Error(), 200) + ")"
			out = append(out, got)
			continue
		}
		got.Size = int64(len(data))
		got.MediaType = media.Detect(name, a.MediaType, data)
		if g.Blobs != nil {
			if got.Ref, err = g.Blobs.Put(data); err != nil {
				g.Log.Warn("store attachment failed", "err", err)
			}
		}
		if inbox != "" {
			// Saved only where the inbox really is inside the workspace: a
			// link named inbox must not lead an attachment elsewhere.
			ws := &tools.Env{Workspace: filepath.Dir(inbox), Policy: &tools.Policy{Locked: lockedFolders(g.Runner, agentName)}}
			if err := os.MkdirAll(inbox, 0o700); err == nil {
				if dir, err := ws.OutputDir(inbox); err == nil {
					path := filepath.Join(dir, g.now().UTC().Format("20060102-150405")+"-"+name)
					if store.WriteFileAtomic(path, data, 0o600) == nil {
						got.Path = path
					}
				} else {
					g.Log.Warn("attachment not saved in the workspace", "err", err)
				}
			}
		}
		if strings.HasPrefix(got.MediaType, "audio/") {
			got.Note = g.transcribe(ctx, name, got.MediaType, data)
		}
		out = append(out, got)
	}
	return out
}

func (g *Gateway) transcribe(ctx context.Context, name, mediaType string, data []byte) string {
	if g.Transcribe == nil {
		return "(voice note; transcription is not configured, so ask the user to type it)"
	}
	tctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	text, err := g.Transcribe(tctx, name, mediaType, data)
	if err != nil {
		g.Log.Warn("transcription failed", "err", err)
		return "(voice note; transcription failed: " + shorten(err.Error(), 200) + ")"
	}
	return "Voice note transcript: " + text
}

// TurnID is the ID of the turn job a message starts, derived from the
// platform message ID so a redelivery maps to the same job.
func TurnID(m Message) string {
	sum := sha256.Sum256([]byte(m.Account + "\x00" + m.Conversation + "\x00" + m.ID))
	return "turn_" + hex.EncodeToString(sum[:12])
}

// Allowed reports whether a sender may talk on an account: listed in
// allow_from or paired. Adapters use it to accept invitations (Matrix).
func (g *Gateway) Allowed(ctx context.Context, account, sender string) bool {
	ch := g.Channels[account]
	if ch == nil {
		return false
	}
	ok, err := g.allowed(ctx, ch, Message{Account: account, Sender: sender})
	return err == nil && ok
}

func (g *Gateway) allowed(ctx context.Context, ch *config.Channel, m Message) (bool, error) {
	for _, id := range ch.AllowFrom {
		id = strings.TrimPrefix(id, "+") // phone numbers may be written +1...
		if id == "*" || id == m.Sender || (m.SenderAlt != "" && id == m.SenderAlt) {
			return true, nil
		}
	}
	return g.Store.Allowed(ctx, m.Account, m.Sender, m.SenderAlt)
}

// owner reports whether a sender is someone the owner vouched for: listed
// by id in allow_from, or paired. allow_from "*" lets anyone talk, but such
// senders are guests: their turns never get the shell, the computer or
// writes outside the workspace. The web chat, behind the API token, is
// the owner's own.
func (g *Gateway) owner(ctx context.Context, ch *config.Channel, m Message) bool {
	if !ch.MessagingChannel() {
		return true
	}
	// A WhatsApp agent only ever hears from the person who created it: the
	// platform refuses everyone else, so whoever writes is the owner.
	if ch.OwnerOnly() {
		return true
	}
	for _, id := range ch.AllowFrom {
		id = strings.TrimPrefix(id, "+")
		if id != "*" && (id == m.Sender || (m.SenderAlt != "" && id == m.SenderAlt)) {
			return true
		}
	}
	ok, err := g.Store.Allowed(ctx, m.Account, m.Sender, m.SenderAlt)
	return err == nil && ok
}

// offerPairing answers an unknown sender with a code the owner can approve,
// at most once every ten minutes per sender.
func (g *Gateway) offerPairing(ctx context.Context, m Message, to Address) {
	key := m.Account + "|" + m.Sender
	if last, ok := g.throttle.Load(key); ok && g.now().Sub(last.(time.Time)) < 10*time.Minute {
		return
	}
	g.throttle.Store(key, g.now())
	sender := m.Sender
	if m.SenderAlt != "" {
		sender = m.SenderAlt
	}
	code, _, err := g.Store.PairCode(ctx, m.Account, sender, m.SenderName)
	if err != nil {
		g.Log.Error("pairing code failed", "err", err)
		return
	}
	g.Log.Info("pairing code issued", "channel", m.Account, "sender", sender, "name", m.SenderName)
	g.reply(to, g.t(to, "pair.offer", code))
}

// conversation returns the session bound to a conversation, starting a new
// one when there is none, the routed agent changed, the conversation was
// idle too long, or fresh is set. pick, when it names a configured agent,
// overrides routing.
func (g *Gateway) conversation(ctx context.Context, ch *config.Channel, to Address, pick string, fresh bool) (*Conversation, error) {
	conv, err := g.Store.Conversation(ctx, to)
	if err != nil {
		return nil, err
	}
	agentName := ch.AgentFor(to.Conversation)
	if _, ok := g.Runner.Agents[pick]; ok {
		agentName = pick
	}
	if conv != nil && !fresh && conv.Agent == agentName &&
		(ch.SessionIdle == 0 || g.now().Sub(conv.LastAt) < ch.SessionIdle) {
		return conv, nil
	}
	// A one-to-one chat may be the agent's first meeting with its user,
	// which starts with the welcome.
	m, _ := ctx.Value(messageKey{}).(Message)
	sess, err := g.Runner.NewSessionWith(ctx, agentName, agent.SessionOptions{Chat: m.Direct})
	if err != nil {
		return nil, err
	}
	if err := g.Store.Bind(ctx, to, agentName, sess.ID); err != nil {
		return nil, err
	}
	return &Conversation{Address: to, Agent: agentName, SessionID: sess.ID, LastAt: g.now()}, nil
}

// command handles a slash command. It returns false for commands it does not
// know, which go to the agent as ordinary text.
func (g *Gateway) command(ctx context.Context, ch *config.Channel, to Address, pick, text string) bool {
	name, _, _ := strings.Cut(strings.Fields(text)[0], "@") // Telegram groups append @botname
	switch strings.ToLower(name) {
	case "/new", "/reset":
		g.resolveApproval(to, false)
		g.forgetAsked(to)
		if _, err := g.conversation(ctx, ch, to, pick, true); err != nil {
			g.Log.Error("new session failed", "err", err)
			g.reply(to, g.t(to, "new.failed"))
			return true
		}
		g.reply(to, g.t(to, "new.done"))
	case "/stop":
		g.resolveApproval(to, false)
		g.forgetAsked(to)
		conv, err := g.Store.Conversation(ctx, to)
		if err != nil || conv == nil {
			g.reply(to, g.t(to, "stop.idle"))
			return true
		}
		n, err := g.Queue.CancelSerial(ctx, conv.SessionID)
		if err != nil {
			g.Log.Error("cancel failed", "err", err)
		}
		if n == 0 {
			g.reply(to, g.t(to, "stop.idle"))
		} else {
			g.reply(to, i18n.N(g.lang(to), "stop.done", int(n)))
		}
	case "/status":
		g.reply(to, g.status(ctx, ch, to))
	case "/compact":
		// A turn job, so it waits for any running turn in this session and
		// its result is delivered like a reply.
		conv, err := g.Store.Conversation(ctx, to)
		if err != nil || conv == nil {
			g.reply(to, g.t(to, "compact.nothing"))
			return true
		}
		origin, _ := json.Marshal(to)
		if _, err := g.Runner.SubmitTurn(ctx, agent.Turn{Session: conv.SessionID, Compact: true, Origin: origin}); err != nil {
			g.Log.Error("queue compaction failed", "err", err)
			g.reply(to, g.t(to, "compact.failed"))
			return true
		}
		if g.Wake != nil {
			g.Wake()
		}
	case "/undo":
		g.reply(to, g.undo(ctx, to))
	case "/help", "/start":
		// A chat app's first-contact /start goes to an agent that has not met
		// its user yet, which answers with its welcome instead of the list.
		if strings.ToLower(name) == "/start" {
			agentName := ch.AgentFor(to.Conversation)
			if _, ok := g.Runner.Agents[pick]; ok {
				agentName = pick
			}
			if spec, ok := g.Runner.Agents[agentName]; ok && agent.NeedsOnboarding(spec.Dir) {
				return false
			}
		}
		g.reply(to, builtinHelp(g.lang(to))+g.commandHelp(g.lang(to))+g.extraHelp())
	default:
		if cmd, ok := g.appCommand(ch, to, pick, strings.ToLower(name)); ok {
			conv, err := g.conversation(ctx, ch, to, pick, false)
			if err != nil {
				g.Log.Error("open conversation failed", "err", err)
				g.reply(to, g.t(to, "conv.open_failed"))
				return true
			}
			_, arg, _ := strings.Cut(strings.TrimSpace(text), " ")
			arg = strings.TrimSpace(arg)
			m, _ := ctx.Value(messageKey{}).(Message)
			if cmd.OwnerOnly && m.Account != "" && !g.owner(ctx, ch, m) {
				g.reply(to, strings.ToLower(name)+": only the owner can do that here.")
				return true
			}
			switch {
			case cmd.Prompt != nil:
				// A command that becomes a message: the text it builds runs as
				// a turn of this conversation, so its reply arrives as usual.
				msg, err := cmd.Prompt(ctx, conv.SessionID, arg)
				if err != nil {
					g.reply(to, err.Error())
					return true
				}
				c := &Call{G: g, Channel: ch, To: to, Pick: pick, Args: arg, Msg: m}
				c.submit(ctx, conv, c.jobID("cmd_"), msg)
			case cmd.Async:
				go g.reply(to, cmd.Run(context.WithoutCancel(ctx), conv.SessionID, arg))
			default:
				g.reply(to, cmd.Run(ctx, conv.SessionID, arg))
			}
			return true
		}
		return g.runCommand(ctx, ch, to, pick, text)
	}
	return true
}

// appCommand finds a command the app added: one of Commands, else, for a
// name that is not one of the gateway's own, one that depends on the agent
// this chat goes to (AgentCommand).
func (g *Gateway) appCommand(ch *config.Channel, to Address, pick, name string) (AppCommand, bool) {
	if cmd, ok := g.Commands[name]; ok {
		return cmd, true
	}
	if g.AgentCommand == nil || Reserved(name) {
		return AppCommand{}, false
	}
	agentName := ch.AgentFor(to.Conversation)
	if _, ok := g.Runner.Agents[pick]; ok {
		agentName = pick
	}
	return g.AgentCommand(agentName, name)
}

// AppCommand is a chat command the app adds to the gateway.
type AppCommand struct {
	Help string // one line for /help, e.g. "/usage: tokens and cost of this conversation"
	Run  func(ctx context.Context, session, arg string) string
	// Prompt, set instead of Run, makes the command a message: the text it
	// returns runs as a turn of the conversation (an error is replied).
	Prompt func(ctx context.Context, session, arg string) (string, error)
	// Async runs Run in the background, for commands that may take a while.
	Async bool
	// OwnerOnly refuses the command to guests (senders let in by
	// allow_from "*"), like /remember.
	OwnerOnly bool
}

// Reserved reports whether a slash command name (with the slash) is one of
// the gateway's own, which app commands cannot replace.
func Reserved(name string) bool {
	switch name {
	case "/new", "/reset", "/stop", "/status", "/compact", "/undo", "/help", "/start":
		return true
	}
	for _, c := range (&Gateway{}).commands() {
		if c.Name == name || slices.Contains(c.Aliases, name) {
			return true
		}
	}
	return false
}

func (g *Gateway) extraHelp() string {
	names := make([]string, 0, len(g.Commands))
	for n := range g.Commands {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		if h := g.Commands[n].Help; h != "" {
			b.WriteString("\n" + h)
		}
	}
	return b.String()
}

func (g *Gateway) status(ctx context.Context, ch *config.Channel, to Address) string {
	var b strings.Builder
	agentName := ch.AgentFor(to.Conversation)
	b.WriteString(g.t(to, "status.agent", agentName) + "\n")
	if conv, err := g.Store.Conversation(ctx, to); err == nil && conv != nil {
		b.WriteString(g.t(to, "status.session", conv.SessionID, g.now().Sub(conv.LastAt).Round(time.Second)) + "\n")
	}
	if spec, ok := g.Runner.Agents[agentName]; ok && len(spec.Chain) > 0 {
		models := make([]string, len(spec.Chain))
		for i, t := range spec.Chain {
			models[i] = t.Endpoint + "/" + t.Model
		}
		b.WriteString(g.t(to, "status.models", strings.Join(models, " → ")) + "\n")
	}
	sts, _ := g.Store.Statuses(ctx)
	for _, st := range sts {
		fmt.Fprintf(&b, "%s: %s", st.Account, st.State)
		if st.State != "connected" && st.LastError != "" {
			fmt.Fprintf(&b, " (%s)", st.LastError)
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// reply sends a short notice outside the job system. It runs in the
// background so the adapter's receive loop never waits on a send.
func (g *Gateway) reply(to Address, markdown string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := g.send(ctx, to, markdown); err != nil {
			g.Log.Warn("reply failed", "channel", to.Account, "err", err)
		}
	}()
}

func (g *Gateway) send(ctx context.Context, to Address, markdown string) error {
	a := g.Adapters[to.Account]
	if a == nil {
		return errNoAdapter
	}
	for i, chunk := range Split(markdown, chunkLimit(a)) {
		replyTo := ""
		if i == 0 {
			replyTo = to.ReplyTo
		}
		if err := a.Send(ctx, to, render(a, chunk), replyTo); err != nil {
			return err
		}
	}
	return nil
}

// chunkLimit leaves room for markup the formatter adds.
func chunkLimit(a Adapter) int { return a.MaxLen() * 85 / 100 }

func render(a Adapter, chunk string) string {
	if out := a.Format(chunk); utf8.RuneCountInString(out) <= a.MaxLen() {
		return out
	}
	return chunk
}

// HandleTurn wraps the runner's turn handler: it shows progress while the
// turn runs, gives the turn's tools its reply address (send_file), and
// queues the reply when it finishes.
func (g *Gateway) HandleTurn(ctx context.Context, j *jobs.Job, owner string) (string, error) {
	var p agent.TurnPayload
	if err := j.Decode(&p); err != nil {
		return "", jobs.Permanent{Err: err}
	}
	var to *Address
	if len(p.Origin) > 0 {
		to = new(Address)
		if err := json.Unmarshal(p.Origin, to); err != nil {
			to = nil
		}
	}
	// What this turn sends (its reply, files, home posts) is the session's
	// own; another chat it lands in gets a note of it (notes.go).
	ctx = agent.WithPoster(ctx, agent.Poster{Agent: j.Agent, What: p.Task, Session: p.Session})
	if to != nil {
		ctx = context.WithValue(ctx, originKey{}, *to)
		stop := g.keepAlive(ctx, j.ID, *to)
		defer stop()
		g.react(ctx, *to, ReactWorking)
	}
	endProgress := func() {}
	if to != nil {
		endProgress = g.startProgress(ctx, j.ID, p.Session, *to)
		defer endProgress()
	}
	final, err := g.Runner.HandleTurn(ctx, j, owner)
	g.reactEnd(ctx, j, to, err)
	endProgress() // no draft edit may follow the reply
	if err == nil || jobs.IsPermanent(err) || j.Attempts >= j.MaxAttempts {
		g.nudged.Delete(j.ID) // the turn will not run again
		if to != nil {
			g.forgetAsked(*to) // nor ask again: a later "yes" is an ordinary message
		}
	}
	if to == nil {
		return final, err
	}
	if err != nil {
		if errors.Is(err, jobs.ErrLeaseLost) || ctx.Err() != nil {
			g.settleStopped(ctx, j, *to)
			return "", err
		}
		if jobs.IsPermanent(err) || j.Attempts >= j.MaxAttempts {
			// No model could take the call (credit or keys): background work
			// tells each chat once per outage, not on every run.
			if ue := new(llm.UnavailableError); errors.As(err, &ue) && p.Background &&
				!g.Runner.UnavailableNotice(ue, to.Account+"\x00"+to.Conversation+"\x00"+to.Thread) {
				g.Log.Info("model outage already reported; failure not sent", "job", j.ID, "outage", ue.Key)
				g.clearDraft(ctx, j.ID, *to)
				return "", err
			}
			msg := g.t(*to, "turn.failed", shorten(err.Error(), 300))
			if stopped := (agent.LoopStopped{}); errors.As(err, &stopped) {
				msg = stopped.Message // the loop guard's own message, whole
			}
			if derr := g.enqueue(ctx, "dlv_"+j.ID, deliverPayload{To: *to, Text: msg, Draft: j.ID}); derr != nil {
				g.Log.Error("queue failure notice failed", "err", derr)
			}
		}
		return "", err
	}
	switch strings.TrimSpace(final) {
	case "":
		final = g.t(*to, "turn.done")
	case Silent:
		g.clearDraft(ctx, j.ID, *to)
		g.afterTurn(ctx, j, p, *to, final)
		return final, nil // the agent judged there was nothing worth sending
	}
	if err := g.deliverReply(ctx, j.ID, p.Media, *to, final, j.ID); err != nil {
		return "", err // the turn reruns, finds itself finished, and queues again
	}
	g.afterTurn(ctx, j, p, *to, final)
	return final, nil
}

// Silent, as a turn's whole reply, means nothing is sent to the chat.
// Scheduled runs use it to stay quiet when there is nothing to report.
const Silent = "[SILENT]"

func (g *Gateway) enqueueDelivery(ctx context.Context, id string, to Address, text string) error {
	return g.enqueue(ctx, id, deliverPayload{To: to, Text: text})
}

// enqueue queues a delivery. Deliveries to one conversation share a serial
// key, so a file sent during a turn goes out before the turn's reply.
func (g *Gateway) enqueue(ctx context.Context, id string, p deliverPayload) error {
	p.poster(ctx)
	if g.Runner != nil {
		turn := llm.CallerFrom(ctx).Turn
		if turn == "" {
			turn = p.Draft // a turn's reply, queued after it ended
		}
		p.Tainted = p.Tainted || (turn != "" && g.Runner.Untrusted(turn))
	}
	p.Tainted = p.Tainted || agent.TaintedFrom(ctx)
	_, err := g.Queue.Enqueue(ctx, jobs.NewJob{
		ID: id, Kind: KindDeliver, SerialKey: "dlv:" + p.To.key(), Priority: 20, MaxAttempts: 20,
		Payload: p,
	})
	if err == nil && g.Wake != nil {
		g.Wake()
	}
	return err
}

// Deliver queues text for a stored reply address (a JSON Address, as
// schedules and board cards keep it). The job ID makes a repeated call send
// once.
func (g *Gateway) Deliver(ctx context.Context, id string, to json.RawMessage, text string) error {
	var addr Address
	if err := json.Unmarshal(to, &addr); err != nil {
		return fmt.Errorf("reply address: %w", err)
	}
	return g.enqueueDelivery(ctx, id, addr, text)
}

// HandleDeliver is the jobs.Handler for KindDeliver.
func (g *Gateway) HandleDeliver(ctx context.Context, j *jobs.Job, owner string) (string, error) {
	var p deliverPayload
	if err := j.Decode(&p); err != nil {
		return "", jobs.Permanent{Err: err}
	}
	a := g.Adapters[p.To.Account]
	if a == nil {
		return "", jobs.Permanent{Err: fmt.Errorf("%w %q", errNoAdapter, p.To.Account)}
	}
	if p.File != nil || strings.TrimSpace(p.Text) == "" {
		g.finishDraft(ctx, p.Draft, a, p.To)
		if p.File != nil {
			return g.deliverFile(ctx, j, owner, a, p)
		}
		return "removed the progress draft", nil
	}
	chunks := Split(p.Text, chunkLimit(a))
	sent, err := g.Store.Sent(ctx, j.ID)
	if err != nil {
		return "", err
	}
	if sent == 0 && g.adoptDraft(ctx, j.ID, p.Draft, a, p.To, chunks[0]) {
		sent = 1
	}
	g.finishDraft(ctx, p.Draft, a, p.To)
	for i := sent; i < len(chunks); i++ {
		if err := g.Queue.Owns(ctx, j.ID, owner); err != nil {
			return "", err
		}
		replyTo := ""
		if i == 0 {
			replyTo = p.To.ReplyTo
		}
		if err := a.Send(WithSendKey(ctx, fmt.Sprintf("%s.%d", j.ID, i)), p.To, render(a, chunks[i]), replyTo); err != nil {
			if errors.As(err, new(Undeliverable)) {
				return "", jobs.Permanent{Err: err}
			}
			return "", err
		}
		if err := g.Store.MarkSent(ctx, j.ID, i+1); err != nil {
			return "", err
		}
	}
	if err := g.Store.ForgetDelivery(ctx, j.ID); err != nil {
		g.Log.Warn("clear delivery progress failed", "err", err)
	}
	g.recordNote(ctx, j.ID, p)
	g.messageOut(ctx, p)
	return fmt.Sprintf("sent %d chunk(s)", len(chunks)), nil
}

const stillWorkingText = "Still working on it. I'll reply here when I'm done."

// keepAlive shows that a turn is in progress until the returned func is
// called. Where the platform has a typing indicator it is refreshed for the
// whole turn (platforms expire it after a few seconds); elsewhere a turn
// that runs past StillWorking gets one short notice.
func (g *Gateway) keepAlive(ctx context.Context, jobID string, to Address) func() {
	ctx, cancel := context.WithCancel(ctx)
	typer, ok := g.Adapters[to.Account].(Typer)
	if !ok {
		go g.stillWorking(ctx, jobID, to)
		return cancel
	}
	go func() {
		t := time.NewTicker(4 * time.Second)
		defer t.Stop()
		for {
			if err := typer.Typing(ctx, to); err != nil && ctx.Err() == nil {
				g.Log.Debug("typing indicator failed", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return cancel
}

// stillWorking sends the progress notice once per turn, even across
// retries, and not while an approval prompt is waiting in the chat.
func (g *Gateway) stillWorking(ctx context.Context, jobID string, to Address) {
	after := g.StillWorking
	if after == 0 {
		after = time.Minute
	}
	if after < 0 || g.drafts(to) { // a progress draft shows it instead
		return
	}
	t := time.NewTimer(after)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-t.C:
	}
	if _, waiting := g.pending.Load(to.key()); waiting {
		return
	}
	if _, sent := g.nudged.LoadOrStore(jobID, true); sent {
		return
	}
	to.ReplyTo = ""
	if err := g.send(ctx, to, g.t(to, "turn.still_working")); err != nil && ctx.Err() == nil {
		g.Log.Debug("progress notice failed", "err", err)
	}
}

// ApproverFor gives a channel session an approver that asks in the chat.
func (g *Gateway) ApproverFor(sessionID string) tools.Approver {
	return &chatApprover{g: g, session: sessionID}
}

type chatApprover struct {
	g       *Gateway
	session string
}

// pendingApproval is one approval prompt waiting in a chat.
type pendingApproval struct {
	answer chan tools.Answer
	once   sync.Once
	hash   string // callHash of the call, carried by the prompt's buttons
	req    tools.ApprovalRequest
	always bool // the prompt offers "Always allow" (req.Rule is set)
}

// resolve answers the approval; only the first answer counts. by names who
// answered.
func (p *pendingApproval) resolve(d tools.Decision, by string) (first bool) {
	p.once.Do(func() { p.answer <- tools.Answer{Decision: d, By: by}; first = true })
	return first
}

// answerer names the sender of m for a saved rule: "7 (Sam)".
func answerer(m Message) string {
	if m.SenderName == "" || m.SenderName == m.Sender {
		return m.Sender
	}
	return m.Sender + " (" + m.SenderName + ")"
}

// holdChat waits, up to the approval timeout, for the chat to have no
// other prompt waiting: one question or approval at a time. ok is false
// when it stays busy or ctx ends, so a worker is never held for good.
func (g *Gateway) holdChat(ctx context.Context, to Address) (release func(), ok bool) {
	v, _ := g.approveMu.LoadOrStore(to.key(), make(chan struct{}, 1))
	sem := v.(chan struct{})
	timeout := g.ApprovalTimeout
	if timeout == 0 {
		timeout = 15 * time.Minute
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	case <-ctx.Done():
	case <-t.C:
	}
	return nil, false
}

func (c *chatApprover) Approve(ctx context.Context, req tools.ApprovalRequest) (bool, error) {
	req.Rule = nil // a plain yes or no: Always allow is offered only through Decide
	a, err := c.Decide(ctx, req)
	return a.Decision != tools.DecisionDeny, err
}

// Decide asks in the chat: Allow once, Always allow (when the request
// carries a rule) or Deny. A scheduled run has no conversation of its own;
// it asks in the chat it delivers to.
func (c *chatApprover) Decide(ctx context.Context, req tools.ApprovalRequest) (tools.Answer, error) {
	g := c.g
	deny := tools.Answer{Decision: tools.DecisionDeny}
	to, agentName, ok, err := c.where(ctx, req)
	if err != nil {
		return deny, err
	}
	if !ok {
		return deny, tools.ErrNoApprover // not a channel session: nobody to ask
	}
	if req.Rule != nil && !g.canOfferRule(to, req) {
		req.Rule = nil
	}
	release, ok := g.holdChat(ctx, to)
	if !ok {
		if ctx.Err() != nil {
			return deny, ctx.Err()
		}
		g.Log.Warn("approval not asked: another request has been waiting in the chat too long", "channel", to.Account, "tool", req.Tool)
		return deny, nil
	}
	defer release()

	p := &pendingApproval{answer: make(chan tools.Answer, 1), hash: callHash(agentName, c.session, req), req: req, always: req.Rule != nil}
	if p.always {
		// A press from a prompt laid out without Always allow (before a
		// restart) must not be read against this one's buttons.
		p.hash += "a"
	}
	g.pending.Store(to.key(), p)
	defer g.pending.Delete(to.key())
	defer g.recordApproval(to, c.session, req.Tool, req.Summary, req.Review)()

	timeout := g.ApprovalTimeout
	if timeout == 0 {
		timeout = 15 * time.Minute
	}
	rows := g.approvalButtons(to, p.hash, p.always)
	if err := g.sendChoice(ctx, to, g.approvalPrompt(ctx, to, req), rows); err != nil {
		return deny, nil // could not ask, so do not allow
	}
	g.rememberAsked(to, p.hash)
	select {
	case a := <-p.answer:
		g.forgetAsked(to)
		return a, nil
	case <-ctx.Done():
		// The turn stopped with the prompt still in the chat: a restart, or
		// a lost lease. The record stays, so an answer to that prompt is
		// explained rather than dropped; the resumed turn asks again.
		return deny, ctx.Err()
	case <-time.After(timeout):
		g.forgetAsked(to)
		g.reply(to, g.t(to, "approval.timeout"))
		return deny, nil
	}
}

// canOfferRule reports whether a chat may be offered "Always allow" for
// req: never in an email conversation (anyone can write one, and a reply
// can be forged), and never when the rule's text would not show as it is
// (the log redactor would hide part of it).
func (g *Gateway) canOfferRule(to Address, req tools.ApprovalRequest) bool {
	if ch := g.Channels[to.Account]; ch != nil && ch.Type == "email" {
		return false
	}
	text := req.Rule.Exact + req.Rule.Path + req.Rule.In
	return g.redact(text) == text
}

// where is the chat to ask in: the session's own conversation, or, for a
// scheduled run, the chat the schedule delivers to. ok is false when there
// is none, and the call is decided as in any unattended run.
func (c *chatApprover) where(ctx context.Context, req tools.ApprovalRequest) (to Address, agentName string, ok bool, err error) {
	conv, err := c.g.Store.BySession(ctx, c.session)
	if err != nil {
		return Address{}, "", false, err
	}
	if conv != nil {
		return conv.Address, conv.Agent, true, nil
	}
	info := agent.TurnInfoFrom(ctx)
	if info.AskHome != "" && (info.Delegated || info.Source == agent.SourceBoard) {
		// A delegated task or a board card: nobody watches it where it
		// started, so it asks in the owner's home chat (agent.askHome).
		home, err := c.g.home(info.AskHome)
		if err != nil {
			c.g.Log.Warn("approval not asked: home chat unavailable", "agent", req.Agent, "home", info.AskHome, "err", err)
			return Address{}, "", false, nil
		}
		home.ReplyTo = ""
		return home, req.Agent, true, nil
	}
	if info.Source != agent.SourceSchedule {
		return Address{}, "", false, nil
	}
	origin := agent.OriginFrom(ctx)
	if len(origin) == 0 || json.Unmarshal(origin, &to) != nil || to.Account == "" || c.g.Adapters[to.Account] == nil {
		return Address{}, "", false, nil
	}
	to.ReplyTo = ""
	return to, req.Agent, true, nil
}

// approvalPrompt is what a chat is asked: what the agent wants to do in
// everyday words, why it asks when there is a reason beyond the permission
// mode, and how to answer. The exact call is shown only on channels with
// approvals_detail: full; elsewhere the prompt says how to see it.
func (g *Gateway) approvalPrompt(ctx context.Context, to Address, req tools.ApprovalRequest) string {
	lang := g.lang(to)
	var b strings.Builder
	b.WriteString(g.describeCall(lang, req))
	if why := g.why(lang, req); why != "" {
		b.WriteString("\n" + why)
	}
	info := agent.TurnInfoFrom(ctx)
	if info.Delegated || info.Source == agent.SourceBoard {
		// Asked in a home chat: say which agent wants it, and who handed
		// it the work.
		if info.From != "" {
			b.WriteString("\n" + i18n.T(lang, "approval.by_agent_from", req.Agent, info.From))
		} else {
			b.WriteString("\n" + i18n.T(lang, "approval.by_agent", req.Agent))
		}
	}
	if task := info.Task; task != "" && (info.Source != "" || info.Delegated) {
		b.WriteString("\n" + i18n.T(lang, "approval.for_task", clean(task, 120)))
	}
	full := g.Display(to.Account).ApprovalsDetail == config.ApprovalsFull
	if full {
		b.WriteString("\n\n" + g.exactCall(req, maxDetailInline))
	}
	if req.Rule != nil {
		b.WriteString("\n\n" + g.ruleText(lang, to, req))
		b.WriteString("\n\n" + i18n.T(lang, "approval.reply_always"))
	} else {
		b.WriteString("\n\n" + i18n.T(lang, "approval.reply"))
	}
	if !full {
		b.WriteString(" " + i18n.T(lang, "approval.details_hint"))
	}
	return b.String()
}

// rememberAsked and forgetAsked keep the record of a prompt waiting in a
// chat (state.Store.SaveAsked); without state.db there is none.
func (g *Gateway) rememberAsked(to Address, hash string) {
	if g.State == nil {
		return
	}
	if err := g.State.SaveAsked(context.Background(), to.key(), hash); err != nil {
		g.Log.Warn("record approval prompt failed", "err", err)
	}
}

func (g *Gateway) forgetAsked(to Address) {
	if g.State == nil {
		return
	}
	if err := g.State.ForgetAsked(context.Background(), to.key()); err != nil {
		g.Log.Warn("forget approval prompt failed", "err", err)
	}
}

// answerApproval consumes a reply to a pending approval. While one is
// pending, yes and no (in the chat's language or English) answer it,
// "details" shows the exact call and keeps it pending, and anything else
// gets a reminder instead of starting new work. A yes or no for a prompt
// that was waiting when the process restarted is answered too: the request
// expired, and the agent asks again if it still needs to.
func (g *Gateway) answerApproval(to Address, text, by string) bool {
	word := strings.ToLower(strings.TrimSpace(strings.Trim(strings.TrimSpace(text), ".!。！")))
	v, ok := g.pending.Load(to.key())
	if !ok {
		if g.State == nil || (!g.isYes(to, word) && !g.isNo(to, word)) {
			return false
		}
		if _, asked, err := g.State.Asked(context.Background(), to.key()); err != nil || !asked {
			return false
		}
		g.forgetAsked(to)
		g.reply(to, g.t(to, "approval.expired"))
		return true
	}
	p := v.(*pendingApproval)
	reply := "approval.reply"
	if p.always {
		reply = "approval.reply_always"
	}
	switch {
	case g.isYes(to, word):
		if p.resolve(tools.DecisionOnce, by) {
			g.reply(to, g.t(to, "approval.approved"))
		}
	case g.isNo(to, word):
		if p.resolve(tools.DecisionDeny, by) {
			g.reply(to, g.t(to, "approval.denied"))
		}
	case g.isAlways(to, word):
		switch {
		case !p.always:
			g.reply(to, g.t(to, "approval.always_unavailable"))
		case p.resolve(tools.DecisionAlways, by):
			g.reply(to, g.decided(to, tools.DecisionAlways))
		}
	case slices.Contains(i18n.Words(g.lang(to), "approval.details_words"), word):
		g.reply(to, g.t(to, "approval.details")+"\n"+g.exactCall(p.req, maxDetail)+"\n\n"+g.t(to, reply))
	case strings.HasPrefix(word, "/"):
		return false // commands always get through; /new and /stop decline the approval
	default:
		g.reply(to, g.t(to, "approval.waiting"))
	}
	return true
}

// resolveApproval answers the conversation's pending approval, if any; it
// reports whether this call was the answer.
func (g *Gateway) resolveApproval(to Address, ok bool) bool {
	d := tools.DecisionDeny
	if ok {
		d = tools.DecisionOnce
	}
	if v, found := g.pending.Load(to.key()); found {
		return v.(*pendingApproval).resolve(d, "")
	}
	return false
}

func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > n {
		return string([]rune(s)[:n]) + "..."
	}
	return s
}

// lockedFolders is the owner's trusted project files folder of the home
// an agent lives in, which no save may reach.
func lockedFolders(r *agent.Runner, agentName string) []string {
	spec, ok := r.Agents[agentName]
	if !ok || spec.Dir == "" {
		return nil
	}
	return []string{(&projtrust.Store{Home: filepath.Dir(filepath.Dir(spec.Dir))}).Folder()}
}

// typed reports whether a message is plain text the owner just typed, one
// to one: the adapter vouched for it (Message.Plain), and it has no quote
// of another message, no forward, no speech and no attachment. Only such a
// turn may use approval rules; a group's turns, every channel whose
// adapter does not set Plain, and every follow-up the gateway queues
// itself (/goal, /retry, /btw) never do.
func typed(m Message, owner bool) bool {
	return owner && m.Plain && m.Direct && m.Quote == nil && !m.Forwarded && !m.Spoken && len(m.Attachments) == 0
}

// TypedChannels are the channel types whose messages can be typed turns:
// their platforms report forwards and quotes (or the chat is the owner's
// own page), so an adapter's Plain means something. Every other channel
// (Slack, Discord, Signal, Matrix, Teams, Google Chat, email, SMS, ntfy,
// IRC, Zulip, DingTalk, WeCom and the rest) is never typed.
var TypedChannels = []string{"telegram", "whatsapp", "whatsappcloud", "whatsappagent", "web"}

// typed is typed(m, owner) on a channel in TypedChannels only.
func (g *Gateway) typed(m Message, owner bool) bool {
	ch := g.Channels[m.Account]
	return ch != nil && slices.Contains(TypedChannels, ch.Type) && typed(m, owner)
}
