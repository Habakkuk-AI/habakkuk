// Package tools holds Habakkuk's tool registry, permission policy and the
// built-in tools. Every tool declares its risk; the registry decides whether a
// call runs, needs approval, or is denied, before the tool sees the input.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

type Risk int

const (
	RiskRead    Risk = iota // reads local state
	RiskWrite               // changes files
	RiskNetwork             // reaches the network
	RiskExec                // runs arbitrary programs
)

type Mode string

const (
	ModeReadOnly  Mode = "read-only"
	ModeAsk       Mode = "ask"
	ModeWorkspace Mode = "workspace"
	ModeFull      Mode = "full"
)

type Tool interface {
	Spec() llm.ToolSpec
	Risk() Risk
	// Run executes the call. Errors are returned to the model as error results.
	Run(ctx context.Context, env *Env, input json.RawMessage) (string, error)
}

// PathTool is implemented by tools that act on one path, so the policy can
// allow writes inside the workspace without asking.
type PathTool interface {
	TargetPath(input json.RawMessage) (string, error)
}

// OwnStateTool marks write tools that only change the agent's own state under
// $HABAKKUK_HOME/agents/<agent> (its memory and skills), never the user's
// files, so workspace mode lets them run without asking.
type OwnStateTool interface {
	OwnState()
}

// PathsTool is implemented by tools that act on several paths (apply_patch).
// Workspace mode allows the call without asking only when every path is
// inside the workspace.
type PathsTool interface {
	TargetPaths(input json.RawMessage) ([]string, error)
}

// Mutator is implemented by tools that may change files in the workspace.
// The first such call in a turn snapshots the workspace (Env.Checkpoints).
type Mutator interface {
	Mutates(env *Env, input json.RawMessage) bool
}

// LocalFileTool marks tools that write local files. They are refused when
// the agent's commands run on a remote host, where those files are not.
type LocalFileTool interface {
	WritesLocalFiles()
}

// ConfirmTool is implemented by tools whose calls sometimes need a human's
// approval whatever the mode, full included: computer actions on the apps
// in computer.confirm_apps. Confirm returns the reason, or "" when the call
// follows the mode as usual. A reviewer cannot approve such a call alone.
type ConfirmTool interface {
	Confirm(ctx context.Context, env *Env, input json.RawMessage) string
}

// Checkpointer snapshots an agent's workspace so a turn's changes can be
// rolled back. Failures are its own to log; a call never fails over one.
type Checkpointer interface {
	Checkpoint(ctx context.Context, env *Env)
}

type ApprovalRequest struct {
	Tool    string
	Summary string
	Input   json.RawMessage
	// For reviewers: the session, the task the agent is working on (the
	// turn's user message) and its workspace.
	Session   string
	Task      string
	Workspace string
	// Review is the reviewer's reason when it handed the call to a human.
	Review string
	// Tainted: the turn has read content from outside (a web page, an
	// email). Shortcuts in review are off for such calls.
	Tainted bool
	// Cause says why a human is asked, for approvers that explain it in
	// their own words (a chat prompt); "" is the permission mode.
	Cause Cause
}

// Cause is why a call needs a human's approval.
type Cause string

const (
	CauseTainted Cause = "tainted" // the turn read outside content, so the risky capabilities need a human
	CauseChannel Cause = "channel" // the turn came from a messaging channel that may not use the capability unasked
	CauseConfirm Cause = "confirm" // the tool always asks for this call (ConfirmTool)
)

// Approver decides calls the mode does not allow outright. Unattended runs
// have no approver, and such calls are denied. An approver that finds no one
// to ask returns ErrNoApprover.
type Approver interface {
	Approve(ctx context.Context, req ApprovalRequest) (bool, error)
}

// ErrNoApprover means there is nobody to ask in this run.
var ErrNoApprover = errors.New("no one is attending this run")

// Approvals says who decides calls that need approval.
type Approvals string

const (
	ApproveHuman             Approvals = "human"               // the attached client or chat (default)
	ApproveReviewer          Approvals = "reviewer"            // a model alone; unsure means deny
	ApproveReviewerThenHuman Approvals = "reviewer_then_human" // a model, then a human when it is unsure
)

// Verdict is a reviewer's decision.
type Verdict string

const (
	VerdictAllow Verdict = "allow"
	VerdictDeny  Verdict = "deny"
	VerdictAsk   Verdict = "ask" // unsure, or its answer was unusable
)

type Review struct {
	Verdict Verdict
	Reason  string
}

// Reviewer judges a call in place of, or before, a human. It never fails:
// anything it cannot decide is VerdictAsk.
type Reviewer interface {
	Review(ctx context.Context, req ApprovalRequest) Review
}

type Env struct {
	Agent     string
	Session   string // the session the call runs in
	Workspace string
	Mode      Mode
	Approver  Approver
	Approvals Approvals // who approves; empty means ApproveHuman
	Reviewer  Reviewer  // used when Approvals names the reviewer
	Task      string    // the user message the turn works on, for the reviewer
	Blobs     *store.Blobs
	BlobDir   string
	// Files, when set, is where read_file, write_file and edit_file read
	// and write text, in place of the local disk: an editor client serves
	// its open buffers, unsaved edits included.
	Files Files

	// Backend runs exec and background processes; nil runs them here.
	Backend Backend
	// Procs tracks background processes; nil disables background exec.
	Procs *Procs
	// Checkpoints snapshots the workspace before a call that may change it.
	// Wrap it with OncePerTurn so a turn takes one snapshot.
	Checkpoints Checkpointer
	// Taint is set once external content enters the turn; nil never is.
	Taint *Taint
	// AllowPrivateURLs lets fetch_url reach loopback, private, link-local
	// and metadata addresses (config tools.allow_private_urls).
	AllowPrivateURLs bool
	// Policy is what this turn may do with the risky capabilities, from
	// where it came from (policy.go); nil is a turn started on this
	// machine.
	Policy *Policy
}

// Files reads and writes text files through a client. Paths are absolute.
type Files interface {
	ReadTextFile(ctx context.Context, path string) (string, error)
	WriteTextFile(ctx context.Context, path, content string) error
}

// checkpoint snapshots the workspace before a call that may change it.
func (e *Env) checkpoint(ctx context.Context, t Tool, input json.RawMessage) {
	if e.Checkpoints == nil {
		return
	}
	if m, ok := t.(Mutator); ok && m.Mutates(e, input) {
		e.Checkpoints.Checkpoint(ctx, e)
	}
}

// OncePerTurn wraps c so it snapshots only on its first call; concurrent
// calls wait for that one. Give each turn's Env a fresh wrapper.
func OncePerTurn(c Checkpointer) Checkpointer { return &oncePerTurn{c: c} }

type oncePerTurn struct {
	c    Checkpointer
	once sync.Once
}

func (o *oncePerTurn) Checkpoint(ctx context.Context, env *Env) {
	o.once.Do(func() { o.c.Checkpoint(ctx, env) })
}

// remote reports whether commands run somewhere the local files are not.
func (e *Env) remote() bool { return e.Backend != nil && e.Backend.Remote() }

// Resolve turns a model-supplied path into an absolute, cleaned path, relative
// to the workspace.
func (e *Env) Resolve(p string) string {
	if p == "" {
		return e.Workspace
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Clean(filepath.Join(homeDir(), p[2:]))
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(e.Workspace, p)
	}
	return filepath.Clean(p)
}

// InWorkspace reports whether abs is the workspace or inside it. The
// separator is the platform's: on Windows a path that leaves the workspace
// is "..\\x", not "../x".
func (e *Env) InWorkspace(abs string) bool {
	rel, err := filepath.Rel(e.Workspace, abs)
	return err == nil && !filepath.IsAbs(rel) && rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

type Registry struct {
	mu       sync.RWMutex
	tools    map[string]Tool
	toolsets map[string][]string
	// MaxInline is the largest result kept in the transcript; larger results
	// are saved as blobs and the model gets the head and tail plus a path.
	MaxInline int
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}, toolsets: map[string][]string{}, MaxInline: 30000}
}

func (r *Registry) Register(toolset string, t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := t.Spec().Name
	if _, dup := r.tools[name]; dup {
		panic("tools: duplicate tool " + name)
	}
	r.tools[name] = t
	r.toolsets[toolset] = append(r.toolsets[toolset], name)
}

// RegisterOrReplace adds t to the toolset, or swaps in t for the tool of the
// same name. It is for tools discovered at run time (MCP servers), which can
// come up late or reconnect; built-ins use Register.
func (r *Registry) RegisterOrReplace(toolset string, t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := t.Spec().Name
	r.tools[name] = t
	if !slices.Contains(r.toolsets[toolset], name) {
		r.toolsets[toolset] = append(r.toolsets[toolset], name)
	}
}

// Specs returns the tools in the given toolsets, sorted by name so the
// request prefix (and therefore the prompt cache) is stable.
func (r *Registry) Specs(toolsets []string) []llm.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := map[string]bool{}
	var names []string
	for _, ts := range toolsets {
		for _, n := range r.toolsets[ts] {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	sort.Strings(names)
	specs := make([]llm.ToolSpec, 0, len(names))
	for _, n := range names {
		specs = append(specs, r.tools[n].Spec())
	}
	return specs
}

type callIDKey struct{}

// WithCallID returns a context carrying the id of the tool call being run.
func WithCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callIDKey{}, id)
}

// CallID is the id of the tool call being run, or "". Tools with side
// effects derive idempotency keys from it.
func CallID(ctx context.Context) string {
	id, _ := ctx.Value(callIDKey{}).(string)
	return id
}

// Execute runs one tool call and always returns a tool_result block; failures
// become error results the model can read and react to.
func (r *Registry) Execute(ctx context.Context, env *Env, call llm.Block) llm.Block {
	ctx = WithCallID(ctx, call.ToolUseID)
	res := llm.Block{Type: llm.ToolResult, ToolUseID: call.ToolUseID}
	r.mu.RLock()
	t, ok := r.tools[call.ToolName]
	r.mu.RUnlock()
	if !ok {
		res.IsError, res.Text = true, fmt.Sprintf("unknown tool %q", call.ToolName)
		return res
	}
	if len(call.Input) == 0 || !json.Valid(call.Input) {
		res.IsError, res.Text = true, "tool input was not valid JSON (possibly truncated); send the call again"
		return res
	}
	if err := hardlineCheck(t, call.Input); err != nil {
		res.IsError, res.Text = true, err.Error()
		return res
	}
	if _, ok := t.(LocalFileTool); ok && env.remote() {
		res.IsError, res.Text = true, fmt.Sprintf("%s is not available: this agent's commands run on %s, so local file writes would not reach them; change files there with exec", call.ToolName, env.Backend.Describe())
		return res
	}
	note, err := r.authorize(ctx, env, t, call)
	if err != nil {
		res.IsError, res.Text = true, err.Error()
		return res
	}
	env.checkpoint(ctx, t, call.Input)
	out, err := t.Run(ctx, env, call.Input)
	if taints(t) {
		env.Taint.Mark()
	}
	if err != nil {
		res.IsError = true
		if out != "" {
			out += "\n"
		}
		out += "error: " + err.Error()
	}
	res.Text = r.budget(env, out)
	if note != "" {
		res.Text += "\n\n" + note
	}
	return res
}

var ErrDenied = errors.New("denied by permission policy")

// authorize returns nil when the call may run. A call the reviewer allowed
// also gets a note for its result, so the transcript records why it ran.
func (r *Registry) authorize(ctx context.Context, env *Env, t Tool, call llm.Block) (string, error) {
	risk := t.Risk()
	var confirm string
	if ct, ok := t.(ConfirmTool); ok {
		confirm = ct.Confirm(ctx, env, call.Input)
	}
	if cp := r.capability(ctx, env, t, call); cp != "" {
		// A note written to one of the agent's trusted folders is not held
		// for approval because the turn came from a chat or read outside
		// content (trusted.go); the mode still decides.
		trusted := false
		if cp == CapWrite {
			var err error
			if trusted, err = trustedWrite(env, t, call); err != nil {
				return "", err
			}
		}
		if trusted {
			switch env.Mode {
			case ModeWorkspace, ModeFull:
				return "", nil
			}
		} else if note, handled, err := gate(ctx, env, cp, risk, call, confirm); handled {
			return note, err
		}
	}
	if confirm == "" && (risk == RiskRead || env.Mode == ModeFull) {
		return "", nil
	}
	if env.Mode == ModeReadOnly {
		return "", fmt.Errorf("%w: %s is not allowed in read-only mode", ErrDenied, call.ToolName)
	}
	if confirm != "" {
		return "", confirmByHuman(ctx, env, call, confirm, CauseConfirm)
	}
	if env.Mode == ModeWorkspace {
		switch risk {
		case RiskNetwork:
			return "", nil
		case RiskWrite:
			if _, ok := t.(OwnStateTool); ok {
				return "", nil
			}
			if pt, ok := t.(PathTool); ok {
				if p, err := pt.TargetPath(call.Input); err == nil && env.InWorkspace(env.Resolve(p)) {
					return "", nil
				}
			}
			if pt, ok := t.(PathsTool); ok {
				ps, err := pt.TargetPaths(call.Input)
				if err != nil {
					return "", err // malformed input: nothing to approve
				}
				if len(ps) > 0 && allIn(env, ps) {
					return "", nil
				}
			}
		}
	}
	return Ask(ctx, env, ApprovalRequest{Tool: call.ToolName, Summary: summarize(call.Input), Input: call.Input})
}

// Ask puts a call to whoever approves this run's calls: the reviewer model
// and then a human, as env.Approvals says. It returns nil when the call
// may go ahead, with a note for the result when the reviewer allowed it.
// Tools that gate their own actions (node_invoke) use it directly.
func Ask(ctx context.Context, env *Env, req ApprovalRequest) (string, error) {
	req.Session, req.Task, req.Workspace = env.Session, env.Task, env.Workspace
	req.Tainted = env.Taint.Marked()
	unattended := fmt.Errorf("%w: %s needs approval and no one is attending this run", ErrDenied, req.Tool)
	if (env.Approvals == ApproveReviewer || env.Approvals == ApproveReviewerThenHuman) && env.Reviewer != nil {
		rv := env.Reviewer.Review(ctx, req)
		switch rv.Verdict {
		case VerdictAllow:
			return "[approved by the reviewer: " + rv.Reason + "]", nil
		case VerdictDeny:
			return "", fmt.Errorf("%w: the reviewer denied %s: %s (find a safer way, or ask the user)", ErrDenied, req.Tool, rv.Reason)
		}
		if env.Approvals == ApproveReviewer {
			return "", fmt.Errorf("%w: the reviewer would not approve %s on its own (%s) and no human reviews this agent's calls", ErrDenied, req.Tool, rv.Reason)
		}
		req.Review = rv.Reason
	}
	if env.Approver == nil {
		return "", unattended
	}
	ok, err := env.Approver.Approve(ctx, req)
	if errors.Is(err, ErrNoApprover) {
		return "", unattended
	}
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w: the user declined %s", ErrDenied, req.Tool)
	}
	return "", nil
}

// confirmByHuman asks the attached human about a call a ConfirmTool marked;
// no mode, and no reviewer, allows it alone.
func confirmByHuman(ctx context.Context, env *Env, call llm.Block, why string, cause Cause) error {
	unattended := fmt.Errorf("%w: %s needs a human's approval (%s) and no one is attending this run", ErrDenied, call.ToolName, why)
	if env.Approver == nil {
		return unattended
	}
	ok, err := env.Approver.Approve(ctx, ApprovalRequest{Tool: call.ToolName, Summary: "[" + why + "] " + summarize(call.Input),
		Input: call.Input, Session: env.Session, Task: env.Task, Workspace: env.Workspace, Review: why, Cause: cause,
		Tainted: env.Taint.Marked()})
	switch {
	case errors.Is(err, ErrNoApprover):
		return unattended
	case err != nil:
		return err
	case !ok:
		return fmt.Errorf("%w: the user declined %s", ErrDenied, call.ToolName)
	}
	return nil
}

func (r *Registry) budget(env *Env, out string) string {
	if len(out) <= r.MaxInline || env.Blobs == nil {
		return out
	}
	ref, err := env.Blobs.Put([]byte(out))
	if err != nil {
		return out[:r.MaxInline] + "\n[output truncated]"
	}
	id := strings.TrimPrefix(ref, "sha256:")
	path := filepath.Join(env.BlobDir, id[:2], id)
	half := r.MaxInline / 2
	return fmt.Sprintf("%s\n\n[... %d characters omitted; full output saved at %s, read it with read_file using offset/limit ...]\n\n%s",
		out[:half], len(out)-2*half, path, out[len(out)-half:])
}

func allIn(env *Env, paths []string) bool {
	for _, p := range paths {
		if !env.InWorkspace(env.Resolve(p)) {
			return false
		}
	}
	return true
}

func summarize(input json.RawMessage) string {
	s := string(input)
	if len(s) > 400 {
		s = s[:400] + "..."
	}
	return s
}
