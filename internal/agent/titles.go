package agent

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

const titleSystem = `You name conversations. Given the start of a conversation between a user and their assistant, reply with a short title for it: three to six words, in the user's language, no quotes, no final period, no emoji. Reply with the title only.`

// autoTitle gives a session a short title after its first turn, from one
// cheap model call, unless it has a better one already (a model's, or one
// the user set). It runs in the background so the reply is not held up;
// failures are logged and the first message stays the title.
func (r *Runner) autoTitle(ctx context.Context, sessionID string, spec *Spec, user, reply string) {
	if !spec.AutoTitle {
		return
	}
	sess, err := r.Sessions.Get(ctx, sessionID)
	if err != nil || sess.TitleSource != "" {
		return
	}
	chain := spec.TitleChain
	if len(chain) == 0 {
		chain = spec.Chain
	}
	ctx = context.WithoutCancel(ctx)
	r.titles.Add(1)
	go func() {
		defer r.titles.Done()
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		title, err := r.makeTitle(ctx, chain, user, reply)
		if err == nil && title != "" {
			_, err = r.Sessions.SetTitleFrom(ctx, sessionID, title, session.TitleAuto)
		}
		if err != nil {
			r.Log.Warn("automatic session title failed", "session", sessionID, "err", err)
		}
	}()
}

// WaitTitles waits for automatic titles being written (tests, shutdown).
func (r *Runner) WaitTitles() { r.titles.Wait() }

func (r *Runner) makeTitle(ctx context.Context, chain []llm.Target, user, reply string) (string, error) {
	input := "<user>\n" + cutRunes(user, 2000) + "\n</user>\n<assistant>\n" + cutRunes(reply, 2000) + "\n</assistant>"
	resp, err := r.Models.Send(ctx, chain, llm.Request{
		System: titleSystem, Messages: []llm.Message{llm.UserText(input)}, MaxTokens: 200,
	}, nil)
	if err != nil {
		return "", err
	}
	return cleanTitle(resp.Message.Text()), nil
}

// cleanTitle keeps the first line, without quotes, markup or a final
// period, at most 80 characters.
func cleanTitle(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "Title:"))
	s = strings.Trim(s, "\"'`*#“”‘’ ")
	s = strings.TrimRight(s, ".")
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 80 {
		s = string([]rune(s)[:80])
	}
	return s
}
