package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// KindSubturn is a delegated task. Sub-turns run on their own worker pool so
// a parent waiting on its children can never starve them of workers, and
// they cannot delegate further.
const KindSubturn = "subturn"

const delegateName = "delegate"

// SubturnSpec is what a delegate call asked of its sub-turn. It rides in the
// sub-turn's payload, so a sub-turn resumed after a crash keeps it.
type SubturnSpec struct {
	Parent       string          `json:"parent,omitempty"`        // the delegating session
	ParentOrigin json.RawMessage `json:"parent_origin,omitempty"` // its chat's reply address, for background results
	Task         string          `json:"task,omitempty"`          // for /subagents
	Model        string          `json:"model,omitempty"`         // "<provider>/<model>" tried first
	Toolsets     []string        `json:"toolsets,omitempty"`      // a subset of the parent's; empty means the agent's own
	OutputSchema json.RawMessage `json:"output_schema,omitempty"` // the final answer must be JSON matching it
	Worktree     *Worktree       `json:"worktree,omitempty"`      // the git worktree the sub-agent works in
	Background   bool            `json:"background,omitempty"`    // the parent gets a turn with the result
	Turn         string          `json:"turn,omitempty"`          // the delegating turn, whose budget it spends from
	Tainted      bool            `json:"tainted,omitempty"`       // the parent had read outside content
	RulesPaused  bool            `json:"rules_paused,omitempty"`  // the parent's approval rules were paused (taint.go)
}

// RegisterDelegate adds the "delegate" toolset.
func (r *Runner) RegisterDelegate(reg *tools.Registry) {
	reg.Register("delegate", delegateTool{r})
}

type delegateTool struct{ r *Runner }

func (delegateTool) Risk() tools.Risk { return tools.RiskNetwork }
func (t delegateTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: delegateName,
		Description: `Hand a self-contained task to a sub-agent and wait for its report. The sub-agent starts with a fresh context: put everything it needs in "task" and "context". Use it to run independent pieces of work in parallel (call it several times in one response) or to keep long research out of your own context. "agent" picks another configured agent; omit it to use a copy of yourself. Sub-agents cannot ask the user for approval.
Options: "model" ("provider/model") runs the sub-agent on another model first. "toolsets" limits it to some of your toolsets. "output_schema" (a JSON Schema) makes it end with a JSON answer that matches; you get that JSON back. "worktree": true (coding tasks in a git workspace) gives it a fresh git worktree and branch, and the report ends with the branch and a diff stat. "background": true returns at once; the report arrives later as a new message in this conversation.`,
		Schema: json.RawMessage(`{"type":"object","properties":{
			"task":{"type":"string","description":"what to do and what to report back"},
			"context":{"type":"string","description":"facts, files and constraints the sub-agent needs"},
			"agent":{"type":"string"},
			"model":{"type":"string","description":"provider/model to run the sub-agent on"},
			"toolsets":{"type":"array","items":{"type":"string"},"description":"a subset of your toolsets"},
			"output_schema":{"type":"object","description":"JSON Schema the final answer must match"},
			"worktree":{"type":"boolean","description":"work in a fresh git worktree of your workspace"},
			"background":{"type":"boolean","description":"return at once; the result arrives as a new message"}
		},"required":["task"]}`),
	}
}

type delegateInput struct {
	Task         string          `json:"task"`
	Context      string          `json:"context"`
	Agent        string          `json:"agent"`
	Model        string          `json:"model"`
	Toolsets     []string        `json:"toolsets"`
	OutputSchema json.RawMessage `json:"output_schema"`
	Worktree     bool            `json:"worktree"`
	Background   bool            `json:"background"`
}

func (t delegateTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	in, err := decodeInput[delegateInput](input)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Task) == "" {
		return "", errors.New("task is required")
	}
	r := t.r
	name := in.Agent
	if name == "" {
		name = env.Agent
	}
	child, ok := r.Agents[name]
	if !ok {
		return "", fmt.Errorf("unknown agent %q; configured: %s", name, strings.Join(r.agentNames(), ", "))
	}
	sub := &SubturnSpec{Parent: env.Session, ParentOrigin: originFrom(ctx), Task: in.Task, Background: in.Background,
		Turn: llm.CallerFrom(ctx).Turn, Tainted: env.Taint.Marked(), RulesPaused: env.RulesPaused}
	if err := r.checkSubturn(env, child, in, sub); err != nil {
		return "", err
	}
	id := subturnID(env.Session, name, in)
	if _, err := r.Queue.Get(ctx, id); err != nil {
		if in.Worktree {
			wt, err := newWorktree(ctx, env.Workspace, id)
			if err != nil {
				return "", err
			}
			sub.Worktree = wt
		}
		workspace := child.Workspace
		if sub.Worktree != nil {
			workspace = sub.Worktree.Dir
		}
		sess, err := r.NewSessionAt(ctx, name, workspace)
		if err != nil {
			return "", err
		}
		if _, err := r.SubmitTurn(ctx, Turn{ID: id, Kind: KindSubturn, Session: sess.ID, Text: subturnText(in, sub), Sub: sub}); err != nil {
			return "", err
		}
		if r.Wake != nil {
			r.Wake()
		}
	}
	if in.Background {
		return fmt.Sprintf("Started %s in the background (agent %s). Its report will arrive as a new message in this conversation when it finishes; /subagents lists it and can stop it. Carry on meanwhile.", id, name), nil
	}
	return r.wait(ctx, id)
}

// checkSubturn validates the options of a delegate call and fills sub.
func (r *Runner) checkSubturn(env *tools.Env, child *Spec, in delegateInput, sub *SubturnSpec) error {
	if m := strings.TrimSpace(in.Model); m != "" {
		if p, mod, ok := strings.Cut(m, "/"); !ok || p == "" || mod == "" {
			return fmt.Errorf("model %q must be provider/model", m)
		}
		if r.CheckModel != nil {
			if err := r.CheckModel(m); err != nil {
				return fmt.Errorf("model %q: %w", m, err)
			}
		}
		sub.Model = m
	}
	if len(in.Toolsets) > 0 {
		parent := r.Agents[env.Agent]
		var own []string
		if parent != nil {
			own = parent.Toolsets
		}
		for _, ts := range in.Toolsets {
			if !slices.Contains(own, ts) {
				return fmt.Errorf("toolset %q is not one of yours (%s)", ts, strings.Join(own, ", "))
			}
			// A sub-agent never gets more than its own agent has, either.
			if slices.Contains(child.Toolsets, ts) && !slices.Contains(sub.Toolsets, ts) {
				sub.Toolsets = append(sub.Toolsets, ts)
			}
		}
		if len(sub.Toolsets) == 0 {
			return fmt.Errorf("agent %s has none of the toolsets %s", child.Name, strings.Join(in.Toolsets, ", "))
		}
	}
	if len(in.OutputSchema) > 0 && string(in.OutputSchema) != "null" {
		if _, err := parseSchema(in.OutputSchema); err != nil {
			return fmt.Errorf("output_schema: %w", err)
		}
		sub.OutputSchema = in.OutputSchema
	}
	return nil
}

// subturnID maps the same call from the same session to the same job, so a
// parent that crashed and repeats the call reattaches instead of starting
// over. Options join the key only when given, so plain calls keep the IDs
// they always had.
func subturnID(session, name string, in delegateInput) string {
	key := session + "\x00" + name + "\x00" + in.Task + "\x00" + in.Context
	if in.Model != "" || len(in.Toolsets) > 0 || len(in.OutputSchema) > 0 || in.Worktree || in.Background {
		opts, _ := json.Marshal([]any{in.Model, in.Toolsets, in.OutputSchema, in.Worktree, in.Background})
		key += "\x00" + string(opts)
	}
	sum := sha256.Sum256([]byte(key))
	return "sub_" + hex.EncodeToString(sum[:12])
}

const subturnPreamble = "[Task from another agent. Do it fully, then reply with a concise report of what you did, what you found and anything unresolved.]\n"

func subturnText(in delegateInput, sub *SubturnSpec) string {
	text := subturnPreamble + in.Task
	if in.Context != "" {
		text += "\n\nContext:\n" + in.Context
	}
	if sub.Worktree != nil {
		text += fmt.Sprintf("\n\nYou work in a fresh git worktree at %s, on branch %s. Make your changes there; they are committed to that branch when you finish.", sub.Worktree.Dir, sub.Worktree.Branch)
	}
	if len(sub.OutputSchema) > 0 {
		text += "\n\nEnd your reply with your answer as one JSON value matching this JSON Schema (nothing after it):\n```json\n" + string(sub.OutputSchema) + "\n```"
	}
	return text
}

// wait polls a job until it finishes.
func (r *Runner) wait(ctx context.Context, id string) (string, error) {
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		j, err := r.Queue.Get(ctx, id)
		if err != nil {
			return "", err
		}
		switch j.Status {
		case jobs.Done:
			return j.Result, nil
		case jobs.Failed:
			return "", fmt.Errorf("sub-agent failed: %s", j.LastError)
		case jobs.Canceled:
			return "", errors.New("sub-agent task was canceled")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-tick.C:
		}
	}
}

func (r *Runner) agentNames() []string {
	var names []string
	for n := range r.Agents {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func decodeInput[T any](input json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(input, &v); err != nil {
		return v, fmt.Errorf("invalid input: %w", err)
	}
	return v, nil
}

// withoutDelegate drops the delegate tool for sub-turns.
func withoutDelegate(specs []llm.ToolSpec) []llm.ToolSpec {
	out := specs[:0:0]
	for _, s := range specs {
		if s.Name != delegateName {
			out = append(out, s)
		}
	}
	return out
}

// originKey carries the running turn's reply address (its payload's Origin)
// to its tools, so a background sub-turn can answer in the same chat.
type originKey struct{}

func withOrigin(ctx context.Context, origin json.RawMessage) context.Context {
	if len(origin) == 0 {
		return ctx
	}
	return context.WithValue(ctx, originKey{}, origin)
}

func originFrom(ctx context.Context) json.RawMessage {
	o, _ := ctx.Value(originKey{}).(json.RawMessage)
	return o
}

// useSub applies a sub-turn's options to the turn: the model tried first and
// the toolsets offered.
func (t *turn) useSub(sub *SubturnSpec) {
	t.sub = sub
	if sub == nil || sub.Model == "" {
		return
	}
	p, m, _ := strings.Cut(sub.Model, "/")
	first := llm.Target{Endpoint: p, Model: m}
	chain := []llm.Target{first}
	for _, c := range t.chain {
		if c != first {
			chain = append(chain, c)
		}
	}
	t.chain = chain
}

// toolsets are the toolsets the turn offers: the sub-turn's subset, or the
// agent's own.
func (t *turn) toolsets() []string {
	if t.sub != nil && len(t.sub.Toolsets) > 0 {
		return t.sub.Toolsets
	}
	return t.spec.Toolsets
}

// equip points a sub-turn's tools at its worktree.
func (t *turn) equip(env *tools.Env) {
	if t.sub != nil && t.sub.Worktree != nil {
		env.Workspace = t.sub.Worktree.Dir
	}
}

// finishSubturn runs when a sub-turn's loop ends: it holds the answer to the
// output schema (asking once more when it does not match), reports the
// worktree's branch and changes, and hands a background result to the
// parent session as a new turn.
func (t *turn) finishSubturn(p TurnPayload, err error) error {
	sub := p.Sub
	if sub == nil {
		return err
	}
	if err == nil && !t.paused && len(sub.OutputSchema) > 0 {
		err = t.enforceSchema(p.Text, sub.OutputSchema)
	}
	if err == nil && sub.Worktree != nil {
		t.final += sub.Worktree.report(t.ctx, sub.Task)
	}
	if !sub.Background || sub.Parent == "" {
		return err
	}
	if err != nil {
		// Only a sub-turn that will not run again reports; one stopped or
		// interrupted is not finished.
		if errors.Is(err, jobs.ErrLeaseLost) || t.ctx.Err() != nil ||
			(!jobs.IsPermanent(err) && t.job.Attempts < t.job.MaxAttempts) {
			return err
		}
	}
	untrusted := sub.Tainted || sub.RulesPaused || t.r.Untrusted(t.job.ID)
	if nerr := t.r.notifyParent(t.ctx, t.job.ID, t.spec.Name, sub, t.final, err, untrusted); nerr != nil {
		t.r.Log.Warn("background result not handed back", "job", t.job.ID, "err", nerr)
		if err == nil {
			return nerr // retry: the turn is finished, so the rerun only notifies
		}
	}
	return err
}

// BackgroundPrefix starts the parent turn that carries a background result.
const BackgroundPrefix = "[Background task "

// notifyParent queues a turn in the parent session with a background
// sub-turn's result. Its ID derives from the sub-turn, so it is queued once.
func (r *Runner) notifyParent(ctx context.Context, jobID, agentName string, sub *SubturnSpec, final string, failed error, untrusted bool) error {
	var text string
	switch {
	case failed != nil:
		text = fmt.Sprintf("%s%s (agent %s) failed]\nTask: %s\n\nError: %s\n\n[Tell the user what happened and what you will do instead.]",
			BackgroundPrefix, jobID, agentName, preview(sub.Task), failed)
	case final == "":
		text = fmt.Sprintf("%s%s (agent %s) was stopped]\nTask: %s\n\n[It did not finish. Tell the user if it matters.]",
			BackgroundPrefix, jobID, agentName, preview(sub.Task))
	default:
		text = fmt.Sprintf("%s%s (agent %s) finished]\nTask: %s\n\nReport:\n%s\n\n[Act on it, or tell the user what matters from it.]",
			BackgroundPrefix, jobID, agentName, preview(sub.Task), final)
	}
	if _, err := r.SubmitTurn(ctx, Turn{ID: "bg_" + strings.TrimPrefix(jobID, "sub_"), Session: sub.Parent, Text: text,
		Origin: sub.ParentOrigin, Background: true, Tainted: untrusted}); err != nil {
		return err
	}
	if r.Wake != nil {
		r.Wake()
	}
	return nil
}
