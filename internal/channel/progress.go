package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/state"
)

// Live progress drafts. On a platform that can edit messages (Editor), a
// turn that runs past ProgressAfter posts one "working" message and keeps
// editing it with a line per tool call, at most once every ProgressEvery.
// The draft is best effort and never carries the answer: the reply is still
// a delivery job, which removes the draft before it sends. The draft's id is
// kept in state.db under the turn, so a turn that reruns after a crash edits
// the same message and its delivery still finds it.

const (
	defaultProgressAfter = 8 * time.Second
	defaultProgressEvery = 3 * time.Second
	maxProgressLines     = 8
	workingText          = "_Working on it…_"
)

// drafts reports whether the conversation's channel shows progress drafts.
func (g *Gateway) drafts(to Address) bool {
	if g.State == nil || g.Runner == nil || g.Runner.Bus == nil {
		return false
	}
	if _, ok := g.Adapters[to.Account].(Editor); !ok {
		return false
	}
	return g.Channels[to.Account].ProgressMode() == config.ProgressDraft
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
	a      Adapter
	turnID string
	to     Address
	id     string   // the draft's message id once posted
	lines  []string // tool lines, newest last
	more   int      // lines dropped off the top
	shown  string   // text of the last post or edit
}

func (g *Gateway) runProgress(ctx context.Context, turnID string, to Address, evs <-chan events.Event) {
	a := g.Adapters[to.Account]
	p := &progress{g: g, ed: a.(Editor), a: a, turnID: turnID, to: to}
	to.ReplyTo = ""
	p.to = to
	if d, err := g.State.Draft(ctx, turnID); err == nil && d != nil {
		p.id = d.MessageID // a rerun of the turn: keep using its draft
	}
	after, every := g.ProgressAfter, g.ProgressEvery
	if after <= 0 {
		after = defaultProgressAfter
	}
	if every <= 0 {
		every = defaultProgressEvery
	}
	wait := time.NewTimer(after)
	if p.id != "" {
		wait.Reset(0)
	}
	defer wait.Stop()
	tick := time.NewTicker(every)
	defer tick.Stop()
	due := false // the timer fired: a draft should be showing
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-evs:
			if e.Kind == events.ToolStart {
				p.add(toolLine(e.Text))
			}
			continue
		case <-wait.C:
			due = true
		case <-tick.C:
		}
		if due {
			p.flush(ctx)
		}
	}
}

func (p *progress) add(line string) {
	p.lines = append(p.lines, line)
	if n := len(p.lines) - maxProgressLines; n > 0 {
		p.lines, p.more = p.lines[n:], p.more+n
	}
}

func (p *progress) text() string {
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

// flush posts the draft, or edits it when its text changed. A failed post
// or edit is logged and tried again on the next tick.
func (p *progress) flush(ctx context.Context) {
	text := render(p.a, p.text())
	if text == p.shown {
		return
	}
	// The platform call is not cut short by the turn ending, so a post that
	// went out is always recorded and later removed.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if p.id == "" {
		id, err := p.ed.Post(cctx, p.to, text)
		if err != nil {
			p.g.Log.Debug("progress draft failed", "channel", p.to.Account, "err", err)
			return
		}
		p.id = id
		addr, _ := json.Marshal(p.to)
		if err := p.g.State.SaveDraft(cctx, p.turnID, state.Draft{Address: string(addr), MessageID: id}); err != nil {
			p.g.Log.Warn("record progress draft failed", "err", err)
		}
	} else if err := p.ed.Edit(cctx, p.to, p.id, text); err != nil {
		p.g.Log.Debug("progress edit failed", "channel", p.to.Account, "err", err)
		return
	}
	p.shown = text
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

// finishDraft removes the progress draft of the turn a delivery answers,
// before the reply goes out. A draft that cannot be deleted is left; the
// reply is never held back for it.
func (g *Gateway) finishDraft(ctx context.Context, turnID string, a Adapter, to Address) {
	if turnID == "" || g.State == nil {
		return
	}
	d, err := g.State.Draft(ctx, turnID)
	if err != nil || d == nil {
		return
	}
	if ed, ok := a.(Editor); ok {
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
