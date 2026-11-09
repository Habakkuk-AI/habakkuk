package agent

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/judge"
)

// Routing modes for an agent with a fast model (config agents.X.routing).
const (
	RouteAuto  = "auto"  // the fast model for light turns, the main model for real work
	RouteFast  = "fast"  // the fast model first for every turn; the main chain as fallbacks
	RouteSmart = "smart" // always the main model; the fast model only runs chores
)

// Defaults for automatic routing.
const (
	// DefaultEscalateAfter is how many tool rounds a turn may take on the
	// fast model before the rest of it moves to the main model.
	DefaultEscalateAfter = 4
	// stickyFor keeps short follow-ups ("yes, do it") on the main model
	// for a while after a turn that needed it.
	stickyFor = 15 * time.Minute
	// followUpWords: a message this short right after real work is
	// treated as part of that work.
	followUpWords = 12
	// longMessage: past this many characters a message is treated as real work.
	longMessage = 600
)

// workWords marks requests that are more than a quick exchange: building,
// fixing, researching, writing something long, planning, reviewing, or
// operating a computer or browser.
var workWords = regexp.MustCompile(`(?i)\b(` +
	`build|code|coding|program|script|debug|fix|bug|error|stack ?trace|implement|refactor|deploy|migrat\w*|` +
	`analy[sz]\w*|research\w*|investigat\w*|compare|comparison|evaluate|audit|review\w*|` +
	`plan|planning|strategy|strateg\w*|design|architect\w*|spec|proposal|` +
	`report|essay|article|blog post|whitepaper|contract|invoice|spreadsheet|financial\w*|` +
	`step by step|think (hard|carefully|deeply)|in detail|detailed|thorough\w*|` +
	`browse|website|web ?page|click|screen|computer|` +
	`schedule|automate|workflow|delegate|agents?` +
	`)\b`)

// smartReason says why a turn needs the main model, or "" when the fast
// model can take it. It looks only at what the turn starts with; a turn
// that grows into real work escalates later (see escalate).
func smartReason(text string, media []Attachment, history []llm.Message) string {
	switch {
	case len(media) > 0:
		return "attachments"
	case hasMedia(history):
		// A text-only fast model would turn media off for the whole session.
		return "attachments in the conversation"
	case strings.Contains(text, "```"):
		return "code"
	case len(text) > longMessage:
		return "long message"
	}
	if m := workWords.FindString(text); m != "" {
		return "asks for " + strings.ToLower(m)
	}
	return ""
}

// pickRoute chooses the chain for a turn on an agent with a fast model.
// Only turns of KindTurn are routed (sub-agents do delegated work on the
// main chain), and never past a /model choice.
func (t *turn) pickRoute(text string, media []Attachment) {
	spec := t.spec
	if len(spec.FastChain) == 0 || t.overridden || t.job.Kind != KindTurn {
		return
	}
	t.mainChain = t.chain
	switch spec.Routing {
	case RouteSmart:
		return
	case RouteFast:
		t.useFast("routing: fast")
		return
	}
	if reason := mediaReason(media, t.msgs); reason != "" {
		t.r.Log.Debug("route: main model", "session", t.sess.ID, "reason", reason)
		t.r.routes.Store(t.sess.ID, time.Now())
		return
	}
	at, recent := t.r.routes.Load(t.sess.ID)
	recent = recent && time.Since(at.(time.Time)) < stickyFor
	if smart, ok := t.askJudge(text, recent); ok {
		if smart {
			t.r.routes.Store(t.sess.ID, time.Now())
			return
		}
		t.useFast("decision model: light turn")
		return
	}
	if reason := smartReason(text, media, t.msgs); reason != "" {
		t.r.Log.Debug("route: main model", "session", t.sess.ID, "reason", reason)
		t.r.routes.Store(t.sess.ID, time.Now())
		return
	}
	if recent && len(strings.Fields(text)) <= followUpWords {
		t.r.Log.Debug("route: main model", "session", t.sess.ID, "reason", "follow-up to real work")
		t.r.routes.Store(t.sess.ID, time.Now())
		return
	}
	t.useFast("light turn")
}

// mediaReason is the part of smartReason no judge overrides: a fast model
// may not see images at all.
func mediaReason(media []Attachment, history []llm.Message) string {
	switch {
	case len(media) > 0:
		return "attachments"
	case hasMedia(history):
		return "attachments in the conversation"
	}
	return ""
}

var realWorkQ = judge.Yes(`Does replying well to the latest message need real work: several steps, tools, code, research, careful reasoning, a plan, or writing something long? A greeting, thanks, a quick question, a reminder or a short factual answer is not real work. When previous_turn says the last turn was real work and the latest message is a short go-ahead or follow-up to it, that is real work.`)

// askJudge asks the decision model whether a turn is real work. ok is
// false when there is no judge or it did not answer in time.
func (t *turn) askJudge(text string, recent bool) (smart, ok bool) {
	if t.r.Judge == nil {
		return false, false
	}
	prev := "none"
	if recent {
		prev = "the last turn, a few minutes ago, was real work"
	}
	state := map[string]string{"latest_message": text, "previous_turn": prev}
	ans, err := t.r.Judge.Ask(t.ctx, state, map[string]judge.Question{"real_work": realWorkQ})
	if err != nil {
		if !errors.Is(err, judge.ErrNoKey) {
			t.r.Log.Warn("route: decision model failed; using word rules", "session", t.sess.ID, "err", err)
		}
		return false, false
	}
	p := ans["real_work"].P
	t.r.Log.Debug("route: decision model", "session", t.sess.ID, "p", p)
	return p >= 0.5, true
}

func (t *turn) useFast(reason string) {
	t.chain = t.spec.FastChain
	t.onFast = true
	t.r.Log.Debug("route: fast model", "session", t.sess.ID, "model", t.chain[0].String(), "reason", reason)
}

// escalate moves the rest of a turn on the fast model to the main model
// once it has taken EscalateAfter tool rounds: by then it is real work.
func (t *turn) escalate(rounds int) {
	if !t.onFast || t.spec.Routing == RouteFast || rounds < t.spec.EscalateAfter {
		return
	}
	t.onFast = false
	t.chain = t.mainChain
	t.r.routes.Store(t.sess.ID, time.Now())
	t.r.Log.Info("route: escalated to the main model", "session", t.sess.ID, "model", t.chain[0].String(), "tool_rounds", rounds)
}

// choreChain is the chain for work around a turn, such as compaction
// summaries: the fast model when the agent has one, unless /model chose.
func (t *turn) choreChain() []llm.Target {
	if len(t.spec.FastChain) > 0 && !t.overridden {
		return t.spec.FastChain
	}
	return t.chain
}

// routingNote describes an agent's routing for /model, or "" without a
// fast model.
func routingNote(spec *Spec) string {
	if len(spec.FastChain) == 0 {
		return ""
	}
	fast, main := spec.FastChain[0].String(), spec.Chain[0].String()
	switch spec.Routing {
	case RouteFast:
		return "Routing: fast. Every turn starts on " + fast + "; " + main + " takes over if it fails."
	case RouteSmart:
		return "Routing: smart. Turns use " + main + "; " + fast + " writes titles and summaries."
	}
	return fmt.Sprintf("Routing: auto. Quick messages go to %s; real work (code, research, documents, attachments, long requests) goes to %s, and so does a quick turn after %d tool steps.",
		fast, main, spec.EscalateAfter)
}

// FastChain builds the chain for a fast model: the fast model first, then
// the agent's own chain, so a failure falls back to the main model.
func FastChain(fast llm.Target, main []llm.Target) []llm.Target {
	out := []llm.Target{fast}
	for _, t := range main {
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}
