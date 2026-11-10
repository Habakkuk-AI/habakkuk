package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

func useCall(id, tool, input string) llm.Block {
	return llm.Block{Type: llm.ToolUse, ToolUseID: id, ToolName: tool, Input: json.RawMessage(input)}
}

func result(id, text string, isErr bool) llm.Block {
	return llm.Block{Type: llm.ToolResult, ToolUseID: id, Text: text, IsError: isErr}
}

func TestLoopGuardRepeatWarnsThenStops(t *testing.T) {
	g := newLoopGuard(LoopLimits{RepeatWarn: 3, RepeatStop: 5})
	var got []string
	for i := 1; i <= 5; i++ {
		id := fmt.Sprint("c", i)
		// Same arguments in another key order are the same call.
		in := `{"path":"a.txt","limit":10}`
		if i%2 == 0 {
			in = `{"limit":10,"path":"a.txt"}`
		}
		v := g.observe([]llm.Block{useCall(id, "read_file", in)}, []llm.Block{result(id, "no such file", true)}, time.Time{}, nil)
		got = append(got, v.Action)
	}
	want := []string{"", "", LoopWarn, "", LoopStop}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("actions = %q, want %q", got, want)
	}
}

func TestLoopGuardDifferentErrorsAreNotARepeat(t *testing.T) {
	g := newLoopGuard(LoopLimits{RepeatWarn: 2, RepeatStop: 3, IdleWarn: 5, IdleStop: 6})
	for i := 1; i <= 6; i++ {
		id := fmt.Sprint("c", i)
		v := g.observe([]llm.Block{useCall(id, "exec", `{"command":"make"}`)},
			[]llm.Block{result(id, fmt.Sprintf("error on line %d", i), true)}, time.Time{}, nil)
		if v.Action != "" {
			t.Fatalf("round %d: %+v; each error differs, so the agent is learning something", i, v)
		}
	}
}

func TestLoopGuardIdleRounds(t *testing.T) {
	g := newLoopGuard(LoopLimits{IdleWarn: 2, IdleStop: 4})
	first := g.observe([]llm.Block{useCall("a", "process_poll", `{"id":"p1"}`)}, []llm.Block{result("a", "still running", false)}, time.Time{}, nil)
	if first.Action != "" {
		t.Fatalf("a new call is progress: %+v", first)
	}
	var got []string
	for i := 1; i <= 4; i++ {
		id := fmt.Sprint("r", i)
		v := g.observe([]llm.Block{useCall(id, "process_poll", `{"id":"p1"}`)}, []llm.Block{result(id, "still running", false)}, time.Time{}, nil)
		got = append(got, v.Action)
	}
	if want := []string{"", LoopWarn, "", LoopStop}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("actions = %q, want %q", got, want)
	}
}

func TestLoopGuardNewResultResetsIdle(t *testing.T) {
	g := newLoopGuard(LoopLimits{IdleWarn: 2, IdleStop: 3})
	g.observe([]llm.Block{useCall("a", "process_poll", `{}`)}, []llm.Block{result("a", "running", false)}, time.Time{}, nil)
	g.observe([]llm.Block{useCall("b", "process_poll", `{}`)}, []llm.Block{result("b", "running", false)}, time.Time{}, nil)
	g.observe([]llm.Block{useCall("c", "process_poll", `{}`)}, []llm.Block{result("c", "done", false)}, time.Time{}, nil) // something new
	v := g.observe([]llm.Block{useCall("d", "process_poll", `{}`)}, []llm.Block{result("d", "done", false)}, time.Time{}, nil)
	if v.Action != "" || g.idle != 1 {
		t.Fatalf("idle = %d, verdict %+v; a new result starts the count again", g.idle, v)
	}
}

func TestLoopGuardOff(t *testing.T) {
	if !newLoopGuard(LoopLimits{}).off() {
		t.Fatal("all zero limits should turn the guard off")
	}
}

// repeatModel calls read_file on a missing file forever, and records each
// request it got.
type repeatModel struct {
	mu   sync.Mutex
	n    int
	reqs []llm.Request
}

func (m *repeatModel) Send(_ context.Context, _ []llm.Target, req llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	m.reqs = append(m.reqs, req)
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{
		useCall(fmt.Sprint("call", m.n), "read_file", `{"path":"missing.txt"}`),
	}}, StopReason: llm.StopToolUse, Model: "fake/m"}, nil
}

type hookLog struct {
	mu     sync.Mutex
	events []LoopGuardPayload
}

func (h *hookLog) Fire(_ context.Context, event, _ string, payload any) string {
	if p, ok := payload.(LoopGuardPayload); ok && event == HookLoopGuard {
		h.mu.Lock()
		h.events = append(h.events, p)
		h.mu.Unlock()
	}
	return ""
}

func TestTurnStopsARepeatingLoop(t *testing.T) {
	ctx := context.Background()
	m := &repeatModel{}
	h := newHarness(t, m)
	spec := h.runner.Agents["habakkuk"]
	spec.MaxIterations = 50
	spec.Loop = LoopLimits{RepeatWarn: 2, RepeatStop: 4}
	hooks := &hookLog{}
	h.runner.Hooks = hooks
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	h.runner.Submit(ctx, s.ID, "read missing.txt")
	j, _ := h.queue.Claim(ctx, "w1", time.Minute, KindTurn)
	final, err := h.runner.HandleTurn(ctx, j, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(final, "going round in circles") || !strings.Contains(final, "read_file 4 times") {
		t.Fatalf("final = %q", final)
	}
	if m.n != 4 {
		t.Fatalf("model called %d times, want 4 (stopped at the 4th identical failure)", m.n)
	}
	// The third request carries the warning after the second failure.
	warned := false
	for _, b := range m.reqs[2].Messages[len(m.reqs[2].Messages)-1].Blocks {
		if b.Type == llm.Text && strings.Contains(b.Text, "Loop guard") {
			warned = true
		}
	}
	if !warned {
		t.Fatal("the model was not told it was repeating itself")
	}
	msgs := h.transcript(t, s.ID)
	if last := msgs[len(msgs)-1]; last.Role != llm.Assistant || last.Text() != final {
		t.Fatalf("transcript should end with the stop message, got %+v", last)
	}
	if len(hooks.events) != 2 || hooks.events[0].Action != LoopWarn || hooks.events[1].Action != LoopStop || hooks.events[1].Tool != "read_file" {
		t.Fatalf("hook events = %+v", hooks.events)
	}
}

func TestLoopGuardReplayKeepsCounts(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &repeatModel{})
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	recs := []struct {
		m   llm.Message
		job string
	}{
		{llm.Message{Role: llm.Assistant, Blocks: []llm.Block{useCall("a", "read_file", `{"path":"x"}`)}}, "j1"},
		{llm.Message{Role: llm.User, Blocks: []llm.Block{result("a", "missing", true)}}, "j1"},
		{llm.Message{Role: llm.Assistant, Blocks: []llm.Block{useCall("b", "read_file", `{"path":"x"}`)}}, "j1"},
		{llm.Message{Role: llm.User, Blocks: []llm.Block{result("b", "missing", true)}}, "j1"},
		{llm.Message{Role: llm.Assistant, Blocks: []llm.Block{useCall("c", "read_file", `{"path":"x"}`)}}, "other"},
		{llm.Message{Role: llm.User, Blocks: []llm.Block{result("c", "missing", true)}}, "other"},
	}
	for i, r := range recs {
		if err := h.sess.Append(ctx, s.ID, r.job, i, r.m, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	stored, _ := h.sess.Messages(ctx, s.ID)
	g := newLoopGuard(LoopLimits{RepeatWarn: 2, RepeatStop: 3})
	g.replay(stored, "j1")
	v := g.observe([]llm.Block{useCall("d", "read_file", `{"path":"x"}`)}, []llm.Block{result("d", "missing", true)}, time.Time{}, nil)
	if v.Action != LoopStop || v.Count != 3 {
		t.Fatalf("after replaying two failures, a third should stop: %+v", v)
	}
}

// Waiting for a server by trying it, doing something else (a sleep, a
// look at the log) and trying again is not a repeat; the idle check still
// bounds it.
func TestLoopGuardProbeWithWaitsIsNotARepeat(t *testing.T) {
	g := newLoopGuard(LoopLimits{RepeatWarn: 2, RepeatStop: 3, IdleWarn: 8, IdleStop: 12})
	curl := `{"command":"curl -s localhost:3000"}`
	for i := 1; i <= 6; i++ {
		probe := fmt.Sprint("p", i)
		v := g.observe([]llm.Block{useCall(probe, "exec", curl)}, []llm.Block{result(probe, "connection refused", true)}, time.Time{}, nil)
		if v.Action == LoopStop || (v.Action == LoopWarn && v.Check == LoopRepeat) {
			t.Fatalf("probe %d tripped the repeat check: %+v", i, v)
		}
		wait := fmt.Sprint("w", i)
		g.observe([]llm.Block{useCall(wait, "exec", `{"command":"sleep 5"}`)}, []llm.Block{result(wait, "", false)}, time.Time{}, nil)
	}
	// The same probe in one round with a wait also resets.
	g2 := newLoopGuard(LoopLimits{RepeatWarn: 2, RepeatStop: 3})
	for i := 1; i <= 5; i++ {
		a, b := fmt.Sprint("a", i), fmt.Sprint("b", i)
		v := g2.observe([]llm.Block{useCall(a, "exec", curl), useCall(b, "process_poll", `{"id":"srv"}`)},
			[]llm.Block{result(a, "connection refused", true), result(b, "starting", false)}, time.Time{}, nil)
		if v.Action != "" && v.Check == LoopRepeat {
			t.Fatalf("round %d: %+v", i, v)
		}
	}
}

// Tries far apart in time are waiting, not repeating.
func TestLoopGuardSlowRetriesAreNotARepeat(t *testing.T) {
	g := newLoopGuard(LoopLimits{RepeatWarn: 2, RepeatStop: 3})
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	for i := 1; i <= 5; i++ {
		id := fmt.Sprint("c", i)
		at = at.Add(repeatGap + time.Second)
		if v := g.observe([]llm.Block{useCall(id, "exec", `{"command":"make deploy"}`)}, []llm.Block{result(id, "server busy", true)}, at, nil); v.Action != "" {
			t.Fatalf("try %d: %+v", i, v)
		}
	}
	// Quick tries do count.
	for i := 1; i <= 3; i++ {
		id := fmt.Sprint("q", i)
		at = at.Add(2 * time.Second)
		v := g.observe([]llm.Block{useCall(id, "exec", `{"command":"make deploy"}`)}, []llm.Block{result(id, "server busy", true)}, at, nil)
		if i == 3 && v.Action != LoopStop {
			t.Fatalf("three quick tries: %+v", v)
		}
	}
}

// A run nobody watches (a schedule) that the guard stopped has failed.
func TestLoopStopFailsABackgroundTurn(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &repeatModel{})
	spec := h.runner.Agents["habakkuk"]
	spec.MaxIterations, spec.Loop = 50, LoopLimits{RepeatWarn: 2, RepeatStop: 3}
	home := &fakeHome{}
	h.runner.Home = home
	spec.Home, spec.ReportHome = "wa:creator", ReportResults
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	h.runner.SubmitTurn(ctx, Turn{Session: s.ID, Text: "read missing.txt", Background: true, Source: SourceSchedule, Task: `scheduled task "inbox"`})
	j, _ := h.queue.Claim(ctx, "w1", time.Minute, KindTurn)
	_, err := h.runner.HandleTurn(ctx, j, "w1")
	var stopped LoopStopped
	if !errors.As(err, &stopped) || !errors.As(err, new(jobs.Permanent)) || !strings.Contains(stopped.Message, "going round in circles") {
		t.Fatalf("err = %v", err)
	}
	// The home chat still hears about it, with the whole message.
	if got := home.take(); len(got) != 1 || got[0].Final != stopped.Message {
		t.Fatalf("home reports %+v", got)
	}
}

// A poll that waits inside its own command (sleep 10 && curl) and so took
// ten seconds is waiting, not repeating; so is any call that took several
// seconds to fail.
func TestLoopGuardWaitingCallsAreNotRepeats(t *testing.T) {
	g := newLoopGuard(LoopLimits{RepeatWarn: 3, RepeatStop: 5, IdleWarn: 8, IdleStop: 12})
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	cmd := `{"command":"sleep 10 && curl -sf localhost:3000/health"}`
	for i := 1; i <= 30; i++ {
		id := fmt.Sprint("p", i)
		at = at.Add(12 * time.Second)
		if v := g.observe([]llm.Block{useCall(id, "exec", cmd)}, []llm.Block{result(id, "exit 7: connection refused", true)}, at,
			map[string]time.Duration{id: 10 * time.Second}); v.Action != "" {
			t.Fatalf("a 10s-sleep poll was flagged at try %d: %+v", i, v)
		}
	}
	slow := newLoopGuard(LoopLimits{RepeatWarn: 2, RepeatStop: 3, IdleWarn: 8, IdleStop: 12})
	for i := 1; i <= 10; i++ {
		id := fmt.Sprint("s", i)
		v := slow.observe([]llm.Block{useCall(id, "http_get", `{"url":"http://localhost:3000"}`)}, []llm.Block{result(id, "timed out", true)},
			time.Time{}, map[string]time.Duration{id: 8 * time.Second})
		if v.Action != "" {
			t.Fatalf("a call that waited 8s to fail was flagged at try %d: %+v", i, v)
		}
	}
	// Genuinely identical instant failures still stop.
	fast := newLoopGuard(LoopLimits{RepeatWarn: 2, RepeatStop: 3})
	var last loopVerdict
	for i := 1; i <= 3; i++ {
		id := fmt.Sprint("f", i)
		last = fast.observe([]llm.Block{useCall(id, "exec", `{"command":"curl -sf localhost:3000"}`)}, []llm.Block{result(id, "refused", true)},
			time.Time{}, map[string]time.Duration{id: 50 * time.Millisecond})
	}
	if last.Action != LoopStop {
		t.Fatalf("instant identical failures not stopped: %+v", last)
	}
}

// Rounds that only wait do not count as idle: a probe, a sleep, a probe
// gets twelve probes, not six.
func TestLoopGuardWaitRoundsAreNotIdle(t *testing.T) {
	g := newLoopGuard(LoopLimits{RepeatWarn: 3, RepeatStop: 5, IdleWarn: 8, IdleStop: 12})
	stoppedAt := 0
	for i := 1; i <= 20 && stoppedAt == 0; i++ {
		p, w := fmt.Sprint("p", i), fmt.Sprint("w", i)
		if v := g.observe([]llm.Block{useCall(p, "exec", `{"command":"curl -sf localhost:3000"}`)}, []llm.Block{result(p, "refused", true)}, time.Time{}, nil); v.Action == LoopStop {
			stoppedAt = i
			break
		}
		if v := g.observe([]llm.Block{useCall(w, "exec", `{"command":"sleep 30"}`)}, []llm.Block{result(w, "", false)}, time.Time{},
			map[string]time.Duration{w: 30 * time.Second}); v.Action == LoopStop {
			t.Fatalf("a sleep round stopped the turn at %d: %+v", i, v)
		}
	}
	if stoppedAt != 13 { // probe 1 is new; probes 2..13 are the twelve idle rounds
		t.Fatalf("stopped at probe %d, want 13", stoppedAt)
	}
}

// Only time actually spent counts as waiting: an instant failure is a
// repeat whatever its command or tool name says.
func TestLoopGuardWaitWordsDoNotExemptInstantFailures(t *testing.T) {
	for _, c := range []struct{ tool, input string }{
		{"exec", `{"command":"timeout 120 ./deploy.sh"}`},
		{"exec", `{"command":"npm run watch"}`},
		{"exec", `{"command":"curl --retry 5 localhost:3000"}`},
		{"exec", `{"command":"sleep 10 && curl localhost:3000"}`}, // replayed or killed at once: no time spent
		{"process_wait", `{"id":"no-such-process"}`},
	} {
		g := newLoopGuard(LoopLimits{RepeatWarn: 2, RepeatStop: 3})
		var v loopVerdict
		for i := 1; i <= 3; i++ {
			id := fmt.Sprint("c", i)
			v = g.observe([]llm.Block{useCall(id, c.tool, c.input)}, []llm.Block{result(id, "failed at once", true)},
				time.Time{}, map[string]time.Duration{id: 30 * time.Millisecond})
		}
		if v.Action != LoopStop {
			t.Errorf("%s %s: three instant identical failures were not stopped: %+v", c.tool, c.input, v)
		}
	}
}

// slowHooks takes its time over tool_before, as a hook or an approval
// can.
type slowHooks struct{ d time.Duration }

func (h slowHooks) Fire(_ context.Context, event, _ string, _ any) string {
	if event == HookToolBefore {
		time.Sleep(h.d)
	}
	return ""
}

// Time spent before the tool runs (a tool_before hook, an approval) is not
// the tool waiting: a call that fails at once is still a repeat.
func TestLoopGuardTimesOnlyTheToolsOwnRun(t *testing.T) {
	old := waitAfter
	waitAfter = 50 * time.Millisecond
	t.Cleanup(func() { waitAfter = old })
	ctx := context.Background()
	h := newHarness(t, &repeatModel{})
	spec := h.runner.Agents["habakkuk"]
	spec.MaxIterations, spec.Loop = 20, LoopLimits{RepeatWarn: 2, RepeatStop: 3}
	h.runner.Hooks = slowHooks{d: 120 * time.Millisecond}
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	h.runner.Submit(ctx, s.ID, "read missing.txt")
	j, _ := h.queue.Claim(ctx, "w1", time.Minute, KindTurn)
	final, err := h.runner.HandleTurn(ctx, j, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(final, "going round in circles") {
		t.Fatalf("a slow hook made instant failures look like waiting: %q", final)
	}
}
