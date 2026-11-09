package channel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// fakeHistory is a MemoryCommands with history commands.
type fakeHistory struct{ fakeMemory }

func (f *fakeHistory) MemoryHistory(_ context.Context, agent string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.said = append(f.said, "history "+agent)
	return "- `ab12cd34` note coffee updated by agent", nil
}

func (f *fakeHistory) UndoMemory(_ context.Context, agent, sessionID, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.said = append(f.said, "undo "+agent+" "+id)
	if id == "zzzz" {
		return "", errors.New("no change zzzz")
	}
	if sessionID == "" {
		return "", errors.New("no session")
	}
	return "Restored note coffee", nil
}

func TestMemoryHistoryCommands(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	// A memory backend without history: the commands say so.
	r.g.Memory = &fakeMemory{}
	send(r, "m0", "/memory")
	waitFor(t, "unavailable", hasText(r.a, "Memory isn't available here."))

	mem := &fakeHistory{}
	r.g.Memory = mem
	send(r, "m1", "/remember I take my coffee black") // starts the session
	waitFor(t, "remember", hasText(r.a, "Saved I take my coffee black"))
	send(r, "m2", "/memory")
	waitFor(t, "history", hasText(r.a, "`ab12cd34` note coffee updated by agent"))
	send(r, "m3", "/undo-memory ab12cd34")
	waitFor(t, "undo", hasText(r.a, "Restored note coffee"))
	send(r, "m4", "/undo-memory zzzz")
	waitFor(t, "undo error", hasText(r.a, "Couldn't undo that: no change zzzz"))
	send(r, "m5", "/undo-memory")
	waitFor(t, "usage", hasText(r.a, "Usage: /undo-memory <id>"))
	mem.mu.Lock()
	defer mem.mu.Unlock()
	if got := strings.Join(mem.said, "|"); got != "remember habakkuk I take my coffee black|history habakkuk|undo habakkuk ab12cd34|undo habakkuk zzzz" {
		t.Fatalf("calls: %s", got)
	}
	send(r, "m6", "/help")
	waitFor(t, "help", hasText(r.a, "/undo-memory <id>"))
}
