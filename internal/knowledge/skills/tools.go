package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// Toolset is the name the skill tools are registered under.
const Toolset = "skills"

// RegisterTools adds skill_view, skill_file and skill_save.
func RegisterTools(r *tools.Registry, l *Library) {
	r.Register(Toolset, viewTool{l})
	r.Register(Toolset, fileTool{l})
	r.Register(Toolset, saveTool{l})
}

func decode[T any](input json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(input, &v); err != nil {
		return v, fmt.Errorf("invalid input: %w", err)
	}
	return v, nil
}

// skill_view

type viewTool struct{ l *Library }

func (viewTool) Risk() tools.Risk { return tools.RiskRead }
func (viewTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "skill_view",
		Description: "Load a skill: returns its instructions and lists its supporting files. Do this before a task the skill covers, then follow it.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
	}
}

func (t viewTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		Name string `json:"name"`
	}](input)
	if err != nil {
		return "", err
	}
	s, body, files, err := t.l.View(env.Agent, in.Name)
	if err != nil {
		return "", err
	}
	t.l.MarkUsed(s)
	var b strings.Builder
	fmt.Fprintf(&b, "# Skill: %s\n", s.Name)
	if s.Description != "" {
		fmt.Fprintf(&b, "%s\n", s.Description)
	}
	if src, untrusted := t.l.Untrusted(s); untrusted {
		fmt.Fprintf(&b, "\n%s\n", untrustedNote(src))
	}
	b.WriteString("\n")
	b.WriteString(body)
	if len(files) > 0 {
		b.WriteString("\n\nSupporting files (read with skill_file):\n")
		for _, f := range files {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	return b.String(), nil
}

// skill_file

type fileTool struct{ l *Library }

func (fileTool) Risk() tools.Risk { return tools.RiskRead }
func (fileTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "skill_file",
		Description: "Read one supporting file of a skill, by its path relative to the skill folder (as listed by skill_view).",
		Schema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string"},"path":{"type":"string"}},"required":["name","path"]}`),
	}
}

func (t fileTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}](input)
	if err != nil {
		return "", err
	}
	return t.l.ReadFile(env.Agent, in.Name, in.Path)
}

// skill_save

type saveTool struct{ l *Library }

func (saveTool) Risk() tools.Risk { return tools.RiskWrite }
func (saveTool) OwnState()        {}
func (saveTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "skill_save",
		Description: "Create or replace one of your own skills: a reusable procedure you worked out and want to follow next time. " +
			"The body replaces the whole SKILL.md (the previous one is kept as SKILL.md.bak), so view the skill first when improving it. " +
			"Saving a skill with the name of a shared skill makes your copy take its place for you.",
		Schema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","description":"lowercase letters, digits and hyphens, e.g. \"weekly-report\""},
			"description":{"type":"string","description":"one line: what the skill does and when to use it"},
			"body":{"type":"string","description":"Markdown instructions"}},
			"required":["name","description","body"]}`),
	}
}

func (t saveTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	}](input)
	if err != nil {
		return "", err
	}
	// Planted text must not become a procedure later prompts list: a turn
	// with outside content in it, or a guest's, saves the skill held.
	if env.Taint.Marked() || (env.Policy != nil && env.Policy.Guest) {
		h, err := t.l.SaveHeld(env.Agent, in.Name, in.Description, in.Body)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("saved skill %s, but it is held and not in use yet: outside content entered this turn, so the owner has to look at it first. Tell the user they can approve it with hk skills approve %s/%s (it is in %s).", h.Name, h.Agent, h.Name, h.Dir), nil
	}
	ctx = history.WithActor(ctx, history.Actor{By: history.ByAgent, Tool: "skill_save", Session: env.Session, Tainted: env.Taint.Marked()})
	s, created, err := t.l.SaveContext(ctx, env.Agent, in.Name, in.Description, in.Body)
	if err != nil {
		return "", err
	}
	note := ""
	if why := t.l.NoCommand(s.Name); why != "" {
		note = ". Note: " + why + "."
	}
	if created {
		return fmt.Sprintf("created skill %s in %s%s", s.Name, s.Dir, note), nil
	}
	return fmt.Sprintf("updated skill %s in %s (previous version kept as %s)%s", s.Name, s.Dir, BackupName, note), nil
}
