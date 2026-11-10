// Package acp serves the Agent Client Protocol, so a code editor can use a
// Habakkuk agent as its coding agent: `hk acp` speaks JSON-RPC on stdin
// and stdout. A session's working directory becomes the agent's workspace,
// tool calls and plans stream to the editor, approvals become the editor's
// permission prompts, and file tools read and write through the editor (its
// unsaved buffers included) when it offers to.
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// ProtocolVersion is the ACP version this agent speaks.
const ProtocolVersion = 1

// KindTurn is the job kind of an editor's turns. Only the process serving
// the editor runs them, since their file access and approvals go through
// its connection.
const KindTurn = "acp_turn"

// Toolset holds the tools only editor sessions get (update_plan).
const Toolset = "acp"

// Agent serves one editor connection.
type Agent struct {
	Agent   string // the Habakkuk agent editor sessions use
	Version string
	Log     *slog.Logger

	runner *agent.Runner // a copy of the app's runner with editor-only tools and file access
	conn   *conn

	mu       sync.Mutex
	caps     fsCaps
	sessions map[string]*session
	running  map[string]context.CancelFunc // session -> cancel for the turn job running here
}

type fsCaps struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

// session is an editor session: a Habakkuk session plus its folder.
type session struct {
	id     string
	cwd    string
	detach func() // removes the session's approver

	mu        sync.Mutex
	job       string // the prompt's turn job while one runs
	cancelled bool
	always    map[string]bool // tools the user allowed for the rest of the session
}

// New prepares an editor agent on top of the app's runner. It registers
// update_plan in reg (once per process) and gives every agent the editor
// toolset, in a runner of its own so other sessions never see it.
func New(base *agent.Runner, reg *tools.Registry, agentName, version string, log *slog.Logger) (*Agent, error) {
	if _, ok := base.Agents[agentName]; !ok {
		return nil, fmt.Errorf("unknown agent %q", agentName)
	}
	a := &Agent{Agent: agentName, Version: version, Log: log,
		sessions: map[string]*session{}, running: map[string]context.CancelFunc{}}
	reg.Register(Toolset, planTool{a})
	specs := map[string]*agent.Spec{}
	for name, s := range base.Agents {
		c := *s
		c.Toolsets = append(slices.Clone(s.Toolsets), Toolset)
		specs[name] = &c
	}
	// Everything that decides what a turn may do comes from the base
	// runner: the tool policy (locked folders, trusted folders, channel
	// rules), the owner's trusted project files, hooks (a tool_before
	// hook can veto a call), spending limits, chat notes and the rest.
	// Only the agents (with the editor toolset) and the environment (the
	// editor's folder) differ.
	a.runner = &agent.Runner{
		Agents: specs, Sessions: base.Sessions, Queue: base.Queue, Models: base.Models, Tools: base.Tools,
		Bus: base.Bus, BlobDir: base.BlobDir, Media: base.Media, Wake: base.Wake, Log: base.Log, Now: base.Now,
		Location: base.Location, Snapshot: base.Snapshot, Env: a.env(base.Env),
		Approvers: base.Approvers, Overlay: base.Overlay, Hooks: base.Hooks, Policy: base.Policy,
		Judge: base.Judge, Home: base.Home, Choices: base.Choices, CheckModel: base.CheckModel,
		Budget: base.Budget, Notes: base.Notes, ProjectTrust: base.ProjectTrust, ToolsPending: base.ToolsPending,
	}
	return a, nil
}

// SetWake sets how a new turn wakes the workers.
func (a *Agent) SetWake(wake func()) { a.runner.Wake = wake }

// Handle is the jobs.Handler for KindTurn. It runs the turn under a context
// session/cancel can end at once, rather than at the next heartbeat.
func (a *Agent) Handle(ctx context.Context, j *jobs.Job, owner string) (string, error) {
	var p agent.TurnPayload
	if err := j.Decode(&p); err != nil {
		return "", jobs.Permanent{Err: err}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.mu.Lock()
	a.running[p.Session] = cancel
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.running, p.Session)
		a.mu.Unlock()
	}()
	return a.runner.HandleTurn(ctx, j, owner)
}

// env gives an editor session its folder as the workspace and, when the
// editor offers, its file system.
func (a *Agent) env(base func(*agent.Spec, string) *tools.Env) func(*agent.Spec, string) *tools.Env {
	return func(spec *agent.Spec, sessionID string) *tools.Env {
		env := base(spec, sessionID)
		if s := a.session(sessionID); s != nil {
			env.Workspace = s.cwd
			env.Files = &clientFiles{a: a, session: sessionID}
		}
		return env
	}
}

func (a *Agent) session(id string) *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}

// Serve speaks ACP on in and out until the editor disconnects.
func (a *Agent) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	a.conn = newConn(in, out)
	err := a.conn.serve(ctx, a.handle)
	a.mu.Lock()
	sessions := slices.Collect(maps.Values(a.sessions))
	a.mu.Unlock()
	for _, s := range sessions {
		s.detach()
	}
	return err
}

func (a *Agent) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return a.initialize(params)
	case "authenticate":
		return map[string]any{}, nil
	case "session/new":
		return a.newSession(ctx, params)
	case "session/load":
		return a.loadSession(ctx, params)
	case "session/prompt":
		return a.prompt(ctx, params)
	case "session/cancel":
		a.cancel(ctx, params)
		return nil, nil
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + method}
}

func decode(params json.RawMessage, v any) error {
	if err := json.Unmarshal(params, v); err != nil {
		return invalidParams("bad params: %v", err)
	}
	return nil
}

func (a *Agent) initialize(params json.RawMessage) (any, error) {
	var in struct {
		ProtocolVersion    int `json:"protocolVersion"`
		ClientCapabilities struct {
			FS fsCaps `json:"fs"`
		} `json:"clientCapabilities"`
	}
	if err := decode(params, &in); err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.caps = in.ClientCapabilities.FS
	a.mu.Unlock()
	return map[string]any{
		"protocolVersion": ProtocolVersion, // the latest we speak; the client decides whether it can
		"agentCapabilities": map[string]any{
			"loadSession":        true,
			"promptCapabilities": map[string]bool{"image": false, "audio": false, "embeddedContext": true},
			"mcpCapabilities":    map[string]bool{"http": false, "sse": false},
		},
		"authMethods": []any{},
		"agentInfo":   map[string]string{"name": "habakkuk", "title": "Habakkuk", "version": a.Version},
	}, nil
}

type sessionParams struct {
	SessionID  string            `json:"sessionId"`
	Cwd        string            `json:"cwd"`
	MCPServers []json.RawMessage `json:"mcpServers"`
}

func (p *sessionParams) check() error {
	if p.Cwd == "" || !filepath.IsAbs(p.Cwd) {
		return invalidParams("cwd must be an absolute path")
	}
	p.Cwd = filepath.Clean(p.Cwd)
	return nil
}

func (a *Agent) register(id, cwd string) {
	s := &session{id: id, cwd: cwd, always: map[string]bool{}}
	s.detach = a.runner.Attach(id, &approver{a: a, s: s})
	a.mu.Lock()
	if old := a.sessions[id]; old != nil { // loaded again
		old.detach = func() {}
	}
	a.sessions[id] = s
	a.mu.Unlock()
}

func (a *Agent) newSession(ctx context.Context, params json.RawMessage) (any, error) {
	var p sessionParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if err := p.check(); err != nil {
		return nil, err
	}
	if len(p.MCPServers) > 0 {
		a.Log.Info("acp: ignoring the editor's MCP servers; agents use the ones in Habakkuk's config", "count", len(p.MCPServers))
	}
	sess, err := a.runner.NewSessionAt(ctx, a.Agent, p.Cwd)
	if err != nil {
		return nil, err
	}
	a.register(sess.ID, p.Cwd)
	a.Log.Info("acp session", "session", sess.ID, "cwd", p.Cwd)
	return map[string]any{"sessionId": sess.ID}, nil
}

// loadSession reopens a Habakkuk session in the editor and replays its
// conversation as session/update notifications before answering.
func (a *Agent) loadSession(ctx context.Context, params json.RawMessage) (any, error) {
	var p sessionParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if err := p.check(); err != nil {
		return nil, err
	}
	sess, err := a.runner.Sessions.Get(ctx, p.SessionID)
	if err != nil {
		return nil, invalidParams("%v", err)
	}
	if _, ok := a.runner.Agents[sess.Agent]; !ok {
		return nil, invalidParams("session %s belongs to agent %q, which is not configured", sess.ID, sess.Agent)
	}
	recs, err := a.runner.Sessions.Messages(ctx, sess.ID)
	if err != nil {
		return nil, err
	}
	a.register(sess.ID, p.Cwd)
	for _, r := range recs {
		for _, b := range r.Message.Blocks {
			var u map[string]any
			switch {
			case b.Type == llm.Text && r.Message.Role == llm.User:
				u = chunk("user_message_chunk", unstamp(b.Text))
			case b.Type == llm.Text:
				u = chunk("agent_message_chunk", b.Text)
			case b.Type == llm.ToolUse:
				title, kind, locs := describe(b.ToolName, b.Input, p.Cwd)
				u = map[string]any{"sessionUpdate": "tool_call", "toolCallId": b.ToolUseID, "title": title, "kind": kind,
					"status": "completed", "locations": locs}
			case b.Type == llm.ToolResult && b.IsError:
				u = map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": b.ToolUseID, "status": "failed"}
			}
			if u != nil {
				a.update(sess.ID, u)
			}
		}
	}
	return map[string]any{}, nil
}

// unstamp drops the time line Habakkuk puts at the top of a user message.
func unstamp(text string) string {
	if strings.HasPrefix(text, "[") {
		if first, rest, ok := strings.Cut(text, "\n"); ok && strings.HasSuffix(first, "]") {
			return rest
		}
	}
	return text
}

func chunk(kind, text string) map[string]any {
	return map[string]any{"sessionUpdate": kind, "content": map[string]string{"type": "text", "text": text}}
}

// update sends one session/update notification.
func (a *Agent) update(sessionID string, u map[string]any) {
	if err := a.conn.notify("session/update", map[string]any{"sessionId": sessionID, "update": u}); err != nil {
		a.Log.Debug("acp update failed", "err", err)
	}
}

type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	URI      string `json:"uri"`
	Name     string `json:"name"`
	Resource *struct {
		URI      string `json:"uri"`
		Text     string `json:"text"`
		Blob     string `json:"blob"`
		MimeType string `json:"mimeType"`
	} `json:"resource"`
}

// promptText turns the editor's content blocks into the user message.
func promptText(blocks []contentBlock) (string, error) {
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "resource_link":
			where := b.URI
			if p, ok := strings.CutPrefix(b.URI, "file://"); ok {
				where = p
			}
			parts = append(parts, fmt.Sprintf("[Referenced: %s (%s)]", b.Name, where))
		case "resource":
			if b.Resource == nil {
				return "", invalidParams("resource block without a resource")
			}
			if b.Resource.Blob != "" && b.Resource.Text == "" {
				parts = append(parts, fmt.Sprintf("[Attached binary resource %s, not included]", b.Resource.URI))
				continue
			}
			parts = append(parts, fmt.Sprintf("<context uri=%q>\n%s\n</context>", b.Resource.URI, b.Resource.Text))
		default:
			return "", invalidParams("content type %q is not supported", b.Type)
		}
	}
	text := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if text == "" {
		return "", invalidParams("the prompt is empty")
	}
	return text, nil
}

func (a *Agent) prompt(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		SessionID string         `json:"sessionId"`
		Prompt    []contentBlock `json:"prompt"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	s := a.session(p.SessionID)
	if s == nil {
		return nil, invalidParams("unknown session %q; create it with session/new or session/load", p.SessionID)
	}
	text, err := promptText(p.Prompt)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.job != "" {
		s.mu.Unlock()
		return nil, invalidParams("a prompt is already running in this session")
	}
	s.cancelled = false
	s.job = "starting"
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.job = ""
		s.mu.Unlock()
	}()

	var evs <-chan events.Event
	if a.runner.Bus != nil {
		ch, unsub := a.runner.Bus.Subscribe(s.id)
		defer unsub()
		evs = ch
	}
	job, err := a.runner.Begin(ctx, agent.Turn{Kind: KindTurn, Session: s.id, Text: text})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.job = job
	cancelled := s.cancelled
	s.mu.Unlock()
	if cancelled { // session/cancel arrived while the turn was being queued
		a.stop(ctx, s.id, job)
	}
	_, err = a.runner.Await(ctx, job, evs, func(e events.Event) { a.forward(s, e) })
	s.mu.Lock()
	cancelled = s.cancelled
	s.mu.Unlock()
	switch {
	case err == nil:
		return map[string]string{"stopReason": "end_turn"}, nil
	case cancelled || errors.Is(err, agent.ErrCanceled):
		return map[string]string{"stopReason": "cancelled"}, nil
	case ctx.Err() != nil: // the editor went away: do not keep working unseen
		a.stop(context.WithoutCancel(ctx), s.id, job)
		return nil, ctx.Err()
	case strings.Contains(err.Error(), "iterations without finishing"):
		return map[string]string{"stopReason": "max_turn_requests"}, nil
	}
	return nil, err
}

// forward turns a turn's live events into session updates.
func (a *Agent) forward(s *session, e events.Event) {
	switch e.Kind {
	case events.Text:
		if e.Text != "" {
			a.update(s.id, chunk("agent_message_chunk", e.Text))
		}
	case events.Thinking, events.Review:
		if e.Text != "" {
			a.update(s.id, chunk("agent_thought_chunk", e.Text))
		}
	case events.ToolStart:
		if e.ToolID == "" || e.Tool == planName {
			return // the plan shows as a plan, not as a tool call
		}
		title, kind, locs := describe(e.Tool, e.Input, s.cwd)
		u := map[string]any{"sessionUpdate": "tool_call", "toolCallId": e.ToolID, "title": title, "kind": kind,
			"status": "pending", "locations": locs}
		if raw := rawObject(e.Input); raw != nil {
			u["rawInput"] = raw
		}
		a.update(s.id, u)
	case events.ToolDone:
		if e.ToolID == "" || e.Tool == planName {
			return
		}
		status := "completed"
		if e.IsError {
			status = "failed"
		}
		u := map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": e.ToolID, "status": status}
		if out := strings.TrimPrefix(e.Text, e.Tool+": "); out != "" {
			u["content"] = []any{map[string]any{"type": "content", "content": map[string]string{"type": "text", "text": out}}}
		}
		a.update(s.id, u)
	}
}

func rawObject(input json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(input))
	if strings.HasPrefix(trimmed, "{") && json.Valid(input) {
		return input
	}
	return nil
}

// cancel handles session/cancel: the turn stops now, and the prompt answers
// with stopReason "cancelled".
func (a *Agent) cancel(ctx context.Context, params json.RawMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	s := a.session(p.SessionID)
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cancelled = true
	job := s.job
	s.mu.Unlock()
	if job != "" && job != "starting" {
		a.stop(ctx, s.id, job)
	}
}

// stop cancels a turn job and, if it runs here, its context.
func (a *Agent) stop(ctx context.Context, sessionID, job string) {
	if err := a.runner.Queue.Cancel(context.WithoutCancel(ctx), job); err != nil {
		a.Log.Warn("acp cancel failed", "err", err)
	}
	a.mu.Lock()
	cancel := a.running[sessionID]
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// describe gives a tool call a title, an ACP tool kind and the files it
// touches.
func describe(tool string, input json.RawMessage, cwd string) (title, kind string, locs []map[string]any) {
	var in struct {
		Path    string `json:"path"`
		Command string `json:"command"`
		URL     string `json:"url"`
		Query   string `json:"query"`
		Task    string `json:"task"`
		Name    string `json:"name"`
	}
	_ = json.Unmarshal(input, &in)
	path := in.Path
	if path != "" && !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	locs = []map[string]any{}
	if path != "" {
		locs = append(locs, map[string]any{"path": path})
	}
	short := func(s string) string {
		s = strings.Join(strings.Fields(s), " ")
		if len(s) > 80 {
			s = s[:80] + "..."
		}
		return s
	}
	switch tool {
	case "read_file":
		return "Read " + in.Path, "read", locs
	case "list_dir":
		return "List " + or(in.Path, "."), "read", locs
	case "write_file":
		return "Write " + in.Path, "edit", locs
	case "edit_file":
		return "Edit " + in.Path, "edit", locs
	case "exec":
		return "Run `" + short(in.Command) + "`", "execute", locs
	case "fetch_url":
		return "Fetch " + in.URL, "fetch", locs
	case "memory_search":
		return "Search memory for " + short(in.Query), "search", locs
	case "delegate":
		return "Delegate: " + short(in.Task), "think", locs
	case "skill_view":
		return "Load skill " + in.Name, "read", locs
	}
	return tool, "other", locs
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// approver asks the editor through session/request_permission.
type approver struct {
	a *Agent
	s *session
}

func (ap *approver) Approve(ctx context.Context, req tools.ApprovalRequest) (bool, error) {
	ap.s.mu.Lock()
	always := ap.s.always[req.Tool]
	ap.s.mu.Unlock()
	if always {
		return true, nil
	}
	id := tools.CallID(ctx)
	title, kind, locs := describe(req.Tool, req.Input, ap.s.cwd)
	if req.Review != "" {
		title += " (reviewer unsure: " + req.Review + ")"
	}
	call := map[string]any{"toolCallId": id, "title": title, "kind": kind, "status": "pending", "locations": locs}
	if raw := rawObject(req.Input); raw != nil {
		call["rawInput"] = raw
	}
	var res struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	err := ap.a.conn.call(ctx, "session/request_permission", map[string]any{
		"sessionId": ap.s.id,
		"toolCall":  call,
		"options": []map[string]string{
			{"optionId": "allow_once", "name": "Allow", "kind": "allow_once"},
			{"optionId": "allow_always", "name": "Always allow " + req.Tool + " in this session", "kind": "allow_always"},
			{"optionId": "reject_once", "name": "Deny", "kind": "reject_once"},
		},
	}, &res)
	if errors.Is(err, errClosed) {
		return false, tools.ErrNoApprover
	}
	if err != nil {
		return false, err
	}
	if res.Outcome.Outcome != "selected" {
		return false, nil // cancelled
	}
	switch res.Outcome.OptionID {
	case "allow_always":
		ap.s.mu.Lock()
		ap.s.always[req.Tool] = true
		ap.s.mu.Unlock()
		fallthrough
	case "allow_once":
		if id != "" {
			ap.a.update(ap.s.id, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": id, "status": "in_progress"})
		}
		return true, nil
	}
	return false, nil
}

// clientFiles reads and writes through the editor when it offers to, and
// the local disk otherwise.
type clientFiles struct {
	a       *Agent
	session string
}

func (f *clientFiles) ReadTextFile(ctx context.Context, path string) (string, error) {
	f.a.mu.Lock()
	ok := f.a.caps.ReadTextFile
	f.a.mu.Unlock()
	if !ok {
		data, err := os.ReadFile(path)
		return string(data), err
	}
	var res struct {
		Content string `json:"content"`
	}
	if err := f.a.conn.call(ctx, "fs/read_text_file", map[string]any{"sessionId": f.session, "path": path}, &res); err != nil {
		return "", fmt.Errorf("the editor could not read %s: %w", path, err)
	}
	return res.Content, nil
}

func (f *clientFiles) WriteTextFile(ctx context.Context, path, content string) error {
	f.a.mu.Lock()
	ok := f.a.caps.WriteTextFile
	f.a.mu.Unlock()
	if !ok {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		perm := os.FileMode(0o644)
		if st, err := os.Stat(path); err == nil {
			perm = st.Mode().Perm()
		}
		return store.WriteFileAtomic(path, []byte(content), perm)
	}
	if err := f.a.conn.call(ctx, "fs/write_text_file", map[string]any{"sessionId": f.session, "path": path, "content": content}, nil); err != nil {
		return fmt.Errorf("the editor could not write %s: %w", path, err)
	}
	return nil
}

const planName = "update_plan"

// planTool shows the agent's plan in the editor.
type planTool struct{ a *Agent }

func (planTool) Risk() tools.Risk { return tools.RiskRead }
func (planTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: planName,
		Description: "Show the user your plan for the current task as a checklist in their editor, and keep it current. " +
			"Use it for work with several steps: send the whole list each time, with each step pending, in_progress or completed.",
		Schema: json.RawMessage(`{"type":"object","properties":{
			"entries":{"type":"array","items":{"type":"object","properties":{
				"content":{"type":"string","description":"the step, in a few words"},
				"status":{"type":"string","enum":["pending","in_progress","completed"]},
				"priority":{"type":"string","enum":["high","medium","low"]}
			},"required":["content","status"]}}
		},"required":["entries"]}`),
	}
}

func (t planTool) Run(_ context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	var in struct {
		Entries []struct {
			Content  string `json:"content"`
			Status   string `json:"status"`
			Priority string `json:"priority"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	entries := make([]map[string]string, 0, len(in.Entries))
	for i, e := range in.Entries {
		switch e.Status {
		case "pending", "in_progress", "completed":
		default:
			return "", fmt.Errorf("entry %d: status must be pending, in_progress or completed", i+1)
		}
		switch e.Priority {
		case "high", "medium", "low":
		case "":
			e.Priority = "medium"
		default:
			return "", fmt.Errorf("entry %d: priority must be high, medium or low", i+1)
		}
		entries = append(entries, map[string]string{"content": e.Content, "status": e.Status, "priority": e.Priority})
	}
	if t.a.session(env.Session) == nil || t.a.conn == nil {
		return "Plan noted (no editor is attached to show it).", nil
	}
	t.a.update(env.Session, map[string]any{"sessionUpdate": "plan", "entries": entries})
	return fmt.Sprintf("Plan shown (%d steps).", len(entries)), nil
}
