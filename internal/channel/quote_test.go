package channel

import (
	"context"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// A reply that quotes a message reaches the agent with the quoted text.
func TestQuotedReplyReachesTheAgent(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"*"}})
	ctx := context.Background()
	m := dm("m1", "yes")
	m.Quote = &Quote{Self: true, Text: "Tests pass.\nMerge it?"}
	r.g.Handle(ctx, m)
	waitFor(t, "reply", hasText(r.a, `echo: [Replying to you: "Tests pass. Merge it?"]`+"\nyes"))

	g := Message{Account: "tg", Conversation: "-5", ID: "g1", Sender: "8", SenderName: "Bo", Text: "agreed",
		Mentioned: true, Quote: &Quote{From: "Cy", Text: strings.Repeat("long ", 200)}}
	r.g.Handle(ctx, g)
	waitFor(t, "group reply", hasText(r.a, "...\"]\nBo: agreed"))
}

func TestQuoteLine(t *testing.T) {
	for _, c := range []struct {
		q    *Quote
		want string
	}{
		{nil, ""},
		{&Quote{From: "Ada"}, ""},
		{&Quote{From: "Ada", Text: " hi  there "}, `[Replying to Ada: "hi there"]`},
		{&Quote{Self: true, From: "bot", Text: "done"}, `[Replying to you: "done"]`},
		{&Quote{Text: "(photo)"}, `[Replying to an earlier message: "(photo)"]`},
	} {
		if got := quoteLine(c.q); got != c.want {
			t.Errorf("quoteLine(%+v) = %q, want %q", c.q, got, c.want)
		}
	}
	if got := quoteLine(&Quote{Text: strings.Repeat("x", 2000)}); len([]rune(got)) > maxQuote+60 {
		t.Errorf("long quote not trimmed: %d runes", len([]rune(got)))
	}
}
