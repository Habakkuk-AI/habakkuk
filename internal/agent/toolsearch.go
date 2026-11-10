package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// Tool search keeps big catalogs (many MCP servers) out of every request.
// An agent with more tool specs than its ToolSearchOver is offered only its
// core tools plus tool_search and tool_describe. The tools those two name
// are offered from the next model call on, appended after the ones already
// offered so the start of the request (and the provider's prompt cache) does
// not move. Which tools were found is read back from the transcript, so it
// lasts for the session and survives restarts.

const (
	toolSearchName   = "tool_search"
	toolDescribeName = "tool_describe"
	// toolSearchSet is the toolset holding both tools; no agent lists it,
	// they are offered only when a catalog is deferred.
	toolSearchSet = "tool-search"
	// enabledPrefix starts the line in their results that names the tools
	// now callable.
	enabledPrefix = "[callable now: "
)

type catalogKey struct{}

func catalogFrom(ctx context.Context) []llm.ToolSpec {
	c, _ := ctx.Value(catalogKey{}).([]llm.ToolSpec)
	return c
}

// toolSpecs are the tools offered at the start of the turn.
func (t *turn) toolSpecs() []llm.ToolSpec {
	all := t.r.Tools.Specs(t.toolsets())
	if t.job.Kind == KindSubturn {
		all = withoutDelegate(all)
	}
	if t.spec.Home == "" {
		all = withoutHomeTool(all)
	}
	if t.spec.ToolSearchOver <= 0 || len(all) <= t.spec.ToolSearchOver {
		return all
	}
	t.r.Tools.RegisterOrReplace(toolSearchSet, toolSearchTool{})
	t.r.Tools.RegisterOrReplace(toolSearchSet, toolDescribeTool{})
	t.ctx = context.WithValue(t.ctx, catalogKey{}, all)
	core := map[string]bool{}
	for _, s := range t.r.Tools.Specs([]string{"core"}) {
		core[s.Name] = true
	}
	var out []llm.ToolSpec
	for _, s := range all {
		if core[s.Name] {
			out = append(out, s)
		}
	}
	out = append(out, toolSearchTool{}.Spec(), toolDescribeTool{}.Spec())
	return appendFound(out, all, foundTools(t.msgs))
}

// moreTools adds the tools this round's tool_search and tool_describe calls
// named, for the next model call.
func (t *turn) moreTools(specs []llm.ToolSpec, calls, results []llm.Block) []llm.ToolSpec {
	all := catalogFrom(t.ctx)
	if all == nil {
		return specs
	}
	var names []string
	for i, c := range calls {
		if (c.ToolName == toolSearchName || c.ToolName == toolDescribeName) && i < len(results) && !results[i].IsError {
			names = append(names, enabledNames(results[i].Text)...)
		}
	}
	return appendFound(specs, all, names)
}

// appendFound appends the catalog's specs for names not offered yet, in
// the order given.
func appendFound(specs, all []llm.ToolSpec, names []string) []llm.ToolSpec {
	for _, n := range names {
		if slices.ContainsFunc(specs, func(s llm.ToolSpec) bool { return s.Name == n }) {
			continue
		}
		if i := slices.IndexFunc(all, func(s llm.ToolSpec) bool { return s.Name == n }); i >= 0 {
			specs = append(specs, all[i])
		}
	}
	return specs
}

// foundTools lists, in order, the tools earlier tool_search and
// tool_describe results in the transcript made callable.
func foundTools(msgs []llm.Message) []string {
	calls := map[string]bool{}
	var out []string
	for _, m := range msgs {
		for _, b := range m.Blocks {
			switch {
			case b.Type == llm.ToolUse && (b.ToolName == toolSearchName || b.ToolName == toolDescribeName):
				calls[b.ToolUseID] = true
			case b.Type == llm.ToolResult && calls[b.ToolUseID] && !b.IsError:
				out = append(out, enabledNames(b.Text)...)
			}
		}
	}
	return out
}

func enabledNames(result string) []string {
	i := strings.LastIndex(result, enabledPrefix)
	if i < 0 {
		return nil
	}
	line, _, _ := strings.Cut(result[i+len(enabledPrefix):], "]")
	var out []string
	for _, n := range strings.Split(line, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

func enabledLine(names []string) string {
	return "\n" + enabledPrefix + strings.Join(names, ", ") + "]"
}

// tool_search

type toolSearchTool struct{}

func (toolSearchTool) Risk() tools.Risk { return tools.RiskRead }
func (toolSearchTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: toolSearchName,
		Description: `Find tools for a task. You have many more tools than are shown; search them by what you want to do (e.g. "create calendar event", "github pull request", "send email"). ` +
			`The tools found become callable from your next step, with their full input schemas. Use tool_describe to see one tool in full.`,
		Schema: json.RawMessage(`{"type":"object","properties":{
			"query":{"type":"string","description":"what you want to do, in a few words"},
			"limit":{"type":"integer","description":"how many tools, default 8, at most 20"}
		},"required":["query"]}`),
	}
}

func (toolSearchTool) Run(ctx context.Context, _ *tools.Env, input json.RawMessage) (string, error) {
	in, err := decodeInput[struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}](input)
	if err != nil {
		return "", err
	}
	all := catalogFrom(ctx)
	if all == nil {
		return "", errors.New("all your tools are already shown; tool search is not needed")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 8
	}
	limit = min(limit, 20)
	hits := rankTools(all, in.Query)
	if len(hits) == 0 {
		names := make([]string, len(all))
		for i, s := range all {
			names[i] = s.Name
		}
		return "No tool matches " + fmt.Sprintf("%q", in.Query) + ". All tool names:\n" + cutRunes(strings.Join(names, ", "), 4000), nil
	}
	hits = hits[:min(limit, len(hits))]
	var b strings.Builder
	names := make([]string, len(hits))
	for i, s := range hits {
		names[i] = s.Name
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, cutRunes(firstSentence(s.Description), 240))
	}
	b.WriteString("These are callable from your next step.")
	b.WriteString(enabledLine(names))
	return b.String(), nil
}

// rankTools scores each spec by the query's words: a whole word of its name
// counts most, part of its name less, its description least. Tools scoring
// under two thirds of the best are left out, so a precise query gets a
// short answer.
func rankTools(all []llm.ToolSpec, query string) []llm.ToolSpec {
	split := func(s string) []string {
		return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r <= 127
		})
	}
	words := split(query)
	if len(words) == 0 {
		return nil
	}
	type scored struct {
		spec  llm.ToolSpec
		score int
	}
	var out []scored
	best := 0
	for _, s := range all {
		name, desc := strings.ToLower(s.Name), strings.ToLower(s.Description)
		parts := split(s.Name)
		score := 0
		for _, w := range words {
			switch {
			case slices.Contains(parts, w):
				score += 6
			case strings.Contains(name, w):
				score += 2
			}
			if len(w) > 2 && strings.Contains(desc, w) {
				score++
			}
		}
		if score > 0 {
			out = append(out, scored{s, score})
			best = max(best, score)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].spec.Name < out[j].spec.Name
	})
	var specs []llm.ToolSpec
	for _, s := range out {
		if s.score*3 >= best*2 {
			specs = append(specs, s.spec)
		}
	}
	return specs
}

// tool_describe

type toolDescribeTool struct{}

func (toolDescribeTool) Risk() tools.Risk { return tools.RiskRead }
func (toolDescribeTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        toolDescribeName,
		Description: `Show a tool's full description and input schema by its exact name, and make it callable from your next step.`,
		Schema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string"}
		},"required":["name"]}`),
	}
}

func (toolDescribeTool) Run(ctx context.Context, _ *tools.Env, input json.RawMessage) (string, error) {
	in, err := decodeInput[struct {
		Name string `json:"name"`
	}](input)
	if err != nil {
		return "", err
	}
	all := catalogFrom(ctx)
	if all == nil {
		return "", errors.New("all your tools are already shown; tool search is not needed")
	}
	name := strings.TrimSpace(in.Name)
	i := slices.IndexFunc(all, func(s llm.ToolSpec) bool { return s.Name == name })
	if i < 0 {
		var near []string
		for _, s := range rankTools(all, name) {
			near = append(near, s.Name)
		}
		if len(near) > 5 {
			near = near[:5]
		}
		msg := fmt.Sprintf("no tool is named %q", name)
		if len(near) > 0 {
			msg += "; did you mean " + strings.Join(near, ", ") + "?"
		}
		return "", errors.New(msg)
	}
	s := all[i]
	return fmt.Sprintf("%s\n\n%s\n\nInput schema:\n%s%s", s.Name, s.Description, s.Schema, enabledLine([]string{s.Name})), nil
}

func firstSentence(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	return s
}

func cutRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

// pendingNote tells the model which tool servers are still connecting, so
// it says so instead of acting as if their tools did not exist.
func pendingNote(servers []string) string {
	return "Note: the MCP server(s) " + strings.Join(servers, ", ") + " are still connecting, so their tools " +
		"are not available in this turn. If the request needs them, say they are still starting and to try again in a minute."
}
