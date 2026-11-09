package channel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

type fakeMemory struct {
	mu   sync.Mutex
	said []string
}

func (f *fakeMemory) Remember(_ context.Context, agent, sessionID, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sessionID == "" {
		return "", errors.New("no session")
	}
	f.said = append(f.said, "remember "+agent+" "+text)
	return "Saved " + text, nil
}

func (f *fakeMemory) Forget(_ context.Context, agent, query string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.said = append(f.said, "forget "+agent+" "+query)
	if query == "nothing" {
		return "", errors.New("no memory matches")
	}
	return "Forgot " + query, nil
}

func TestRememberAndForgetCommands(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	send(r, "m0", "/remember I take my coffee black")
	waitFor(t, "unavailable", hasText(r.a, "Memory isn't available here."))

	mem := &fakeMemory{}
	r.g.Memory = mem
	send(r, "m1", "/remember I take my coffee black")
	waitFor(t, "remember", hasText(r.a, "Saved I take my coffee black"))
	send(r, "m2", "/forget coffee")
	waitFor(t, "forget", hasText(r.a, "Forgot coffee"))
	send(r, "m3", "/forget nothing")
	waitFor(t, "forget error", hasText(r.a, "Couldn't forget that: no memory matches"))
	send(r, "m4", "/remember")
	waitFor(t, "usage", hasText(r.a, "Usage: /remember <text>"))
	mem.mu.Lock()
	defer mem.mu.Unlock()
	if len(mem.said) != 3 || !strings.HasPrefix(mem.said[0], "remember habakkuk I take") || mem.said[1] != "forget habakkuk coffee" {
		t.Fatalf("calls: %q", mem.said)
	}
	// Commands never reach the agent as a turn.
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	if r.model.calls != 0 {
		t.Fatalf("model called %d times", r.model.calls)
	}
}

func TestHelpListsMemoryCommands(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	send(r, "m1", "/help")
	waitFor(t, "help", hasText(r.a, "/remember <text>"))
	if !strings.Contains(r.a.texts()[0], "/forget") {
		t.Fatal("/help lacks /forget")
	}
}
