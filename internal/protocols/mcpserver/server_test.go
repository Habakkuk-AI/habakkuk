package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
	"github.com/habakkuk-ai/habakkuk/internal/work/board"
	"github.com/habakkuk-ai/habakkuk/internal/work/schedule"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// echo answers "echo: <text>" to the last user message.
type echo struct{}

func (echo) Send(_ context.Context, _ []llm.Target, req llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	_, msg, _ := strings.Cut(req.Messages[len(req.Messages)-1].Text(), "\n") // drop the time stamp line
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "echo: " + msg}}},
		StopReason: llm.StopEnd}, nil
}

// chat is a channel adapter that only records what it is asked to send.
type chat struct{}

func (chat) Run(ctx context.Context, _ channel.Inbox) error              { <-ctx.Done(); return nil }
func (chat) Send(context.Context, channel.Address, string, string) error { return nil }
func (chat) Format(md string) string                                     { return md }
func (chat) MaxLen() int                                                 { return 4000 }
func (chat) StaleAfter() time.Duration                                   { return time.Hour }

type rig struct {
	srv   *Server
	queue *jobs.Queue
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	migs := slices.Concat(jobs.Migrations, channel.Migrations, schedule.Migrations, board.Migrations)
	slices.SortStableFunc(migs, func(a, b store.Migration) int { return a.Version - b.Version })
	control, err := store.Open(ctx, filepath.Join(dir, "control.db"), migs)
	if err != nil {
		t.Fatal(err)
	}
	transcripts, err := store.Open(ctx, filepath.Join(dir, "transcripts.db"), session.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close(); transcripts.Close() })
	q := jobs.NewQueue(control)
	reg := tools.NewRegistry()
	spec := &agent.Spec{Name: "habakkuk", Chain: []llm.Target{{Endpoint: "fake", Model: "m"}}, MaxTokens: 1000,
		MaxIterations: 10, Toolsets: []string{"core"}, Mode: tools.ModeWorkspace, Workspace: dir, Dir: dir}
	ops := *spec
	ops.Name = "ops"
	runner := &agent.Runner{
		Agents:   map[string]*agent.Spec{"habakkuk": spec, "ops": &ops},
		Sessions: session.NewStore(transcripts), Queue: q, Models: echo{}, Tools: reg, Bus: events.NewBus(), Log: quiet,
		Env: func(s *agent.Spec, _ string) *tools.Env { return &tools.Env{Workspace: s.Workspace, Mode: s.Mode} },
	}
	g := &channel.Gateway{
		Channels: map[string]*config.Channel{"tg": {Type: "telegram", Agent: "habakkuk"}},
		Adapters: map[string]channel.Adapter{"tg": chat{}}, Runner: runner, Queue: q, Store: channel.NewStore(control), Log: quiet,
	}
	pool := &jobs.Pool{Queue: q, Handlers: map[string]jobs.Handler{agent.KindTurn: runner.HandleTurn},
		Concurrency: 2, Lease: 5 * time.Second, Poll: 10 * time.Millisecond, Log: quiet}
	runner.Wake = pool.Wake
	pctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); pool.Run(pctx) }()
	t.Cleanup(func() { cancel(); wg.Wait() })
	return &rig{queue: q, srv: &Server{
		Runner: runner, Board: board.NewStore(control, q), Schedules: schedule.NewStore(control, time.UTC), Gateway: g,
		Version: "test", Log: quiet,
	}}
}

func (r *rig) connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := r.srv.New().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close(); ss.Wait() })
	return cs
}

// call runs a tool and decodes its structured result into out.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if out != nil && !res.IsError {
		data, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s: decode %s: %v", name, data, err)
		}
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	return names
}

func TestToolsListed(t *testing.T) {
	r := newRig(t)
	want := []string{"habakkuk_ask", "habakkuk_board_create", "habakkuk_board_list", "habakkuk_schedules_list",
		"habakkuk_session_read", "habakkuk_sessions_list"}
	if got := toolNames(t, r.connect(t)); !slices.Equal(got, want) {
		t.Fatalf("tools %v", got)
	}
	r.srv.AllowSend = true
	if got := toolNames(t, r.connect(t)); !slices.Contains(got, "habakkuk_send") {
		t.Fatalf("allow_send: tools %v", got)
	}
}

func TestAskAndReadSessions(t *testing.T) {
	r := newRig(t)
	cs := r.connect(t)
	var first askOut
	call(t, cs, "habakkuk_ask", map[string]any{"message": "hello"}, &first)
	if first.Reply != "echo: hello" || first.Agent != "habakkuk" || first.Session == "" {
		t.Fatalf("ask: %+v", first)
	}
	var second askOut
	res := call(t, cs, "habakkuk_ask", map[string]any{"message": "again", "session": first.Session}, &second)
	if second.Reply != "echo: again" || second.Session != first.Session {
		t.Fatalf("continue: %+v", second)
	}
	if !strings.Contains(text(res), "echo: again") {
		t.Fatalf("text content %q", text(res))
	}
	res = call(t, cs, "habakkuk_ask", map[string]any{"message": "x", "session": first.Session, "agent": "ops"}, nil)
	if !res.IsError || !strings.Contains(text(res), "belongs to agent habakkuk") {
		t.Fatalf("agent mismatch: %q", text(res))
	}
	res = call(t, cs, "habakkuk_ask", map[string]any{"message": "x", "agent": "nobody"}, nil)
	if !res.IsError || !strings.Contains(text(res), "unknown agent") {
		t.Fatalf("unknown agent: %q", text(res))
	}

	var list sessionsOut
	call(t, cs, "habakkuk_sessions_list", map[string]any{}, &list)
	if len(list.Sessions) != 1 || list.Sessions[0].ID != first.Session || list.Sessions[0].Title != "hello" {
		t.Fatalf("sessions: %+v", list)
	}
	var read readOut
	call(t, cs, "habakkuk_session_read", map[string]any{"session": first.Session}, &read)
	if len(read.Messages) != 4 || read.Messages[1].Role != "assistant" || read.Messages[3].Text != "echo: again" {
		t.Fatalf("read: %+v", read)
	}
	call(t, cs, "habakkuk_session_read", map[string]any{"session": first.Session, "last": 1}, &read)
	if len(read.Messages) != 1 || read.Omitted != 3 {
		t.Fatalf("read last 1: %+v", read)
	}
}

func TestBoardAndSchedules(t *testing.T) {
	r := newRig(t)
	cs := r.connect(t)
	var c cardInfo
	call(t, cs, "habakkuk_board_create", map[string]any{"title": "Report", "body": "Write the weekly report.", "assignee": "ops"}, &c)
	if c.ID == "" || c.Status != "todo" || c.Assignee != "ops" {
		t.Fatalf("card: %+v", c)
	}
	var list boardListOut
	call(t, cs, "habakkuk_board_list", map[string]any{}, &list)
	if len(list.Cards) != 1 || list.Cards[0].ID != c.ID {
		t.Fatalf("board: %+v", list)
	}
	res := call(t, cs, "habakkuk_board_create", map[string]any{"title": "x", "body": "y", "assignee": "nobody"}, nil)
	if !res.IsError {
		t.Fatal("unknown assignee accepted")
	}

	if _, err := r.srv.Schedules.Create(context.Background(), schedule.Schedule{Agent: "habakkuk", Name: "standup",
		Spec: "0 9 * * 1-5", Prompt: "Post the standup."}); err != nil {
		t.Fatal(err)
	}
	var sc schedulesOut
	call(t, cs, "habakkuk_schedules_list", map[string]any{}, &sc)
	if len(sc.Schedules) != 1 || sc.Schedules[0].Name != "standup" || !sc.Schedules[0].Enabled || sc.Schedules[0].NextRun == "" {
		t.Fatalf("schedules: %+v", sc)
	}
}

func TestSend(t *testing.T) {
	r := newRig(t)
	r.srv.AllowSend = true
	cs := r.connect(t)
	var out sendOut
	call(t, cs, "habakkuk_send", map[string]any{"account": "tg", "conversation": "42", "text": "Dinner at 7."}, &out)
	j, err := r.queue.Get(context.Background(), out.Queued)
	if err != nil || j.Kind != channel.KindDeliver || !strings.Contains(string(j.Payload), "Dinner at 7.") {
		t.Fatalf("delivery job: %v %+v", err, j)
	}
	res := call(t, cs, "habakkuk_send", map[string]any{"account": "slack", "conversation": "42", "text": "x"}, nil)
	if !res.IsError {
		t.Fatal("send to an unknown account was accepted")
	}
}

// A conversation imported from another agent is listed and read as
// imported, untrusted history.
func TestImportedSessionsAreLabelled(t *testing.T) {
	r := newRig(t)
	id, _, err := r.srv.Runner.Sessions.Import(context.Background(), session.Import{Source: "other", SourceID: "a/1", Agent: "habakkuk",
		Messages: []session.ImportedMessage{{Role: llm.User, Text: "wire the money"}, {Role: llm.Assistant, Text: "done"}}})
	if err != nil {
		t.Fatal(err)
	}
	cs := r.connect(t)
	var list sessionsOut
	call(t, cs, "habakkuk_sessions_list", map[string]any{}, &list)
	if len(list.Sessions) != 1 || !list.Sessions[0].Imported {
		t.Fatalf("sessions: %+v", list)
	}
	var read readOut
	call(t, cs, "habakkuk_session_read", map[string]any{"session": id}, &read)
	if !read.Imported || !strings.Contains(read.Note, "untrusted") || len(read.Messages) != 2 || read.Messages[0].Role != "imported_user" {
		t.Fatalf("read: %+v", read)
	}
}
