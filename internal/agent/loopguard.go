package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// The loop guard ends turns that go round in circles long before
// MaxIterations would. It watches each round of tool calls for two things:
//
//   - repeat: one failing call retried straight away, again and again, with
//     the same tool, the same arguments and the same error each time. A try
//     counts only when it follows the previous one at once: in the next
//     round, with nothing else done in between, within repeatGap. An agent
//     that waits for a server by trying it, sleeping and trying again is
//     not repeating itself; the idle check still bounds such a loop;
//   - idle: rounds in a row whose every call repeats an earlier call of
//     the turn and gets the same result back, so nothing new is learned.
//
// At the warn count the model is told plainly that it is repeating itself,
// once per failing call and once per idle stretch. At the stop count the
// turn ends with a message to the user. A turn resumed after a crash
// rebuilds the counts from its own transcript, so a restart does not reset
// them.

// LoopLimits are an agent's loop guard thresholds; 0 turns a check off.
type LoopLimits struct {
	RepeatWarn, RepeatStop int
	IdleWarn, IdleStop     int
}

// HookLoopGuard fires when the loop guard warns the model or stops a turn.
const HookLoopGuard = "loop_guard"

// LoopGuardPayload is the JSON a loop_guard hook reads.
type LoopGuardPayload struct {
	Event   string `json:"event"`
	Agent   string `json:"agent"`
	Session string `json:"session"`
	Job     string `json:"job"`
	Check   string `json:"check"`  // repeat or idle
	Action  string `json:"action"` // warn or stop
	Tool    string `json:"tool,omitempty"`
	Count   int    `json:"count"`
}

// Loop guard checks and actions.
const (
	LoopRepeat = "repeat"
	LoopIdle   = "idle"
	LoopWarn   = "warn"
	LoopStop   = "stop"
)

// repeatGap is how long after a failed try the same try still counts as a
// straight retry. Longer means the agent waited.
const repeatGap = 20 * time.Second

type loopGuard struct {
	lim    LoopLimits
	seen   map[string]int      // call and result -> times seen this turn
	fails  map[string]int      // failing call and error -> tries in a row
	last   map[string]failMark // failing call and error -> its latest try
	warned map[string]bool     // failing calls already warned about
	idle   int                 // rounds in a row with nothing new
	idleOK bool                // the current idle stretch was warned about
	round  int
	busy   bool // the previous round had a call that worked
}

type failMark struct {
	round int
	at    time.Time
}

// loopVerdict is what one round of calls means for the turn.
type loopVerdict struct {
	Action string // "", LoopWarn or LoopStop
	Check  string
	Tool   string
	Error  string // the repeated error, for repeat
	Count  int
}

func newLoopGuard(lim LoopLimits) *loopGuard {
	return &loopGuard{lim: lim, seen: map[string]int{}, fails: map[string]int{}, last: map[string]failMark{}, warned: map[string]bool{}}
}

func (g *loopGuard) off() bool {
	return g.lim.RepeatStop <= 0 && g.lim.RepeatWarn <= 0 && g.lim.IdleStop <= 0 && g.lim.IdleWarn <= 0
}

// observe records one round: the calls of an assistant message and the
// blocks of the user message that answers them, which arrived at at (zero
// when unknown, which counts as straight away). It returns what to do.
//
// durs is how long each call ran, by call id (nil when unknown, as in a
// replay). A call that ran for waitAfter or more waited: it is not a
// repeat however often it fails, since that is how an agent waits for a
// server. A round in which every call waited is neither progress nor idle.
func (g *loopGuard) observe(calls, results []llm.Block, at time.Time, durs map[string]time.Duration) loopVerdict {
	g.round++
	busy := false
	waitedAll := len(calls) > 0
	byID := map[string]llm.Block{}
	for _, r := range results {
		if r.Type == llm.ToolResult {
			byID[r.ToolUseID] = r
		}
	}
	fresh := false
	var worst loopVerdict
	worstKey := ""
	for _, c := range calls {
		res, ok := byID[c.ToolUseID]
		if !ok {
			fresh = true // no result recorded: cannot tell, so count it as new
			continue
		}
		waited := waits(c, durs[c.ToolUseID])
		waitedAll = waitedAll && waited
		call := c.ToolName + "\x00" + canonicalJSON(c.Input)
		sum := sha256.Sum256([]byte(res.Text))
		key := fmt.Sprintf("%s\x00%t\x00%x", call, res.IsError, sum[:12])
		if g.seen[key]++; g.seen[key] == 1 {
			fresh = true
		}
		if !res.IsError {
			busy = true
			continue
		}
		if waited {
			g.fails[key] = 0
			delete(g.last, key)
			continue
		}
		prev, ok := g.last[key]
		quick := prev.at.IsZero() || at.IsZero() || at.Sub(prev.at) < repeatGap
		if ok && prev.round == g.round-1 && !g.busy && quick {
			g.fails[key]++
		} else {
			g.fails[key] = 1
		}
		g.last[key] = failMark{round: g.round, at: at}
		if g.fails[key] > worst.Count {
			worst = loopVerdict{Check: LoopRepeat, Tool: c.ToolName, Error: res.Text, Count: g.fails[key]}
			worstKey = key
		}
	}
	g.busy = busy
	switch {
	case fresh:
		g.idle, g.idleOK = 0, false
	case waitedAll:
		// Waiting is neither progress nor going round in circles.
	case len(calls) > 0:
		g.idle++
	}
	switch {
	case g.lim.RepeatStop > 0 && worst.Count >= g.lim.RepeatStop:
		worst.Action = LoopStop
		return worst
	case g.lim.IdleStop > 0 && g.idle >= g.lim.IdleStop:
		return loopVerdict{Action: LoopStop, Check: LoopIdle, Count: g.idle}
	case g.lim.RepeatWarn > 0 && worst.Count >= g.lim.RepeatWarn && !g.warned[worstKey]:
		g.warned[worstKey] = true
		worst.Action = LoopWarn
		return worst
	case g.lim.IdleWarn > 0 && g.idle >= g.lim.IdleWarn && !g.idleOK:
		g.idleOK = true
		return loopVerdict{Action: LoopWarn, Check: LoopIdle, Count: g.idle}
	}
	return loopVerdict{}
}

// replay rebuilds the counts from the rounds a turn's earlier run already
// recorded. Warnings it would give again are marked as given.
func (g *loopGuard) replay(recs []session.Record, jobID string) {
	for i, rec := range recs {
		if rec.JobID != jobID || rec.Message.Role != llm.Assistant {
			continue
		}
		calls := rec.Message.ToolUses()
		if len(calls) == 0 || i+1 >= len(recs) || recs[i+1].Message.Role != llm.User {
			continue
		}
		g.observe(calls, recs[i+1].Message.Blocks, recs[i+1].At, nil)
	}
}

// waitAfter is how long a call must have run to count as waiting. Only
// time actually spent counts: an instant failure is a repeat whatever its
// command or tool name says (timeout 120 ..., npm run watch, a *_wait tool
// on an unknown id).
const waitAfter = 2 * time.Second

// waits reports whether a call waited: it ran for waitAfter or more. A
// replayed round has no timings, so none of its calls waited.
func waits(_ llm.Block, ran time.Duration) bool {
	return ran >= waitAfter
}

// canonicalJSON renders a call's arguments with object keys sorted, so the
// same arguments in another order count as the same call.
func canonicalJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// warning is the note added after the round's results.
func (v loopVerdict) warning() string {
	if v.Check == LoopIdle {
		return fmt.Sprintf("[Loop guard: your last %d rounds of tool calls only repeated calls you had already made, and got the same results. "+
			"You are not making progress. Try a different approach, or stop and tell the user what you found and what is blocking you.]", v.Count)
	}
	return fmt.Sprintf("[Loop guard: you have now called %s %d times with the same arguments, and it failed the same way each time: %s\n"+
		"Calling it again will not work. Fix what causes the error, try a different approach, or stop and tell the user what is blocking you.]",
		v.Tool, v.Count, cutRunes(oneLineText(v.Error), 300))
}

// stopMessage is the reply that ends a stopped turn.
func (v loopVerdict) stopMessage() string {
	if v.Check == LoopIdle {
		return fmt.Sprintf("I stopped working on this because I was going round in circles: my last %d steps only repeated earlier ones and got the same results. "+
			"Tell me how you'd like me to go on, or give me more to work with.", v.Count)
	}
	return fmt.Sprintf("I stopped working on this because I was going round in circles: I tried %s %d times and it failed the same way each time (%s). "+
		"Tell me how you'd like me to go on.", v.Tool, v.Count, cutRunes(oneLineText(v.Error), 200))
}

func oneLineText(s string) string {
	out := make([]rune, 0, len(s))
	space := false
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' || r == ' ' {
			if !space && len(out) > 0 {
				out = append(out, ' ')
			}
			space = true
			continue
		}
		space = false
		out = append(out, r)
	}
	return string(out)
}

// guardRound applies the loop guard to a finished round. It returns the
// round's results with a warning added, and stop true when the turn must
// end; the caller then records the results and ends with t.stopLoop.
func (t *turn) guardRound(calls, results []llm.Block) ([]llm.Block, *loopVerdict) {
	if t.guard == nil {
		return results, nil
	}
	now := time.Now
	if t.r.Now != nil {
		now = t.r.Now
	}
	v := t.guard.observe(calls, results, now(), t.callDur)
	if v.Action == "" {
		return results, nil
	}
	t.r.Fire(t.ctx, HookLoopGuard, t.spec.Name, LoopGuardPayload{Event: HookLoopGuard, Agent: t.spec.Name, Session: t.sess.ID,
		Job: t.job.ID, Check: v.Check, Action: v.Action, Tool: v.Tool, Count: v.Count})
	t.r.Log.Warn("loop guard", "agent", t.spec.Name, "session", t.sess.ID, "check", v.Check, "action", v.Action, "tool", v.Tool, "count", v.Count)
	if v.Action == LoopStop {
		return results, &v
	}
	return append(results, llm.Block{Type: llm.Text, Text: v.warning()}), nil
}

// stopLoop ends a turn the loop guard stopped: the reply tells the user why.
func (t *turn) stopLoop(v *loopVerdict) error {
	msg := v.stopMessage()
	if err := t.append(llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: msg}}}, "", nil); err != nil {
		return err
	}
	t.final, t.loopStopped = msg, true
	return nil
}

// LoopStopped is the error of a turn nobody was watching (a schedule, a
// board card) that the loop guard stopped: such a run failed, and its
// schedule says so. The message is the one the user would have got.
type LoopStopped struct{ Message string }

func (e LoopStopped) Error() string { return e.Message }
