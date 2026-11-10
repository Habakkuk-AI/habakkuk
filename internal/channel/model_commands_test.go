package channel

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

func TestExtraCommands(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ctx := context.Background()
	var gotSession, gotArg string
	r.g.Commands = map[string]AppCommand{"/usage": {Help: "/usage: tokens and cost", Run: func(_ context.Context, session, arg string) string {
		gotSession, gotArg = session, arg
		return "12 calls, $0.40"
	}}}
	r.g.Handle(ctx, dm("m1", "/usage  today "))
	waitFor(t, "command reply", hasText(r.a, "12 calls, $0.40"))
	conv, _ := r.g.Store.Conversation(ctx, Address{Account: "tg", Conversation: "100"})
	if conv == nil || gotSession != conv.SessionID || gotArg != "today" {
		t.Fatalf("session %q (conversation %+v) arg %q", gotSession, conv, gotArg)
	}
	if r.model.calls != 0 {
		t.Fatal("a command reached the model")
	}
	r.g.Handle(ctx, dm("m2", "/help"))
	waitFor(t, "help lists the command", hasText(r.a, "/usage: tokens and cost"))
}

func TestPromptCommandRunsAsATurn(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ctx := context.Background()
	r.g.Commands = map[string]AppCommand{
		"/skill": {Help: "/skill <name>: run a skill", Prompt: func(_ context.Context, _, arg string) (string, error) {
			if arg == "" {
				return "", errors.New("Usage: /skill <name>")
			}
			return "[Skill " + arg + "] do the thing", nil
		}},
	}
	r.g.Handle(ctx, dm("m1", "/skill"))
	waitFor(t, "usage reply", hasText(r.a, "Usage: /skill <name>"))
	r.g.Handle(ctx, dm("m2", "/skill report"))
	waitFor(t, "turn reply", hasText(r.a, "[Skill report] do the thing"))
	if !Reserved("/retry") || !Reserved("/new") || Reserved("/standup") {
		t.Fatal("reserved names")
	}
}

func TestOwnerOnlyCommandRefusesGuests(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7", "*"}})
	ctx := context.Background()
	var ran atomic.Int32
	r.g.Commands = map[string]AppCommand{"/retro": {Help: "/retro: look back", OwnerOnly: true, Run: func(context.Context, string, string) string {
		ran.Add(1)
		return "retro done"
	}}}
	guest := dm("m1", "/retro")
	guest.Sender = "8"
	r.g.Handle(ctx, guest)
	waitFor(t, "refusal", hasText(r.a, "/retro: only the owner can do that here."))
	r.g.Handle(ctx, dm("m2", "/retro"))
	waitFor(t, "owner reply", hasText(r.a, "retro done"))
	if ran.Load() != 1 {
		t.Fatalf("ran %d times", ran.Load())
	}
}
