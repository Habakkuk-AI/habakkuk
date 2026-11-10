package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// stuck calls read_file on a missing file forever.
type stuck struct {
	mu sync.Mutex
	n  int
}

func (s *stuck) Send(context.Context, []llm.Target, llm.Request, func(llm.Event)) (*llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{
		{Type: llm.ToolUse, ToolUseID: fmt.Sprint("c", s.n), ToolName: "read_file", Input: json.RawMessage(`{"path":"missing.txt"}`)},
	}}, StopReason: llm.StopToolUse}, nil
}

// A scheduled run the loop guard stopped counts as failed, and its chat
// gets the guard's whole message, not a cut "turn failed" notice.
func TestLoopStopReachesTheChatWhole(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"*"}})
	ctx := context.Background()
	spec := r.g.Runner.Agents["habakkuk"]
	spec.Loop = agent.LoopLimits{RepeatWarn: 2, RepeatStop: 3}
	r.g.Runner.Models = &stuck{}
	sess, err := r.g.Runner.NewSession(ctx, "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := json.Marshal(Address{Account: "tg", Conversation: "100"})
	if _, err := r.g.Runner.SubmitTurn(ctx, agent.Turn{Session: sess.ID, Text: "read missing.txt", Origin: origin,
		Background: true, Source: agent.SourceSchedule}); err != nil {
		t.Fatal(err)
	}
	r.g.Wake()
	var got string
	waitFor(t, "the stop message", func() bool {
		r.a.mu.Lock()
		defer r.a.mu.Unlock()
		for _, s := range r.a.sent {
			if strings.Contains(s.Text, "going round in circles") {
				got = s.Text
				return true
			}
		}
		return false
	})
	if !strings.HasPrefix(got, "I stopped working on this") || !strings.HasSuffix(got, "Tell me how you'd like me to go on.") {
		t.Fatalf("chat got %q", got)
	}
}
