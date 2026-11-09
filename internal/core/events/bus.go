// Package events fans out live turn activity to attached clients (CLI,
// channels, web). Delivery is best effort; the transcript is the source of
// truth, so a slow or absent subscriber never blocks a turn.
package events

import (
	"encoding/json"
	"sync"
)

type Kind string

const (
	Text      Kind = "text"
	Thinking  Kind = "thinking"
	ToolStart Kind = "tool_start"
	ToolDone  Kind = "tool_done"
	Retry     Kind = "retry"
	Review    Kind = "review" // an approval reviewer's decision on a tool call
	TurnDone  Kind = "turn_done"
	TurnError Kind = "turn_error"
)

type Event struct {
	Session string
	Kind    Kind
	Text    string
	IsError bool

	// Tool events also name the call, so a client can pair a start with its
	// result when calls run in parallel.
	ToolID string
	Tool   string
	Input  json.RawMessage // ToolStart only: the call's full input
}

type Bus struct {
	mu   sync.Mutex
	subs map[string]map[chan Event]struct{}
}

func NewBus() *Bus { return &Bus{subs: map[string]map[chan Event]struct{}{}} }

// Subscribe returns a channel of events for one session and a cancel func.
func (b *Bus) Subscribe(session string) (<-chan Event, func()) {
	ch := make(chan Event, 256)
	b.mu.Lock()
	if b.subs[session] == nil {
		b.subs[session] = map[chan Event]struct{}{}
	}
	b.subs[session][ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs[session], ch)
		if len(b.subs[session]) == 0 {
			delete(b.subs, session)
		}
		b.mu.Unlock()
	}
}

func (b *Bus) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[e.Session] {
		select {
		case ch <- e:
		default: // drop rather than block the turn
		}
	}
}
