package channel

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// withBuilder adds an agent "builder" with no home chat, and gives
// "habakkuk" the rig's chat as its home when home is set. Sub-agent tasks
// run on the runner, as in the app.
func withBuilder(home string) func(*rig) {
	return func(r *rig) {
		r.g.Runner.Agents["habakkuk"].Home = home
		b := *r.g.Runner.Agents["habakkuk"]
		b.Name, b.Home = "builder", ""
		r.g.Runner.Agents["builder"] = &b
		r.pool.Handlers[agent.KindSubturn] = r.g.Runner.HandleTurn
	}
}

// unattendedTurn queues work for agentName in a fresh session with no
// reply address: nobody watches it where it started.
func unattendedTurn(t *testing.T, r *rig, tn agent.Turn, agentName string) {
	t.Helper()
	sess, err := r.g.Runner.NewSession(context.Background(), agentName)
	if err != nil {
		t.Fatal(err)
	}
	tn.Session, tn.Text, tn.Background = sess.ID, "run it", true
	if _, err := r.g.Runner.SubmitTurn(context.Background(), tn); err != nil {
		t.Fatal(err)
	}
	r.g.Wake()
}

func boardCard(from string) agent.Turn {
	return agent.Turn{Source: agent.SourceBoard, Task: `card crd_1 "Build the report"`, From: from}
}

func TestBoardCardAsksInItsAgentsHome(t *testing.T) {
	r := openStateRig(t, t.TempDir(), &config.Channel{AllowFrom: []string{"7"}}, withBuilder("tg:100"))
	unattendedTurn(t, r, boardCard(""), "habakkuk")
	waitFor(t, "approval prompt", hasText(r.a, "I'd like to run a command on your computer."))
	waitFor(t, "who asks", hasText(r.a, "habakkuk is asking, for work it does in the background."))
	waitFor(t, "task line", hasText(r.a, `This is for your card crd 1 "Build the report".`))
	send(r, "m1", "yes")
	waitFor(t, "tool output", modelSaw(r, "approved-run"))
}

// A worker with no home asks in the home of the agent that made its card.
func TestBoardCardAsksInItsCreatorsHome(t *testing.T) {
	r := openStateRig(t, t.TempDir(), &config.Channel{AllowFrom: []string{"7"}}, withBuilder("tg:100"))
	unattendedTurn(t, r, boardCard("habakkuk"), "builder")
	waitFor(t, "who asks", hasText(r.a, "builder is asking, for work habakkuk handed to it."))
	send(r, "m1", "no")
	waitFor(t, "denial", modelSaw(r, "the user declined exec"))
}

// A delegated task asks in the home of the agent that delegated it.
func TestDelegatedTaskAsksInTheDelegatorsHome(t *testing.T) {
	r := openStateRig(t, t.TempDir(), &config.Channel{AllowFrom: []string{"7"}}, withBuilder("tg:100"))
	parent, err := r.g.Runner.NewSession(context.Background(), "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	unattendedTurn(t, r, agent.Turn{Sub: &agent.SubturnSpec{Parent: parent.ID, Task: "Build the report"}}, "builder")
	waitFor(t, "who asks", hasText(r.a, "builder is asking, for work habakkuk handed to it."))
	waitFor(t, "task line", hasText(r.a, "This is for your task: Build the report."))
	send(r, "m1", "yes")
	waitFor(t, "tool output", modelSaw(r, "approved-run"))
}

// With no home anywhere, the denial says what would help.
func TestUnattendedWithoutHomeSaysWhatHelps(t *testing.T) {
	r := openStateRig(t, t.TempDir(), &config.Channel{AllowFrom: []string{"7"}}, withBuilder(""))
	unattendedTurn(t, r, boardCard("habakkuk"), "builder")
	waitFor(t, "denial", modelSaw(r, "a yes typed in another chat cannot reach this call"))
	if hasText(r.a, "I'd like to run a command")() {
		t.Fatal("asked with no home chat")
	}
}

// An A2A task is another system's request: it never asks in a home chat.
func TestA2ATaskDoesNotAskAtHome(t *testing.T) {
	r := openStateRig(t, t.TempDir(), &config.Channel{AllowFrom: []string{"7"}}, withBuilder("tg:100"))
	unattendedTurn(t, r, agent.Turn{Source: agent.SourceA2A, Task: "task from a peer"}, "habakkuk")
	waitFor(t, "denial", modelSaw(r, "no one is attending this run"))
	if hasText(r.a, "I'd like to run a command")() {
		t.Fatal("an A2A task asked in the home chat")
	}
}

// modelSaw reports whether the model's last request carried sub: the
// result of a tool call in work that has no chat of its own to answer.
func modelSaw(r *rig, sub string) func() bool {
	return func() bool {
		r.model.mu.Lock()
		defer r.model.mu.Unlock()
		return strings.Contains(fmt.Sprintf("%+v", r.model.last.Messages), sub)
	}
}
