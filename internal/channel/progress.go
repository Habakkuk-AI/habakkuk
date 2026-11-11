package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/state"
	"github.com/habakkuk-ai/habakkuk/internal/ops/i18n"
)

// Live progress drafts. On a platform that can edit messages (Editor), a
// turn that runs past ProgressAfter posts one "working" message and keeps
// editing it, at most once every ProgressEvery. What it says is the
// channel's progress setting: plain (the default) is one short line in
// everyday words, such as "Checking the web", that never names a tool, a
// command, a path or an id; detailed is a line per tool call, for people
// who want to watch the steps. With stream on (the default), the message
// also shows the reply while the model writes it. The draft is best effort:
// the reply is still a delivery job. It removes a progress draft before it
// sends, and turns a draft showing the reply into the reply's first part by
// one last edit. The draft's id is kept in state.db under the turn, so a
// turn that reruns after a crash edits the same message and its delivery
// still finds it.

const (
	defaultProgressAfter = 8 * time.Second
	defaultProgressEvery = 3 * time.Second
	maxProgressLines     = 8
	workingText          = "_Working on it…_"
	// plainElapsedAfter is how long a turn runs before the plain line says
	// for how long ("Working on it… (40s)").
	plainElapsedAfter = 20 * time.Second
)

// drafts reports whether the conversation's channel shows progress drafts.
func (g *Gateway) drafts(to Address) bool {
	if g.State == nil || g.Runner == nil || g.Runner.Bus == nil {
		return false
	}
	if _, ok := g.Adapters[to.Account].(Editor); !ok {
		return false
	}
	d := g.Display(to.Account)
	return d.Progress != config.ProgressOff || d.Stream
}

// startProgress runs the turn's progress draft until the returned func is
// called; the func waits for an edit in flight, so nothing is posted after
// it returns. It is safe to call more than once.
func (g *Gateway) startProgress(ctx context.Context, turnID, sessionID string, to Address) func() {
	if !g.drafts(to) {
		return func() {}
	}
	evs, unsub := g.Runner.Bus.Subscribe(sessionID)
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer unsub()
		g.runProgress(ctx, turnID, to, evs)
	}()
	var once sync.Once
	return func() { once.Do(func() { cancel(); <-done }) }
}

type progress struct {
	g      *Gateway
	ed     Editor
	loud   LoudPoster // set when Post is silent and a streamed reply needs its own message
	a      Adapter
	turnID string
	to     Address // where the draft goes, without the quoted message
	quote  string  // the message a streamed reply quotes
	id     string  // the draft's message id once posted
	shown  string  // text of the last post or edit
	// showsAnswer: the message shows the reply being written, not progress.
	showsAnswer bool
	lastFlush   time.Time

	// What the turn is doing, from its events. The event reader writes
	// these while the loop reads them, so they sit behind mu.
	mu     sync.Mutex
	lines  []string // detailed: tool lines, newest last
	more   int      // detailed: lines dropped off the top
	phrase string   // plain: the line shown, in the chat's language
	answer string   // the reply text so far, while the model writes it
	began  time.Time
	// Settings read once, when the turn starts.
	plain     bool
	lines0    bool // progress is shown at all (not off)
	stream    bool
	lang      string
	started   time.Time
	streamGap time.Duration
}

// Streaming a reply as it is written. The message is posted once the text
// is long enough to be worth reading, or has been coming for a moment, so
// a short "let me check" before a tool call rarely shows; it is then edited
// at most once every streamEvery. Text written before a tool call is not
// the reply: when a tool starts the message goes back to showing progress.
const (
	defaultStreamEvery = 1200 * time.Millisecond
	streamMinChars     = 60
	streamMinWait      = 1500 * time.Millisecond
	streamCursor       = " ▍"
)

func (g *Gateway) runProgress(ctx context.Context, turnID string, to Address, evs <-chan events.Event) {
	a := g.Adapters[to.Account]
	d := g.Display(to.Account)
	// The settings are read once, when the turn starts: a change made
	// while it runs applies from the next turn.
	p := &progress{g: g, ed: a.(Editor), a: a, turnID: turnID, lang: g.lang(to), started: g.now(),
		plain: d.Progress != config.ProgressDetailed, lines0: d.Progress != config.ProgressOff, stream: d.Stream,
		quote: to.ReplyTo, streamGap: g.StreamEvery}
	if p.streamGap <= 0 {
		p.streamGap = defaultStreamEvery
	}
	if lp, ok := a.(LoudPoster); ok {
		p.loud = lp
	}
	to.ReplyTo = ""
	p.to = to
	if d, err := g.State.Draft(ctx, turnID); err == nil && d != nil {
		p.id = d.MessageID // a rerun of the turn: keep using its draft
		p.showsAnswer = d.Answer != ""
	}
	after, every := g.ProgressAfter, g.ProgressEvery
	if after <= 0 {
		after = defaultProgressAfter
	}
	if every <= 0 {
		every = defaultProgressEvery
	}
	// The events are read on their own, so a slow edit never makes the
	// bus drop the reply's text.
	kick := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case e := <-evs:
				if p.take(e) {
					select {
					case kick <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	wait := time.NewTimer(after)
	if p.id != "" {
		wait.Reset(0)
	}
	defer wait.Stop()
	tick := time.NewTicker(min(every, p.streamGap))
	defer tick.Stop()
	due := false // the timer fired: a progress draft should be showing
	for {
		select {
		case <-ctx.Done():
			return
		case <-kick:
		case <-wait.C:
			due = true
		case <-tick.C:
		}
		p.step(ctx, due, every)
	}
}

// take records one event; it reports whether the message may need to
// change now.
func (p *progress) take(e events.Event) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch e.Kind {
	case events.ToolStart:
		p.answer = "" // what came before a tool call was not the reply
		p.tool(e)
		return true
	case events.Retry:
		p.answer = "" // the model starts again
		return true
	case events.Text:
		if !p.stream {
			return false
		}
		if p.answer == "" {
			p.began = p.g.now()
		}
		p.answer += e.Text
		return true
	}
	return false
}

// step posts or edits the message if what it should show has changed and
// enough time has passed since the last change.
func (p *progress) step(ctx context.Context, due bool, every time.Duration) {
	p.mu.Lock()
	answer, began := p.answer, p.began
	text := ""
	if !p.answerReady(answer, began) {
		answer = ""
		if due && p.lines0 || p.showsAnswer {
			text = p.text()
		}
	}
	p.mu.Unlock()
	now := p.g.now()
	if answer != "" {
		if now.Sub(p.lastFlush) < p.streamGap {
			return
		}
		p.flushAnswer(ctx, answer)
		return
	}
	if text == "" || now.Sub(p.lastFlush) < every && !p.showsAnswer {
		return
	}
	p.flush(ctx, text)
}

// answerReady reports whether the reply written so far should be shown.
func (p *progress) answerReady(answer string, began time.Time) bool {
	trimmed := strings.TrimSpace(answer)
	if !p.stream || trimmed == "" {
		return false
	}
	if strings.HasPrefix(Silent, trimmed) || strings.HasPrefix(trimmed, Silent) {
		return false // a reply that sends nothing must not show while written
	}
	return utf8.RuneCountInString(trimmed) >= streamMinChars || p.g.now().Sub(began) >= streamMinWait
}

// tool takes in a tool call that just started. The caller holds p.mu.
func (p *progress) tool(e events.Event) {
	if !p.plain {
		p.add(toolLine(e.Text))
		return
	}
	name := e.Tool
	if name == "" {
		name, _, _ = strings.Cut(e.Text, " ")
	}
	p.phrase = p.g.plainPhrase(p.lang, name, e.Input)
}

// plainText is the plain line: what the agent is doing in everyday words
// and, once the turn has run a while, for how long. The time moves in steps
// (tens of seconds, then minutes) so the message is not edited every tick.
func (p *progress) plainText() string {
	phrase := p.phrase
	if phrase == "" {
		phrase = i18n.T(p.lang, "progress.working")
	}
	line := i18n.T(p.lang, "progress.line", phrase)
	switch el := p.g.now().Sub(p.started); {
	case el >= time.Minute:
		line = i18n.T(p.lang, "progress.elapsed_min", phrase, int(el/time.Minute))
	case el >= plainElapsedAfter:
		line = i18n.T(p.lang, "progress.elapsed_sec", phrase, int(el/(10*time.Second))*10)
	}
	return "_" + line + "_"
}

func (p *progress) add(line string) {
	p.lines = append(p.lines, line)
	if n := len(p.lines) - maxProgressLines; n > 0 {
		p.lines, p.more = p.lines[n:], p.more+n
	}
}

// text is the progress the message shows. The caller holds p.mu.
func (p *progress) text() string {
	if p.plain {
		return p.plainText()
	}
	var b strings.Builder
	b.WriteString(workingText)
	if len(p.lines) > 0 {
		b.WriteString("\n")
	}
	if p.more > 0 {
		fmt.Fprintf(&b, "\n(%d earlier step%s)", p.more, map[bool]string{true: "s"}[p.more != 1])
	}
	for _, l := range p.lines {
		b.WriteString("\n" + l)
	}
	return b.String()
}

// platformCtx is for one platform call. It is not cut short by the turn
// ending, so a post that went out is always recorded and later removed.
func platformCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
}

// flush posts the progress draft, or edits it when its text changed. A
// failed post or edit is logged and tried again on the next tick.
func (p *progress) flush(ctx context.Context, markdown string) {
	text := render(p.a, markdown)
	if text == p.shown {
		return
	}
	cctx, cancel := platformCtx(ctx)
	defer cancel()
	p.lastFlush = p.g.now()
	if p.id == "" {
		id, err := p.ed.Post(cctx, p.to, text)
		if err != nil {
			p.g.Log.Debug("progress draft failed", "channel", p.to.Account, "err", err)
			return
		}
		p.id = id
		p.save(cctx, "")
	} else {
		if err := p.ed.Edit(cctx, p.to, p.id, text); err != nil {
			p.g.Log.Debug("progress edit failed", "channel", p.to.Account, "err", err)
			return
		}
		if p.showsAnswer {
			p.save(cctx, "") // the delivery must not take this message for the reply
		}
	}
	p.shown, p.showsAnswer = text, false
}

// flushAnswer shows the reply written so far. The reply always starts as a
// new message: a progress draft already in the chat is removed first, so
// the reply lands below anything posted since (an approval prompt, say) and
// notifies where posts notify. Where progress drafts are silent, the reply
// is posted with PostLoud so it notifies there too.
func (p *progress) flushAnswer(ctx context.Context, answer string) {
	text := render(p.a, streamPreview(answer, chunkLimit(p.a)))
	if text == p.shown {
		return
	}
	cctx, cancel := platformCtx(ctx)
	defer cancel()
	p.lastFlush = p.g.now()
	if p.id != "" && !p.showsAnswer {
		if err := p.ed.Delete(cctx, p.to, p.id); err != nil {
			p.g.Log.Debug("remove progress draft failed", "channel", p.to.Account, "err", err)
			return
		}
		p.id = ""
		if err := p.g.State.DeleteDraft(cctx, p.turnID); err != nil {
			p.g.Log.Warn("forget progress draft failed", "err", err)
		}
	}
	if p.id == "" {
		var id string
		var err error
		if p.loud != nil {
			id, err = p.loud.PostLoud(cctx, p.to, text, p.quote)
		} else {
			id, err = p.ed.Post(cctx, p.to, text)
		}
		if err != nil {
			p.g.Log.Debug("streamed reply failed", "channel", p.to.Account, "err", err)
			return
		}
		p.id = id
	} else if err := p.ed.Edit(cctx, p.to, p.id, text); err != nil {
		p.g.Log.Debug("streamed reply edit failed", "channel", p.to.Account, "err", err)
		return
	}
	p.save(cctx, answer)
	p.shown, p.showsAnswer = text, true
}

// save records the draft, with the reply it shows ("" for progress).
func (p *progress) save(ctx context.Context, answer string) {
	addr, _ := json.Marshal(p.to)
	if err := p.g.State.SaveDraft(ctx, p.turnID, state.Draft{Address: string(addr), MessageID: p.id, Answer: answer}); err != nil {
		p.g.Log.Warn("record progress draft failed", "err", err)
	}
}

// streamPreview is the reply so far as Markdown fit to show while more is
// coming: cut to the chunk size (the rest follows when it is sent), with
// code spans, code blocks and bold closed so the platform can render it,
// and a cursor that says it is still being written.
func streamPreview(answer string, limit int) string {
	s := strings.TrimRight(answer, " \t\n")
	if limit -= 8; limit > 0 && utf8.RuneCountInString(s) > limit {
		s = string([]rune(s)[:limit]) + "…"
	}
	if strings.Count(s, "```")%2 == 1 {
		return s + "\n```" // the cursor would land inside the block
	}
	if strings.Count(strings.ReplaceAll(s, "```", ""), "`")%2 == 1 {
		s += "`"
	}
	if strings.Count(s, "**")%2 == 1 {
		s += "**"
	}
	return s + streamCursor
}

var titleField = regexp.MustCompile(`"(?:command|path|url|query|title|task|name|pattern|action)"\s*:\s*"((?:[^"\\]|\\.)*)`)

// toolLine turns a tool_start event ("name {input preview}") into a short
// line: the tool's name and, when the input has one, a title such as the
// command or path.
func toolLine(ev string) string {
	name, input, _ := strings.Cut(ev, " ")
	line := "→ " + name
	if m := titleField.FindStringSubmatch(input); m != nil {
		title := strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, " ", "`", "'").Replace(m[1])
		if title = shorten(title, 60); title != "" {
			line += " `" + title + "`"
		}
	}
	return line
}

// draftIsReply marks a draft that became the first part of its reply, so a
// delivery that reruns after a crash neither deletes it nor sends that
// part again.
const draftIsReply = "\x00reply"

// adoptDraft turns a draft that shows the reply being written into the
// reply's first chunk, by one last edit, and reports whether it did. The
// draft is taken only when what it shows is the start of this reply: a
// failure notice or a reply that came out differently gets a message of its
// own. The steps are ordered so a crash at any point resumes without a
// second copy: edit, record the draft as the reply, count the chunk sent.
func (g *Gateway) adoptDraft(ctx context.Context, jobID, turnID string, a Adapter, to Address, first string) bool {
	if turnID == "" || g.State == nil {
		return false
	}
	d, err := g.State.Draft(ctx, turnID)
	if err != nil || d == nil || d.Answer == "" {
		return false
	}
	ed, ok := a.(Editor)
	if !ok {
		return false
	}
	if d.Answer != draftIsReply {
		if !sameStart(d.Answer, first) {
			return false
		}
		var where Address
		if json.Unmarshal([]byte(d.Address), &where) != nil {
			where = to
		}
		if err := ed.Edit(ctx, where, d.MessageID, render(a, first)); err != nil {
			g.Log.Warn("finish streamed reply failed; sending it anew", "channel", to.Account, "err", err)
			return false
		}
		d.Answer = draftIsReply
		if err := g.State.SaveDraft(ctx, turnID, *d); err != nil {
			g.Log.Warn("record streamed reply failed", "err", err)
		}
	}
	if err := g.Store.MarkSent(ctx, jobID, 1); err != nil {
		g.Log.Warn("count streamed reply failed", "err", err)
		return false
	}
	return true
}

// sameStart reports whether the reply begins with the text streamed for it.
func sameStart(streamed, reply string) bool {
	a, b := []rune(strings.TrimSpace(streamed)), []rune(strings.TrimSpace(reply))
	n := min(len(a), len(b), 80)
	return n > 0 && string(a[:n]) == string(b[:n])
}

// finishDraft removes the progress draft of the turn a delivery answers,
// before the reply goes out. A draft that cannot be deleted is left; the
// reply is never held back for it. A draft that became the reply is only
// forgotten.
func (g *Gateway) finishDraft(ctx context.Context, turnID string, a Adapter, to Address) {
	if turnID == "" || g.State == nil {
		return
	}
	d, err := g.State.Draft(ctx, turnID)
	if err != nil || d == nil {
		return
	}
	if ed, ok := a.(Editor); ok && d.Answer != draftIsReply {
		var where Address
		if json.Unmarshal([]byte(d.Address), &where) != nil {
			where = to
		}
		if err := ed.Delete(ctx, where, d.MessageID); err != nil {
			g.Log.Warn("remove progress draft failed", "channel", to.Account, "err", err)
		}
	}
	if err := g.State.DeleteDraft(ctx, turnID); err != nil {
		g.Log.Warn("forget progress draft failed", "err", err)
	}
}

// clearDraft queues a delivery that only removes a turn's draft, for turns
// that end without a reply (silent, or stopped).
func (g *Gateway) clearDraft(ctx context.Context, turnID string, to Address) {
	if g.State == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if d, err := g.State.Draft(ctx, turnID); err != nil || d == nil {
		return
	}
	if err := g.enqueue(ctx, "dlv_clear_"+turnID, deliverPayload{To: to, Draft: turnID}); err != nil {
		g.Log.Warn("queue draft removal failed", "err", err)
	}
}

// settleStopped clears the draft of a turn that ended because it was
// canceled (/stop); a turn that lost its lease is still running elsewhere
// and keeps its draft.
func (g *Gateway) settleStopped(ctx context.Context, j *jobs.Job, to Address) {
	cur, err := g.Queue.Get(context.WithoutCancel(ctx), j.ID)
	if err == nil && cur.Status == jobs.Canceled {
		g.clearDraft(ctx, j.ID, to)
	}
}
