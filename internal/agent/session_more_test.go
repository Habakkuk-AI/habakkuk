package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// echo answers every user turn with "re: <text>".
func echo() *script {
	return &script{fn: func(_ []llm.Target, req llm.Request) *llm.Response {
		if req.System == titleSystem {
			return say("\"Plans for a Paris trip.\"")
		}
		_, msg, _ := strings.Cut(req.Messages[len(req.Messages)-1].Text(), "\n")
		return say("re: " + msg)
	}}
}

func (h *harness) turn(t *testing.T, sid, text string) string {
	t.Helper()
	ctx := context.Background()
	h.pools(t)
	id, err := h.runner.Submit(ctx, sid, text)
	if err != nil {
		t.Fatal(err)
	}
	final, err := h.runner.wait(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return final
}

func TestForkAndRewind(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, echo())
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	for _, m := range []string{"one", "two", "three"} {
		h.turn(t, s.ID, m)
	}
	orig := h.transcript(t, s.ID)
	if len(orig) != 6 {
		t.Fatalf("transcript has %d messages", len(orig))
	}
	fork, err := h.runner.Fork(ctx, s.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := h.sess.Get(ctx, fork.ID)
	if got.Parent != s.ID || got.SystemPrompt != s.SystemPrompt || !strings.HasSuffix(got.Title, "(fork)") {
		t.Fatalf("fork = %+v", got)
	}
	if n := len(h.transcript(t, fork.ID)); n != 6 {
		t.Fatalf("fork has %d messages", n)
	}
	rew, err := h.runner.Fork(ctx, s.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	msgs := h.transcript(t, rew.ID)
	if len(msgs) != 2 || !strings.HasSuffix(msgs[1].Text(), "re: one") {
		t.Fatalf("rewound transcript = %v", msgs)
	}
	// The rewound session goes on from there; the original is untouched.
	if final := h.turn(t, rew.ID, "other way"); final != "re: other way" {
		t.Fatalf("final = %q", final)
	}
	if n := len(h.transcript(t, s.ID)); n != 6 {
		t.Fatalf("original changed: %d messages", n)
	}
	if _, err := h.runner.Fork(ctx, s.ID, 9); err == nil {
		t.Fatal("rewound past the start")
	}
	// The fork's messages are searchable as its own.
	hits, err := h.sess.Search(ctx, "", "three", "", 10)
	if err != nil || !slices.ContainsFunc(hits, func(x session.Hit) bool { return x.Session == fork.ID }) {
		t.Fatalf("hits = %+v, %v", hits, err)
	}
}

func TestForkLeavesATurnInProgressOut(t *testing.T) {
	recs := []session.Record{
		{Message: llm.UserText("a")}, {Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "b"}}}},
		{Message: llm.UserText("c")},
		{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.ToolUse, ToolUseID: "x", ToolName: "exec"}}}},
	}
	if n, err := forkPoint(recs, 0); err != nil || n != 2 {
		t.Fatalf("fork point = %d, %v", n, err)
	}
	if n, err := forkPoint(recs, 1); err != nil || n != 2 {
		t.Fatalf("rewind 1 = %d, %v", n, err)
	}
	if n, err := forkPoint(recs, 2); err != nil || n != 0 {
		t.Fatalf("rewind 2 = %d, %v", n, err)
	}
	if _, err := forkPoint(recs, -1); err == nil {
		t.Fatal("negative rewind accepted")
	}
}

func TestAutoTitleAfterTheFirstTurn(t *testing.T) {
	ctx := context.Background()
	m := echo()
	h := newHarness(t, m)
	h.runner.Agents["habakkuk"].AutoTitle = true
	h.runner.Agents["habakkuk"].TitleChain = []llm.Target{{Endpoint: "cheap", Model: "c"}}
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	h.turn(t, s.ID, "help me plan five days in Paris in May")
	h.runner.WaitTitles()
	got, _ := h.sess.Get(ctx, s.ID)
	if got.Title != "Plans for a Paris trip" || got.TitleSource != session.TitleAuto {
		t.Fatalf("title = %q (%s)", got.Title, got.TitleSource)
	}
	// Later turns do not ask again, and a title the user set stays.
	h.turn(t, s.ID, "and a day trip")
	h.runner.WaitTitles()
	s2, _ := h.runner.NewSession(ctx, "habakkuk")
	if _, err := h.sess.SetTitleFrom(ctx, s2.ID, "Mine", session.TitleUser); err != nil {
		t.Fatal(err)
	}
	h.turn(t, s2.ID, "hello")
	h.runner.WaitTitles()
	if got, _ := h.sess.Get(ctx, s2.ID); got.Title != "Mine" {
		t.Fatalf("user title replaced by %q", got.Title)
	}
	titles := 0
	m.mu.Lock()
	for _, r := range m.reqs {
		if r.System == titleSystem {
			titles++
			if !strings.Contains(r.Messages[0].Text(), "five days in Paris") {
				t.Errorf("title input = %q", r.Messages[0].Text())
			}
		}
	}
	m.mu.Unlock()
	if titles != 1 {
		t.Fatalf("title calls = %d", titles)
	}
}

func TestCleanTitle(t *testing.T) {
	for in, want := range map[string]string{
		"\"Quarterly report draft.\"\nextra": "Quarterly report draft",
		"Title: **Fix the build**":           "Fix the build",
		"  spaced   out  ":                   "spaced out",
	} {
		if got := cleanTitle(in); got != want {
			t.Errorf("cleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToolSearchDefersABigCatalog(t *testing.T) {
	ctx := context.Background()
	var offered [][]string
	m := &script{fn: func(_ []llm.Target, req llm.Request) *llm.Response {
		var names []string
		for _, s := range req.Tools {
			names = append(names, s.Name)
		}
		offered = append(offered, names)
		last := req.Messages[len(req.Messages)-1]
		res, isResult := lastResult(last)
		switch {
		case !isResult && strings.Contains(last.Text(), "again"):
			return call("c3", "widget_42", map[string]any{})
		case !isResult:
			return call("c1", "tool_search", map[string]any{"query": "widget 42"})
		case strings.Contains(res, "callable now"):
			return call("c2", "widget_42", map[string]any{})
		}
		return say("used: " + res)
	}}
	h := newHarness(t, m)
	for i := range 70 {
		h.runner.Tools.Register("big", noteTool{fmt.Sprintf("widget_%d", i)})
	}
	spec := h.runner.Agents["habakkuk"]
	spec.Toolsets = []string{"core", "big"}
	spec.ToolSearchOver = 60
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	if final := h.turn(t, s.ID, "use widget 42"); final != "used: ran widget_42" {
		t.Fatalf("final = %q", final)
	}
	first, second := offered[0], offered[1]
	if slices.Contains(first, "widget_42") || !slices.Contains(first, "tool_search") || !slices.Contains(first, "read_file") {
		t.Fatalf("first call offered %v", first)
	}
	if len(second) != len(first)+1 || !slices.Equal(second[:len(first)], first) || second[len(first)] != "widget_42" {
		t.Fatalf("found tool not appended: %v then %v", first, second)
	}
	// The next turn offers it from the start, from the transcript.
	h.turn(t, s.ID, "again")
	if next := offered[3]; !slices.Equal(next, second) {
		t.Fatalf("next turn offered %v", next)
	}
}

func TestToolSearchOffWhenSmall(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, echo())
	h.runner.Agents["habakkuk"].ToolSearchOver = 60
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	h.turn(t, s.ID, "hi")
	m := h.runner.Models.(*script)
	for _, spec := range m.reqs[0].Tools {
		if spec.Name == toolSearchName {
			t.Fatal("tool_search offered for a small catalog")
		}
	}
}

func TestToolSearchRanking(t *testing.T) {
	all := []llm.ToolSpec{
		{Name: "mcp__cal__create_event", Description: "Create a calendar event."},
		{Name: "mcp__gh__create_pr", Description: "Open a pull request."},
		{Name: "send_email", Description: "Send an email to someone, for example about a calendar event."},
	}
	got := rankTools(all, "calendar event create")
	if len(got) != 1 || got[0].Name != "mcp__cal__create_event" {
		t.Fatalf("ranked = %v", got)
	}
	if got := rankTools(all, "email"); len(got) != 1 || got[0].Name != "send_email" {
		t.Fatalf("ranked = %v", got)
	}
	out, err := toolDescribeTool{}.Run(context.WithValue(context.Background(), catalogKey{}, all), nil,
		json.RawMessage(`{"name":"send_emial"}`))
	if err == nil || !strings.Contains(err.Error(), "send_email") {
		t.Fatalf("describe typo: %q %v", out, err)
	}
	if names := enabledNames("x\n" + enabledPrefix + "a, b]"); !slices.Equal(names, []string{"a", "b"}) {
		t.Fatalf("enabled = %v", names)
	}
}
