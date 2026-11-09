package channel

import (
	"context"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// A chat app's first-contact /start reaches an agent that has not met its
// user, whose prompt carries the welcome; once it has, /start is help again.
func TestStartOpensTheWelcome(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	send(r, "m1", "/start")
	waitFor(t, "welcome turn", hasText(r.a, "echo: /start"))
	r.model.mu.Lock()
	system := r.model.last.System
	r.model.mu.Unlock()
	if !strings.Contains(system, "# Welcome a new user") {
		t.Fatalf("first chat prompt has no welcome:\n%s", system)
	}
	send(r, "m2", "/start")
	waitFor(t, "help", hasText(r.a, "/new"))

	// A group message never opens the welcome.
	g := newRig(t, &config.Channel{AllowFrom: []string{"7"}, Groups: "all"})
	g.g.Handle(context.Background(), Message{Account: "tg", Conversation: "-5", ID: "g1", Sender: "7", SenderName: "Ay", Text: "hello all"})
	waitFor(t, "group reply", hasText(g.a, "hello all"))
	g.model.mu.Lock()
	system = g.model.last.System
	g.model.mu.Unlock()
	if strings.Contains(system, "Welcome a new user") {
		t.Fatal("a group chat got the welcome")
	}
}
