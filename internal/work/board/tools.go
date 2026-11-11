package board

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// Here returns the reply address of the chat a session belongs to, or nil
// when the session is not a chat.
type Here func(ctx context.Context, sessionID string) json.RawMessage

// RegisterTools adds the "board" toolset. Creating and changing cards hands
// work to other agents, so those tools carry network risk, like delegate
// (allowed in workspace mode, denied in read-only mode). Reading the board,
// commenting, and completing or blocking your own card only record what an
// agent says on the board, so they are allowed in every mode: an agent that
// may not write files must still be able to report on the work it was given.
func RegisterTools(r *tools.Registry, d *Dispatcher, here Here) {
	r.Register("board", createTool{d, here})
	r.Register("board", listTool{d})
	r.Register("board", showTool{d})
	r.Register("board", commentTool{d})
	r.Register("board", completeTool{d})
	r.Register("board", blockTool{d})
	r.Register("board", updateTool{d})
}

func decode[T any](input json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(input, &v); err != nil {
		return v, fmt.Errorf("invalid input: %w", err)
	}
	return v, nil
}

// known checks that name is a configured agent.
func (d *Dispatcher) known(name string) error {
	if _, ok := d.Runner.Agents[name]; ok {
		return nil
	}
	var names []string
	for n := range d.Runner.Agents {
		names = append(names, n)
	}
	slices.Sort(names)
	return fmt.Errorf("unknown agent %q; configured: %s", name, strings.Join(names, ", "))
}

func (d *Dispatcher) loc() *time.Location {
	if d.Runner.Location != nil {
		return d.Runner.Location
	}
	return time.Local
}

type createTool struct {
	d    *Dispatcher
	here Here
}

func (createTool) Risk() tools.Risk { return tools.RiskNetwork }
func (createTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "board_create",
		Description: `Put a card on the shared task board for an agent (yourself or another configured agent). The assignee starts on it as soon as every card in depends_on is done, in a fresh conversation that sees only the card, so write the body to stand alone: what to do, what "done" means, and what proof to give. You do not wait: when the card is done or blocked you get its result here as a new message. Use depends_on to chain steps (each later card sees the results of the cards it depends on) and parent to group cards under a larger one.`,
		Schema: json.RawMessage(`{"type":"object","properties":{
			"title":{"type":"string"},
			"body":{"type":"string","description":"the task in full"},
			"assignee":{"type":"string","description":"agent to do it; omit to leave the card unassigned"},
			"priority":{"type":"integer","description":"higher runs first (default 0)"},
			"depends_on":{"type":"array","items":{"type":"string"},"description":"card ids that must be done first"},
			"parent":{"type":"string","description":"card id this one is part of"}
		},"required":["title","body"]}`),
	}
}

func (t createTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		Title     string   `json:"title"`
		Body      string   `json:"body"`
		Assignee  string   `json:"assignee"`
		Priority  int      `json:"priority"`
		DependsOn []string `json:"depends_on"`
		Parent    string   `json:"parent"`
	}](input)
	if err != nil {
		return "", err
	}
	if in.Assignee != "" {
		if err := t.d.known(in.Assignee); err != nil {
			return "", err
		}
	}
	c := Card{Title: in.Title, Body: in.Body, Assignee: in.Assignee, Priority: in.Priority, DependsOn: in.DependsOn,
		Parent: in.Parent, CreatedBy: env.Agent, CreatorSession: env.Session, Tainted: env.Untrusted()}
	if t.here != nil {
		c.ReplyTo = t.here(ctx, env.Session)
	}
	created, err := t.d.Store.Create(ctx, c)
	if err != nil {
		return "", err
	}
	t.d.Wake()
	switch {
	case created.Assignee == "":
		return fmt.Sprintf("Created %s, unassigned. Assign it with board_update.", created.ID), nil
	case len(created.DependsOn) > 0:
		return fmt.Sprintf("Created %s for %s; it starts once %s are done. Its result will come back here as a new message.",
			created.ID, created.Assignee, strings.Join(created.DependsOn, ", ")), nil
	}
	return fmt.Sprintf("Created %s for %s; it starts now. Its result will come back here as a new message.", created.ID, created.Assignee), nil
}

type listTool struct{ d *Dispatcher }

func (listTool) Risk() tools.Risk { return tools.RiskRead }
func (listTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "board_list",
		Description: "List cards on the shared task board. By default shows open cards (todo, doing, blocked).",
		Schema: json.RawMessage(`{"type":"object","properties":{
			"status":{"type":"string","enum":["open","todo","doing","blocked","done","cancelled","all"]},
			"assignee":{"type":"string"},
			"mine":{"type":"boolean","description":"only cards assigned to you"}
		}}`),
	}
}

func (t listTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		Status   string `json:"status"`
		Assignee string `json:"assignee"`
		Mine     bool   `json:"mine"`
	}](input)
	if err != nil {
		return "", err
	}
	f := Filter{Status: in.Status, Assignee: in.Assignee}
	if f.Status == "open" {
		f.Status = ""
	}
	if in.Mine {
		f.Assignee = env.Agent
	}
	list, err := t.d.Store.List(ctx, f)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "No cards.", nil
	}
	return FormatList(list), nil
}

type showTool struct{ d *Dispatcher }

func (showTool) Risk() tools.Risk { return tools.RiskRead }
func (showTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "board_show",
		Description: "Show one card in full: body, result, dependencies, comments and history.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
	}
}

func (t showTool) Run(ctx context.Context, _ *tools.Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		ID string `json:"id"`
	}](input)
	if err != nil {
		return "", err
	}
	c, err := t.d.Store.Get(ctx, in.ID)
	if err != nil {
		return "", err
	}
	return FormatCard(ctx, t.d.Store, c, t.d.loc()), nil
}

type commentTool struct{ d *Dispatcher }

func (commentTool) Risk() tools.Risk { return tools.RiskRead }
func (commentTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "board_comment",
		Description: "Add a comment to a card: progress, a question, or an answer for a blocked card. The assignee sees comments when its next attempt starts.",
		Schema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"text":{"type":"string"}},
			"required":["id","text"]}`),
	}
}

func (t commentTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}](input)
	if err != nil {
		return "", err
	}
	if err := t.d.markIfUntrusted(ctx, env, in.ID); err != nil {
		return "", err
	}
	if err := t.d.Store.Comment(ctx, in.ID, env.Agent, in.Text); err != nil {
		return "", err
	}
	return "Commented on " + in.ID + ".", nil
}

type completeTool struct{ d *Dispatcher }

func (completeTool) Risk() tools.Risk { return tools.RiskRead }
func (completeTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "board_complete",
		Description: "Mark a card assigned to you as done. The result goes back to whoever created the card, so make it complete on its own: what you did, what you found, and proof (file paths, links, command output).",
		Schema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"result":{"type":"string"}},
			"required":["id","result"]}`),
	}
}

func (t completeTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		ID     string `json:"id"`
		Result string `json:"result"`
	}](input)
	if err != nil {
		return "", err
	}
	if err := t.d.markIfUntrusted(ctx, env, in.ID); err != nil {
		return "", err
	}
	if _, err := t.d.Store.Complete(ctx, in.ID, env.Agent, in.Result); err != nil {
		return "", err
	}
	t.d.Wake()
	return "Completed " + in.ID + "; the result was sent to its creator.", nil
}

type blockTool struct{ d *Dispatcher }

func (blockTool) Risk() tools.Risk { return tools.RiskRead }
func (blockTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "board_block",
		Description: "Mark a card assigned to you as blocked when you cannot finish it: say exactly what is missing or what you need decided. Its creator is told.",
		Schema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"reason":{"type":"string"}},
			"required":["id","reason"]}`),
	}
}

func (t blockTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}](input)
	if err != nil {
		return "", err
	}
	if err := t.d.markIfUntrusted(ctx, env, in.ID); err != nil {
		return "", err
	}
	if _, err := t.d.Store.Block(ctx, in.ID, env.Agent, in.Reason); err != nil {
		return "", err
	}
	t.d.Wake()
	return "Blocked " + in.ID + "; its creator was told.", nil
}

type updateTool struct{ d *Dispatcher }

func (updateTool) Risk() tools.Risk { return tools.RiskNetwork }
func (updateTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "board_update",
		Description: `Change a card you created or that is assigned to you: reassign it, change its priority, cancel it (status "cancelled"), or reopen a blocked card for another attempt (status "todo"). Reassigning a card that is blocked or in progress restarts it with the new assignee.`,
		Schema: json.RawMessage(`{"type":"object","properties":{
			"id":{"type":"string"},
			"assignee":{"type":"string"},
			"priority":{"type":"integer"},
			"status":{"type":"string","enum":["todo","cancelled"]},
			"note":{"type":"string","description":"comment explaining the change"}
		},"required":["id"]}`),
	}
}

func (t updateTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		ID       string  `json:"id"`
		Assignee *string `json:"assignee"`
		Priority *int    `json:"priority"`
		Status   string  `json:"status"`
		Note     string  `json:"note"`
	}](input)
	if err != nil {
		return "", err
	}
	if in.Assignee != nil && *in.Assignee != "" {
		if err := t.d.known(*in.Assignee); err != nil {
			return "", err
		}
	}
	if err := t.d.markIfUntrusted(ctx, env, in.ID); err != nil {
		return "", err
	}
	c, err := t.d.Store.Update(ctx, in.ID, env.Agent, Update{Assignee: in.Assignee, Priority: in.Priority,
		Status: Status(in.Status), Note: in.Note})
	if err != nil {
		return "", err
	}
	t.d.Wake()
	return fmt.Sprintf("Updated %s: %s, assigned to %s, priority %d.", c.ID, c.Status, orNone(c.Assignee), c.Priority), nil
}

// FormatList renders cards one per line.
func FormatList(list []Card) string {
	var b strings.Builder
	for _, c := range list {
		fmt.Fprintf(&b, "%s  %-9s p%-3d %-12s %q", c.ID, c.Status, c.Priority, orNone(c.Assignee), c.Title)
		if len(c.DependsOn) > 0 {
			fmt.Fprintf(&b, "  after %s", strings.Join(c.DependsOn, ","))
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// FormatCard renders one card in full, with the state of its dependencies.
func FormatCard(ctx context.Context, st *Store, c *Card, loc *time.Location) string {
	var b strings.Builder
	ts := func(t time.Time) string { return t.In(loc).Format("2006-01-02 15:04") }
	fmt.Fprintf(&b, "%s %q\nstatus %s, priority %d, assigned to %s, created by %s %s\n",
		c.ID, c.Title, c.Status, c.Priority, orNone(c.Assignee), c.CreatedBy, ts(c.CreatedAt))
	if c.Parent != "" {
		fmt.Fprintf(&b, "part of %s\n", c.Parent)
	}
	if c.Attempt > 0 {
		fmt.Fprintf(&b, "attempt %d, session %s\n", c.Attempt, orNone(c.Session))
	}
	for _, id := range c.DependsOn {
		state := "missing"
		if dep, err := st.Get(ctx, id); err == nil {
			state = string(dep.Status)
		}
		fmt.Fprintf(&b, "depends on %s (%s)\n", id, state)
	}
	if body := strings.TrimSpace(c.Body); body != "" {
		b.WriteString("\n" + body + "\n")
	}
	if c.Result != "" {
		b.WriteString("\nResult:\n" + c.Result + "\n")
	}
	if len(c.Comments) > 0 {
		b.WriteString("\nComments:\n")
		for _, cm := range c.Comments {
			fmt.Fprintf(&b, "- %s %s: %s\n", ts(cm.At), cm.Author, cm.Text)
		}
	}
	if len(c.Events) > 0 {
		b.WriteString("\nHistory:\n")
		for _, e := range c.Events {
			fmt.Fprintf(&b, "- %s %s %s", ts(e.At), e.Actor, e.Kind)
			if e.Detail != "" {
				b.WriteString(" " + e.Detail)
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// markIfUntrusted marks a card tainted before a turn steered by outside
// content writes to it (Card.Tainted).
func (d *Dispatcher) markIfUntrusted(ctx context.Context, env *tools.Env, id string) error {
	if !env.Untrusted() {
		return nil
	}
	return d.Store.MarkTainted(ctx, id)
}
