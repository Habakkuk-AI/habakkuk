package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// RegisterTools adds session_search to the memory toolset: recalling what
// was said in earlier conversations is part of remembering.
func RegisterTools(r *tools.Registry, s *Store) {
	r.Register("memory", searchTool{s})
}

type searchTool struct{ s *Store }

func (searchTool) Risk() tools.Risk { return tools.RiskRead }
func (searchTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "session_search",
		Description: `Full-text search over your past conversations (this one excluded): what the user asked, what you answered, decisions and details from earlier sessions. ` +
			`Give "query" words; results are ranked, with the session id, message number and a snippet with matches in [brackets]. ` +
			`To read the messages around a hit, call again with "session" and "seq" (no query). Long-term facts belong in memory; use this to find what memory does not hold.`,
		Schema: json.RawMessage(`{"type":"object","properties":{
			"query":{"type":"string"},
			"limit":{"type":"integer","description":"default 10, at most 50"},
			"session":{"type":"string","description":"with seq: show messages around a hit"},
			"seq":{"type":"integer"}
		}}`),
	}
}

func (t searchTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	var in struct {
		Query   string `json:"query"`
		Limit   int    `json:"limit"`
		Session string `json:"session"`
		Seq     int    `json:"seq"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	var b strings.Builder
	if in.Session != "" && strings.TrimSpace(in.Query) == "" {
		ex, err := t.s.Around(ctx, env.Agent, in.Session, in.Seq, 3)
		if err != nil {
			return "", err
		}
		if len(ex) == 0 {
			return "no messages there", nil
		}
		for _, e := range ex {
			fmt.Fprintf(&b, "#%d %s:\n%s\n\n", e.Seq, e.Role, cutText(e.Text, 2000))
		}
		return strings.TrimSpace(b.String()), nil
	}
	hits, err := t.s.Search(ctx, env.Agent, in.Query, env.Session, in.Limit)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "no matches", nil
	}
	for _, h := range hits {
		fmt.Fprintf(&b, "- %s #%d (%s, %s) %q: %s\n", h.Session, h.Seq, h.Role, h.At.Format("2006-01-02"), h.Title, h.Snippet)
	}
	return strings.TrimSpace(b.String()), nil
}

func cutText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
