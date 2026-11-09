package consolidate

import (
	"context"

	"github.com/habakkuk-ai/habakkuk/internal/llm/judge"
)

var worthQ = judge.Yes(`Does this conversation contain anything worth remembering for later conversations: facts about the user, their preferences, people, places, projects, decisions, commitments, or how they like things done? Small talk, one-off questions and answers that will not matter later are not worth remembering.`)

// skipBoring reports whether the judge is confident a session holds nothing
// worth remembering, so its review can be skipped. Without a judge, or when
// it fails, the review goes ahead.
func (c *Consolidator) skipBoring(ctx context.Context, session, transcript string) bool {
	if c.Judge == nil {
		return false
	}
	below := c.SkipBelow
	if below <= 0 || below >= 1 {
		below = 0.15
	}
	ans, err := c.Judge.Ask(ctx, cutText(transcript, 50_000), map[string]judge.Question{"worth": worthQ})
	if err != nil {
		return false
	}
	if p := ans["worth"].P; p < below {
		c.log().Info("memory review skipped: nothing worth remembering", "session", session, "p", p)
		return true
	}
	return false
}
