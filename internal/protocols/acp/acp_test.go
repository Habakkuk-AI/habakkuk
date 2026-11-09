package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	habakkuk "github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// script is a model that plays a fixed coding session:
//   - "edit": plan, read notes.txt and run a risky tool, then write notes.txt, then report
//   - "slow": stream a little, then wait until the turn is cancelled
//   - anything else: echo it
type script struct{}

func use(id, name, input string) llm.Block {
	return llm.Block{Type: llm.ToolUse, ToolUseID: id, ToolName: name, Input: json.RawMessage(input)}
}

func (script) Send(ctx context.Context, _ []llm.Target, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error) {
	say := func(text string) (*llm.Response, error) {
		onEvent(llm.Event{Type: llm.EventText, Text: text})
		return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: text}}}, StopReason: llm.StopEnd}, nil
	}
	calls := func(blocks ...llm.Block) (*llm.Response, error) {
		return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: blocks}, StopReason: llm.StopToolUse}, nil
	}
	msgs := req.Messages
	last := msgs[len(msgs)-1]
	if len(last.Blocks) > 0 && last.Blocks[0].Type == llm.ToolResult {
		prev := msgs[len(msgs)-2]
		if prev.ToolUses()[0].ToolName == "update_plan" {
			return calls(use("w1", "write_file", `{"path":"notes.txt","content":"rewritten"}`))
		}
		var parts []string
		for _, b := range last.Blocks {
			parts = append(parts, b.Text)
		}
		return say("done: " + strings.Join(parts, " | "))
	}
	_, msg, _ := strings.Cut(last.Text(), "\n") // drop the time stamp line
	switch msg {
	case "edit":
		onEvent(llm.Event{Type: llm.EventText, Text: "Let me look."})
		return calls(
			use("p1", "update_plan", `{"entries":[{"content":"Read the notes","status":"in_progress","priority":"high"},{"content":"Rewrite them","status":"pending"}]}`),
			use("r1", "read_file", `{"path":"notes.txt"}`),
			use("d1", "danger", `{"what":"launch"}`),
		)
	case "slow":
		onEvent(llm.Event{Type: llm.EventText, Text: "working"})
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return say("echo: " + msg)
}

// danger needs approval below full mode.
type danger struct{}

func (danger) Risk() tools.Risk { return tools.RiskExec }
func (danger) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "danger", Schema: json.RawMessage(`{"type":"object"}`)}
}
func (danger) Run(context.Context, *tools.Env, json.RawMessage) (string, error) {
	return "launched", nil
}

// client is an editor: it answers file and permission requests and keeps
// every session/update.
type client struct {
	c *conn

	mu      sync.Mutex
	updates []map[string]any
	reads   []string
	writes  map[string]string
	asked   []map[string]any
	allow   string // optionId to pick
	buffer  string // what fs/read_text_file returns
	chunked chan struct{}
}

func (cl *client) handle(_ context.Context, method string, params json.RawMessage) (any, error) {
	var p map[string]any
	_ = json.Unmarshal(params, &p)
	cl.mu.Lock()
	defer cl.mu.Unlock()
	switch method {
	case "session/update":
		u := p["update"].(map[string]any)
		cl.updates = append(cl.updates, u)
		if u["sessionUpdate"] == "agent_message_chunk" {
			select {
			case cl.chunked <- struct{}{}:
			default:
			}
		}
		return nil, nil
	case "fs/read_text_file":
		cl.reads = append(cl.reads, p["path"].(string))
		return map[string]string{"content": cl.buffer}, nil
	case "fs/write_text_file":
		cl.writes[p["path"].(string)] = p["content"].(string)
		return nil, nil
	case "session/request_permission":
		cl.asked = append(cl.asked, p)
		return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": cl.allow}}, nil
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: method}
}

func (cl *client) take() []map[string]any {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	u := cl.updates
	cl.updates = nil
	return u
}

func (cl *client) call(t *testing.T, method string, params, out any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return cl.c.call(ctx, method, params, out)
}

type rig struct {
	cl  *client
	cwd string
	q   *jobs.Queue
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	control, err := store.Open(ctx, filepath.Join(dir, "control.db"), jobs.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	transcripts, err := store.Open(ctx, filepath.Join(dir, "transcripts.db"), habakkuk.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close(); transcripts.Close() })
	q := jobs.NewQueue(control)
	reg := tools.NewRegistry()
	tools.RegisterBuiltins(reg)
	reg.Register("core", danger{})
	home := filepath.Join(dir, "home")
	base := &agent.Runner{
		Agents: map[string]*agent.Spec{"habakkuk": {Name: "habakkuk", Chain: []llm.Target{{Endpoint: "fake", Model: "m"}},
			MaxTokens: 1000, MaxIterations: 10, Toolsets: []string{"core"}, Mode: tools.ModeWorkspace, Workspace: home, Dir: home}},
		Sessions: habakkuk.NewStore(transcripts), Queue: q, Models: script{}, Tools: reg, Bus: events.NewBus(), Log: quiet,
		Env: func(s *agent.Spec, _ string) *tools.Env { return &tools.Env{Workspace: s.Workspace, Mode: s.Mode} },
	}
	a, err := New(base, reg, "habakkuk", "test", quiet)
	if err != nil {
		t.Fatal(err)
	}
	pool := &jobs.Pool{Queue: q, Handlers: map[string]jobs.Handler{KindTurn: a.Handle},
		Concurrency: 2, Lease: 5 * time.Second, Poll: 10 * time.Millisecond, Log: quiet}
	a.SetWake(pool.Wake)

	toAgent, fromClient := io.Pipe()
	toClient, fromAgent := io.Pipe()
	cl := &client{c: newConn(toClient, fromClient), writes: map[string]string{}, allow: "allow_once",
		buffer: "unsaved buffer text", chunked: make(chan struct{}, 16)}
	sctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); pool.Run(sctx) }()
	go func() { defer wg.Done(); _ = a.Serve(sctx, toAgent, fromAgent); fromAgent.Close() }()
	go func() { defer wg.Done(); _ = cl.c.serve(sctx, cl.handle); fromClient.Close() }()
	t.Cleanup(func() { fromClient.Close(); cancel(); wg.Wait() })

	cwd := filepath.Join(dir, "project")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	return &rig{cl: cl, cwd: cwd, q: q}
}

func (r *rig) init(t *testing.T) {
	t.Helper()
	var res struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			LoadSession bool `json:"loadSession"`
		} `json:"agentCapabilities"`
	}
	err := r.cl.call(t, "initialize", map[string]any{"protocolVersion": 1,
		"clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": true, "writeTextFile": true}}}, &res)
	if err != nil || res.ProtocolVersion != 1 || !res.AgentCapabilities.LoadSession {
		t.Fatalf("initialize: %v %+v", err, res)
	}
}

func (r *rig) newSession(t *testing.T) string {
	t.Helper()
	var res struct {
		SessionID string `json:"sessionId"`
	}
	if err := r.cl.call(t, "session/new", map[string]any{"cwd": r.cwd, "mcpServers": []any{}}, &res); err != nil {
		t.Fatal(err)
	}
	return res.SessionID
}

func (r *rig) prompt(t *testing.T, sid, text string) (string, error) {
	t.Helper()
	var res struct {
		StopReason string `json:"stopReason"`
	}
	err := r.cl.call(t, "session/prompt", map[string]any{"sessionId": sid,
		"prompt": []any{map[string]string{"type": "text", "text": text}}}, &res)
	return res.StopReason, err
}

func find(updates []map[string]any, kind string, match func(map[string]any) bool) map[string]any {
	for _, u := range updates {
		if u["sessionUpdate"] == kind && (match == nil || match(u)) {
			return u
		}
	}
	return nil
}

func TestPromptStreamsToolsPlansAndUsesEditorFiles(t *testing.T) {
	r := newRig(t)
	r.init(t)
	var rerr *rpcError
	if err := r.cl.call(t, "session/new", map[string]any{"cwd": "relative/dir", "mcpServers": []any{}}, nil); !errors.As(err, &rerr) || rerr.Code != codeInvalidParams {
		t.Fatalf("relative cwd: %v", err)
	}
	sid := r.newSession(t)
	stop, err := r.prompt(t, sid, "edit")
	if err != nil || stop != "end_turn" {
		t.Fatalf("prompt: %q %v", stop, err)
	}
	ups := r.cl.take()
	notes := filepath.Join(r.cwd, "notes.txt")

	plan := find(ups, "plan", nil)
	if plan == nil || len(plan["entries"].([]any)) != 2 {
		t.Fatalf("no plan in %v", ups)
	}
	if e := plan["entries"].([]any)[1].(map[string]any); e["priority"] != "medium" || e["status"] != "pending" {
		t.Fatalf("plan entry %v", e)
	}
	read := find(ups, "tool_call", func(u map[string]any) bool { return u["toolCallId"] == "r1" })
	if read == nil || read["title"] != "Read notes.txt" || read["kind"] != "read" ||
		read["locations"].([]any)[0].(map[string]any)["path"] != notes {
		t.Fatalf("read tool call %v", read)
	}
	if find(ups, "tool_call", func(u map[string]any) bool { return u["toolCallId"] == "p1" }) != nil {
		t.Fatal("update_plan showed as a tool call")
	}
	if find(ups, "tool_call_update", func(u map[string]any) bool { return u["toolCallId"] == "r1" && u["status"] == "completed" }) == nil {
		t.Fatalf("read never completed: %v", ups)
	}
	if find(ups, "tool_call", func(u map[string]any) bool { return u["toolCallId"] == "w1" && u["kind"] == "edit" }) == nil {
		t.Fatalf("no write tool call: %v", ups)
	}
	if find(ups, "agent_message_chunk", func(u map[string]any) bool {
		return strings.HasPrefix(u["content"].(map[string]any)["text"].(string), "done: ")
	}) == nil {
		t.Fatalf("no final message chunk: %v", ups)
	}

	// The file tools went through the editor: the read saw its buffer, the
	// write landed in it, and the disk was never touched.
	r.cl.mu.Lock()
	reads, wrote, asked := r.cl.reads, r.cl.writes[notes], r.cl.asked
	r.cl.mu.Unlock()
	if len(reads) != 1 || reads[0] != notes || wrote != "rewritten" {
		t.Fatalf("reads %v, write %q", reads, wrote)
	}
	if _, err := os.Stat(notes); !os.IsNotExist(err) {
		t.Fatalf("notes.txt is on disk: %v", err)
	}
	// The risky tool asked the editor first, naming the same tool call.
	if len(asked) != 1 || asked[0]["toolCall"].(map[string]any)["toolCallId"] != "d1" || len(asked[0]["options"].([]any)) != 3 {
		t.Fatalf("permission requests %v", asked)
	}
	if find(ups, "tool_call_update", func(u map[string]any) bool { return u["toolCallId"] == "d1" && u["status"] == "in_progress" }) == nil {
		t.Fatalf("approved call never went in progress: %v", ups)
	}
	final := find(ups, "agent_message_chunk", func(u map[string]any) bool {
		return strings.HasPrefix(u["content"].(map[string]any)["text"].(string), "done: ")
	})["content"].(map[string]any)["text"].(string)
	if !strings.Contains(final, "wrote") {
		t.Fatalf("final %q", final)
	}
}

func TestPermissionDeniedAndAlways(t *testing.T) {
	r := newRig(t)
	r.init(t)
	sid := r.newSession(t)
	r.cl.allow = "reject_once"
	if stop, err := r.prompt(t, sid, "edit"); err != nil || stop != "end_turn" {
		t.Fatalf("prompt: %q %v", stop, err)
	}
	ups := r.cl.take()
	if find(ups, "tool_call_update", func(u map[string]any) bool { return u["toolCallId"] == "d1" && u["status"] == "failed" }) == nil {
		t.Fatalf("denied call did not fail: %v", ups)
	}
	r.cl.mu.Lock()
	r.cl.allow, r.cl.asked = "allow_always", nil
	r.cl.mu.Unlock()
	for range 2 {
		if _, err := r.prompt(t, sid, "edit"); err != nil {
			t.Fatal(err)
		}
	}
	r.cl.mu.Lock()
	defer r.cl.mu.Unlock()
	if len(r.cl.asked) != 1 {
		t.Fatalf("asked %d times after allow_always, want 1", len(r.cl.asked))
	}
}

func TestCancel(t *testing.T) {
	r := newRig(t)
	r.init(t)
	sid := r.newSession(t)
	done := make(chan string, 1)
	go func() {
		stop, err := r.prompt(t, sid, "slow")
		if err != nil {
			stop = "error: " + err.Error()
		}
		done <- stop
	}()
	select {
	case <-r.cl.chunked:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never started")
	}
	if err := r.cl.c.notify("session/cancel", map[string]string{"sessionId": sid}); err != nil {
		t.Fatal(err)
	}
	select {
	case stop := <-done:
		if stop != "cancelled" {
			t.Fatalf("stop reason %q", stop)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not end the prompt")
	}
	// The session takes the next prompt.
	if stop, err := r.prompt(t, sid, "hi"); err != nil || stop != "end_turn" {
		t.Fatalf("after cancel: %q %v", stop, err)
	}
}

func TestLoadSessionReplays(t *testing.T) {
	r := newRig(t)
	r.init(t)
	sid := r.newSession(t)
	if _, err := r.prompt(t, sid, "remember me"); err != nil {
		t.Fatal(err)
	}
	r.cl.take()
	if err := r.cl.call(t, "session/load", map[string]any{"sessionId": sid, "cwd": r.cwd, "mcpServers": []any{}}, nil); err != nil {
		t.Fatal(err)
	}
	ups := r.cl.take()
	user := find(ups, "user_message_chunk", nil)
	agentMsg := find(ups, "agent_message_chunk", nil)
	if user == nil || user["content"].(map[string]any)["text"] != "remember me" ||
		agentMsg == nil || agentMsg["content"].(map[string]any)["text"] != "echo: remember me" {
		t.Fatalf("replay %v", ups)
	}
	if err := r.cl.call(t, "session/load", map[string]any{"sessionId": "ses_nope", "cwd": r.cwd, "mcpServers": []any{}}, nil); err == nil {
		t.Fatal("loaded a missing session")
	}
	if err := r.cl.call(t, "session/set_mode", map[string]any{}, nil); err == nil {
		t.Fatal("unknown method accepted")
	}
}

func TestPromptContent(t *testing.T) {
	text, err := promptText([]contentBlock{
		{Type: "text", Text: "Fix this"},
		{Type: "resource_link", URI: "file:///src/a.go", Name: "a.go"},
		{Type: "resource", Resource: &struct {
			URI      string `json:"uri"`
			Text     string `json:"text"`
			Blob     string `json:"blob"`
			MimeType string `json:"mimeType"`
		}{URI: "file:///src/b.go", Text: "package b"}},
	})
	if err != nil || !strings.Contains(text, "[Referenced: a.go (/src/a.go)]") || !strings.Contains(text, "package b") {
		t.Fatalf("%q %v", text, err)
	}
	if _, err := promptText([]contentBlock{{Type: "image"}}); err == nil {
		t.Fatal("image accepted")
	}
}
