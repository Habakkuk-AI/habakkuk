package channel

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// Approvals across a restart. A pending approval and its buttons live in
// the memory of the process that asked. These tests stop that process the
// way SIGTERM does and start a new one on the same databases.

// asker asks to run a command, and asks again when its call comes back
// interrupted, as a model does after a restart. Once the call has a real
// result it reports it.
func asker(command func() string) *stepModel {
	return &stepModel{fn: func(_ llm.Request, last llm.Message) *llm.Response {
		for _, b := range last.Blocks {
			if b.Type == llm.ToolResult && !strings.HasPrefix(b.Text, "interrupted:") {
				return text("tool said: " + b.Text)
			}
		}
		return toolCall("call", "exec", `{"command":"`+command()+`"}`)
	}}
}

// restartWithPrompt runs a turn until its approval prompt is in the chat,
// shuts the process down, and starts the next one. It returns the first
// process's buttons and the new rig, whose model asks for nextCommand after
// modelDelay.
func restartWithPrompt(t *testing.T, nextCommand string, modelDelay time.Duration) (old [][]Button, r2 *rig, a2 *rich) {
	t.Helper()
	ch := func() *config.Channel { return &config.Channel{AllowFrom: []string{"7"}} }
	dir := t.TempDir()
	r1 := openStateRig(t, dir, ch())
	a1 := &rich{recorder: r1.a}
	r1.g.Adapters["tg"] = a1
	r1.g.Runner.Models = asker(func() string { return "echo approved-run" })
	send(r1, "m1", "please run it")
	waitFor(t, "the first prompt", func() bool { return a1.lastRows() != nil })
	old = a1.lastRows()
	jobID := TurnID(dm("m1", ""))
	r1.stop()
	if j, err := r1.q.Get(context.Background(), jobID); err != nil || j.Status != "pending" || j.Attempts != 0 {
		t.Fatalf("after the shutdown the turn is %+v, %v; want pending with no attempt used", j, err)
	}

	r2 = openStateRig(t, dir, ch(), func(r *rig) {
		a2 = &rich{recorder: r.a}
		r.g.Adapters["tg"] = a2
		m := asker(func() string { return nextCommand })
		m.delay = modelDelay
		r.g.Runner.Models = m
	})
	return old, r2, a2
}

// The resumed turn reaches the same call and asks again, with a fresh
// prompt; a press on the prompt from before the restart answers it,
// because it is the same call.
func TestApprovalIsAskedAgainAfterARestart(t *testing.T) {
	old, r2, a2 := restartWithPrompt(t, "echo approved-run", 0)
	waitFor(t, "a fresh prompt after the restart", func() bool { return a2.lastRows() != nil })
	prompts, _, _ := a2.snapshot()
	if len(prompts) != 1 || !strings.HasPrefix(prompts[0], "I'd like to run a command on your computer.") {
		t.Fatalf("prompts after the restart: %q", prompts)
	}
	fresh := a2.lastRows()
	if fresh[0][0].Data == old[0][0].Data {
		t.Fatal("the fresh prompt reuses the old prompt's button data")
	}
	if hasText(r2.a, "approved-run\n")() {
		t.Fatal("the command ran after the restart without an answer")
	}
	press(r2, "8", old[0][0].Data) // not allowed to talk: ignored, as ever
	time.Sleep(50 * time.Millisecond)
	if hasText(r2.a, "approved-run\n")() {
		t.Fatal("a stranger's press approved the command")
	}
	press(r2, "7", old[0][0].Data)
	waitFor(t, "the old button to approve the same call", hasText(r2.a, "approved-run\n"))
	waitFor(t, "the acknowledgement", hasText(r2.a, "Approved."))
	// The fresh prompt's own button now finds it answered, and says so.
	press(r2, "7", fresh[0][1].Data)
	waitFor(t, "a notice for the second press", hasText(r2.a, "That request has expired or was already answered."))
}

// An old button never answers a different call: the chat is told the
// request expired and pointed at the one waiting now, which stays open.
func TestOldButtonDoesNotAnswerADifferentCall(t *testing.T) {
	old, r2, a2 := restartWithPrompt(t, "echo something-else", 0)
	waitFor(t, "a fresh prompt after the restart", func() bool { return a2.lastRows() != nil })
	press(r2, "7", old[0][0].Data)
	waitFor(t, "the expired notice", hasText(r2.a, "That request has expired. Please answer my latest request instead."))
	time.Sleep(50 * time.Millisecond)
	if hasText(r2.a, "something-else\n")() || hasText(r2.a, "Approved.")() {
		t.Fatalf("an old button approved a different call: %q", r2.a.texts())
	}
	to := Address{Account: "tg", Conversation: "100"}
	if _, waiting := r2.g.pending.Load(to.key()); !waiting {
		t.Fatal("the new approval is no longer waiting")
	}
	// A button from before buttons carried a hash is treated the same way.
	nonce := strings.Split(old[0][0].Data, "|")[1]
	press(r2, "7", callbackPrefix+nonce+"|0")
	time.Sleep(50 * time.Millisecond)
	if hasText(r2.a, "something-else\n")() {
		t.Fatal("a button without a hash approved the call")
	}
	send(r2, "m2", "yes")
	waitFor(t, "the new call to run on a typed yes", hasText(r2.a, "something-else\n"))
}

// With nothing waiting, a press or a typed yes for the old prompt is
// answered, never dropped and never taken as a new message.
func TestAnswersToAPromptFromBeforeTheRestartAreNotDropped(t *testing.T) {
	// The new process has not got back to the call yet: its model is slow.
	old, r2, a2 := restartWithPrompt(t, "echo approved-run", time.Hour)
	press(r2, "7", old[0][1].Data)
	waitFor(t, "a notice for the old button", hasText(r2.a, "That request has expired or was already answered."))
	send(r2, "m2", "Yes.")
	waitFor(t, "a notice for the typed yes", hasText(r2.a, "That request expired when I restarted. If I still need it, I'll ask again."))
	time.Sleep(50 * time.Millisecond)
	if a2.lastRows() != nil || hasText(r2.a, "approved-run\n")() {
		t.Fatal("the old answer approved something")
	}
	// The yes was not queued as a message for the agent.
	conv, _ := r2.g.Store.Conversation(context.Background(), Address{Account: "tg", Conversation: "100"})
	jobs, _ := r2.q.Active(context.Background(), "turn")
	for _, j := range jobs {
		if j.SerialKey == conv.SessionID && j.ID != TurnID(dm("m1", "")) {
			t.Fatalf("the typed yes became a new turn: %+v", j)
		}
	}
	// One notice is enough: after it, yes is an ordinary word again.
	if _, asked, _ := r2.g.State.Asked(context.Background(), Address{Account: "tg", Conversation: "100"}.key()); asked {
		t.Fatal("the expired prompt is still on record after its notice")
	}
}

// Without a restart nothing is on record once a prompt is answered, so a
// later "yes" is an ordinary message for the agent.
func TestYesIsAnOrdinaryMessageWhenNothingWasAsked(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	send(r, "m1", "please run it")
	waitFor(t, "prompt", hasText(r.a, "I'd like to run a command on your computer."))
	send(r, "m2", "yes")
	waitFor(t, "tool output", hasText(r.a, "approved-run\n"))
	send(r, "m3", "yes")
	waitFor(t, "the yes to reach the agent", hasText(r.a, "echo: yes"))
	if hasText(r.a, "expired")() {
		t.Fatalf("an answered prompt was reported as expired: %q", r.a.texts())
	}
}
