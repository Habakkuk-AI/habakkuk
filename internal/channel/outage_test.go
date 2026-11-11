package channel

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// broke refuses every call the way a router does when every provider in
// the chain is out of credit, and counts the calls. It keeps its own
// notice record, like llm.Router.
type broke struct {
	mu    sync.Mutex
	calls int
	told  map[string]bool
}

func (b *broke) Send(context.Context, []llm.Target, llm.Request, func(llm.Event)) (*llm.Response, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	return nil, &llm.UnavailableError{
		Message: "no model could answer: deepseek is out of credit. Top up or fix the key for one of them, or add a model you can use to this agent's fallbacks in config.yaml.",
		Key:     "unavailable:deepseek=billing",
	}
}

func (b *broke) UnavailableNotice(key, where string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.told[key+" "+where] {
		return false
	}
	b.told[key+" "+where] = true
	return true
}

// When no model can answer because of credit, a schedule's chat hears it
// once, in plain words, and the job isn't retried; later runs stay quiet.
// A message the user sent always gets the answer.
func TestOutageToldOncePerScheduleChat(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"*"}})
	ctx := context.Background()
	b := &broke{told: map[string]bool{}}
	r.g.Runner.Models = b
	sess, err := r.g.Runner.NewSession(ctx, "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := json.Marshal(Address{Account: "tg", Conversation: "100"})
	failures := func() (n int, last string) {
		r.a.mu.Lock()
		defer r.a.mu.Unlock()
		for _, s := range r.a.sent {
			if strings.Contains(s.Text, "out of credit") {
				n++
				last = s.Text
			}
		}
		return n, last
	}
	run := func(text string, background bool) {
		t.Helper()
		turn := agent.Turn{Session: sess.ID, Text: text, Origin: origin}
		if background {
			turn.Background, turn.Source = true, agent.SourceSchedule
		}
		if _, err := r.g.Runner.SubmitTurn(ctx, turn); err != nil {
			t.Fatal(err)
		}
		r.g.Wake()
	}

	run("morning check", true)
	waitFor(t, "the outage notice", func() bool { n, _ := failures(); return n == 1 })
	_, got := failures()
	if !strings.Contains(got, "no model could answer: deepseek is out of credit") {
		t.Fatalf("chat got %q", got)
	}
	b.mu.Lock()
	if b.calls != 1 {
		t.Errorf("the job ran %d times; an outage should not be retried", b.calls)
	}
	b.mu.Unlock()

	run("morning check", true)
	waitFor(t, "the second run", func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.calls == 2 })
	time.Sleep(200 * time.Millisecond) // a notice would be queued by now
	if n, _ := failures(); n != 1 {
		t.Fatalf("the schedule's chat got %d outage notices, want 1", n)
	}

	run("are you there?", false)
	waitFor(t, "the reply to the user's own message", func() bool { n, _ := failures(); return n == 2 })
}

// Each chat a schedule reports to is told about the outage once: a second
// schedule's chat isn't left out because the first was told.
func TestOutageToldToEachChat(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"*"}})
	ctx := context.Background()
	b := &broke{told: map[string]bool{}}
	r.g.Runner.Models = b
	sess, err := r.g.Runner.NewSession(ctx, "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	for _, chat := range []string{"100", "200", "100"} {
		origin, _ := json.Marshal(Address{Account: "tg", Conversation: chat})
		if _, err := r.g.Runner.SubmitTurn(ctx, agent.Turn{Session: sess.ID, Text: "check", Origin: origin,
			Background: true, Source: agent.SourceSchedule}); err != nil {
			t.Fatal(err)
		}
		r.g.Wake()
	}
	waitFor(t, "all three runs", func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.calls == 3 })
	time.Sleep(200 * time.Millisecond)
	got := map[string]int{}
	r.a.mu.Lock()
	for _, s := range r.a.sent {
		if strings.Contains(s.Text, "out of credit") {
			got[s.To.Conversation]++
		}
	}
	r.a.mu.Unlock()
	if got["100"] != 1 || got["200"] != 1 {
		t.Fatalf("notices per chat = %v, want one each", got)
	}
}
