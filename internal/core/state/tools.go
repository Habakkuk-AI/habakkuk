package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// RegisterTools adds the todo tool to the core toolset.
func RegisterTools(r *tools.Registry, s *Store) {
	r.Register("core", todoTool{s})
}

type todoTool struct{ s *Store }

// Risk is read: the list is the session's own scratch state, so planning
// works in every permission mode.
func (todoTool) Risk() tools.Risk { return tools.RiskRead }
func (todoTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "todo",
		Description: `Keep a task list for this conversation, so multi-step work is planned and nothing is forgotten. The open list is shown to you at the start of every turn. ` +
			`Actions: "add" appends "items"; "update" changes item "id" ("text" and/or "status": pending, in_progress, done); "complete" marks "id" done; "list" shows the list; "clear" removes done items (or everything with "all": true). ` +
			`Keep one item in_progress at a time and mark items done as soon as they are.`,
		Schema: json.RawMessage(`{"type":"object","properties":{
			"action":{"type":"string","enum":["add","update","complete","list","clear"]},
			"items":{"type":"array","items":{"type":"string"},"description":"for add: the tasks, in order"},
			"id":{"type":"integer","description":"for update and complete"},
			"text":{"type":"string","description":"for update: new wording"},
			"status":{"type":"string","enum":["pending","in_progress","done"]},
			"all":{"type":"boolean","description":"for clear: remove every item, not just done ones"}
		},"required":["action"]}`),
	}
}

func (t todoTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	var in struct {
		Action string   `json:"action"`
		Items  []string `json:"items"`
		ID     int      `json:"id"`
		Text   string   `json:"text"`
		Status string   `json:"status"`
		All    bool     `json:"all"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if env.Session == "" {
		return "", errors.New("no session to keep a task list for")
	}
	var err error
	switch in.Action {
	case "add":
		if len(in.Items) == 0 {
			return "", errors.New(`"items" is required for add`)
		}
		err = t.s.AddTodos(ctx, env.Session, in.Items)
	case "update":
		if in.Text == "" && in.Status == "" {
			return "", errors.New(`update needs "text" or "status"`)
		}
		err = t.s.UpdateTodo(ctx, env.Session, in.ID, in.Text, in.Status)
	case "complete":
		err = t.s.UpdateTodo(ctx, env.Session, in.ID, "", TodoDone)
	case "clear":
		err = t.s.ClearTodos(ctx, env.Session, !in.All)
	case "list":
	default:
		return "", fmt.Errorf("unknown action %q", in.Action)
	}
	if err != nil {
		return "", err
	}
	list, err := t.s.Todos(ctx, env.Session)
	if err != nil {
		return "", err
	}
	return RenderTodos(list), nil
}
