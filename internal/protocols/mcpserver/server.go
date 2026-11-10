// Package mcpserver exposes Habakkuk as a Model Context Protocol server, so
// other MCP clients can ask its agents, read sessions, use the board and
// schedules, and (when allowed) send messages to chats. It serves stdio
// (`hk mcp serve`) and Streamable HTTP (/mcp on the HTTP surface).
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/work/board"
	"github.com/habakkuk-ai/habakkuk/internal/work/schedule"
)

// Server holds what the tools reach. Board, Schedules and Gateway may be
// nil; the tools that need them then say so.
type Server struct {
	Runner    *agent.Runner
	Board     *board.Store
	Wake      func() // nudges the board dispatcher; may be nil
	Schedules *schedule.Store
	Gateway   *channel.Gateway
	// AllowSend exposes habakkuk_send. Off by default, since it lets the
	// client message anyone in a configured chat.
	AllowSend bool
	Version   string
	Log       *slog.Logger
}

const maxText = 100_000

// New builds the MCP server with Habakkuk's tools.
func (s *Server) New() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "habakkuk", Title: "Habakkuk", Version: s.Version}, &mcp.ServerOptions{
		Instructions: "Habakkuk is a personal AI agent with its own tools, memory, task board and schedules. " +
			"Use habakkuk_ask to hand an agent a task and get its answer; pass the returned session to continue the conversation.",
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name: "habakkuk_ask",
		Description: "Ask a Habakkuk agent to do something and wait for its reply. The agent works with its own tools " +
			"(files in its workspace, web, memory, delegation) for as long as the task needs. Omit session to start a " +
			"fresh conversation; pass the session from an earlier reply to continue it. Steps that need a human's approval are skipped.",
	}, s.ask)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "habakkuk_sessions_list",
		Description: "List recent Habakkuk sessions (conversations), newest first.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.sessionsList)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "habakkuk_session_read",
		Description: "Read a Habakkuk session's transcript: user and assistant messages, with tool calls and results abbreviated.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.sessionRead)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "habakkuk_board_create",
		Description: "Put a card on Habakkuk's shared task board for an agent. The agent starts once every card in depends_on " +
			"is done, in a fresh conversation that sees only the card, so write the body to stand alone. Check on it with habakkuk_board_list.",
	}, s.boardCreate)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "habakkuk_board_list",
		Description: "List cards on Habakkuk's task board. By default shows open cards (todo, doing, blocked).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.boardList)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "habakkuk_schedules_list",
		Description: "List Habakkuk's scheduled tasks: prompts and scripts that run on a timetable.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.schedulesList)
	if s.AllowSend && s.Gateway != nil {
		mcp.AddTool(srv, &mcp.Tool{
			Name: "habakkuk_send",
			Description: "Send a text message to a chat through one of Habakkuk's channels (account names a configured channel, " +
				"conversation the chat id on that platform). The message is queued and delivered by `hk serve`, even across a restart.",
			Annotations: &mcp.ToolAnnotations{OpenWorldHint: ptr(true)},
		}, s.send)
	}
	return srv
}

// Handler serves the server over Streamable HTTP. The caller guards it with
// Habakkuk's token.
func (s *Server) Handler() http.Handler {
	srv := s.New()
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Logger: s.Log})
}

// ServeStdio serves one client on stdin and stdout until it disconnects.
func (s *Server) ServeStdio(ctx context.Context) error {
	return s.New().Run(ctx, &mcp.StdioTransport{})
}

func ptr[T any](v T) *T { return &v }

// defaultAgent is the agent to use when the client names none.
func (s *Server) defaultAgent(name string) (string, error) {
	if name != "" {
		if _, ok := s.Runner.Agents[name]; !ok {
			return "", fmt.Errorf("unknown agent %q; configured: %s", name, strings.Join(s.agentNames(), ", "))
		}
		return name, nil
	}
	if _, ok := s.Runner.Agents["habakkuk"]; ok {
		return "habakkuk", nil
	}
	if names := s.agentNames(); len(names) == 1 {
		return names[0], nil
	}
	return "", fmt.Errorf("name an agent: %s", strings.Join(s.agentNames(), ", "))
}

func (s *Server) agentNames() []string {
	names := make([]string, 0, len(s.Runner.Agents))
	for n := range s.Runner.Agents {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

type askIn struct {
	Agent   string `json:"agent,omitempty" jsonschema:"agent to ask; default the main agent"`
	Message string `json:"message" jsonschema:"what to ask or ask for"`
	Session string `json:"session,omitempty" jsonschema:"session id from an earlier reply, to continue that conversation"`
}

type askOut struct {
	Reply   string `json:"reply"`
	Session string `json:"session"`
	Agent   string `json:"agent"`
}

func (s *Server) ask(ctx context.Context, _ *mcp.CallToolRequest, in askIn) (*mcp.CallToolResult, askOut, error) {
	text := strings.TrimSpace(in.Message)
	switch {
	case text == "":
		return nil, askOut{}, errors.New("message is empty")
	case utf8.RuneCountInString(text) > maxText:
		return nil, askOut{}, fmt.Errorf("message is longer than %d characters", maxText)
	}
	var sessionID, name string
	if in.Session != "" {
		sess, err := s.Runner.Sessions.Get(ctx, in.Session)
		if err != nil {
			return nil, askOut{}, err
		}
		if in.Agent != "" && in.Agent != sess.Agent {
			return nil, askOut{}, fmt.Errorf("session %s belongs to agent %s, not %s", sess.ID, sess.Agent, in.Agent)
		}
		sessionID, name = sess.ID, sess.Agent
	} else {
		var err error
		if name, err = s.defaultAgent(in.Agent); err != nil {
			return nil, askOut{}, err
		}
		sess, err := s.Runner.NewSession(ctx, name)
		if err != nil {
			return nil, askOut{}, err
		}
		sessionID = sess.ID
	}
	reply, err := s.Runner.Ask(ctx, agent.Turn{Session: sessionID, Text: text}, nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, askOut{}, fmt.Errorf("stopped waiting; the turn keeps running in session %s (read it with habakkuk_session_read)", sessionID)
		}
		return nil, askOut{}, fmt.Errorf("%w (session %s)", err, sessionID)
	}
	out := askOut{Reply: reply, Session: sessionID, Agent: name}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: reply + "\n\n[session " + sessionID + "]"}}}, out, nil
}

type sessionsIn struct {
	Agent string `json:"agent,omitempty" jsonschema:"only this agent's sessions"`
	Limit int    `json:"limit,omitempty" jsonschema:"how many, default 20, at most 200"`
}

type sessionInfo struct {
	ID      string `json:"id"`
	Agent   string `json:"agent"`
	Title   string `json:"title"`
	Updated string `json:"updated"`
}

type sessionsOut struct {
	Sessions []sessionInfo `json:"sessions"`
}

func (s *Server) sessionsList(ctx context.Context, _ *mcp.CallToolRequest, in sessionsIn) (*mcp.CallToolResult, sessionsOut, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	list, err := s.Runner.Sessions.List(ctx, in.Agent, min(limit, 200))
	if err != nil {
		return nil, sessionsOut{}, err
	}
	out := sessionsOut{Sessions: []sessionInfo{}}
	for _, ss := range list {
		out.Sessions = append(out.Sessions, sessionInfo{ID: ss.ID, Agent: ss.Agent, Title: ss.Title, Updated: ss.UpdatedAt.UTC().Format(time.RFC3339)})
	}
	return nil, out, nil
}

type readIn struct {
	Session string `json:"session" jsonschema:"session id"`
	Last    int    `json:"last,omitempty" jsonschema:"only the last N messages; default all, up to 200"`
}

type transcriptLine struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type readOut struct {
	Session  string           `json:"session"`
	Agent    string           `json:"agent"`
	Title    string           `json:"title"`
	Messages []transcriptLine `json:"messages"`
	Omitted  int              `json:"omitted,omitempty"`
}

const maxLine = 4000

func (s *Server) sessionRead(ctx context.Context, _ *mcp.CallToolRequest, in readIn) (*mcp.CallToolResult, readOut, error) {
	sess, err := s.Runner.Sessions.Get(ctx, in.Session)
	if err != nil {
		return nil, readOut{}, err
	}
	recs, err := s.Runner.Sessions.Messages(ctx, sess.ID)
	if err != nil {
		return nil, readOut{}, err
	}
	out := readOut{Session: sess.ID, Agent: sess.Agent, Title: sess.Title, Messages: []transcriptLine{}}
	last := in.Last
	if last <= 0 || last > 200 {
		last = 200
	}
	if len(recs) > last {
		out.Omitted = len(recs) - last
		recs = recs[len(recs)-last:]
	}
	for _, r := range recs {
		for _, b := range r.Message.Blocks {
			switch b.Type {
			case llm.Text:
				out.Messages = append(out.Messages, transcriptLine{Role: string(r.Message.Role), Text: clip(b.Text, maxLine)})
			case llm.ToolUse:
				out.Messages = append(out.Messages, transcriptLine{Role: "tool_call", Text: b.ToolName + " " + clip(string(b.Input), 500)})
			case llm.ToolResult:
				role := "tool_result"
				if b.IsError {
					role = "tool_error"
				}
				out.Messages = append(out.Messages, transcriptLine{Role: role, Text: clip(b.Text, 1000)})
			case llm.Image, llm.Document:
				out.Messages = append(out.Messages, transcriptLine{Role: string(r.Message.Role), Text: fmt.Sprintf("[%s %s]", b.Type, b.Name)})
			}
		}
	}
	return nil, out, nil
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}

type boardCreateIn struct {
	Title     string   `json:"title"`
	Body      string   `json:"body" jsonschema:"the task in full: what to do, what done means, what proof to give"`
	Assignee  string   `json:"assignee" jsonschema:"agent to do it"`
	Priority  int      `json:"priority,omitempty" jsonschema:"higher runs first, default 0"`
	DependsOn []string `json:"depends_on,omitempty" jsonschema:"card ids that must be done first"`
	Parent    string   `json:"parent,omitempty" jsonschema:"card id this one is part of"`
}

type cardInfo struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Assignee  string   `json:"assignee"`
	Status    string   `json:"status"`
	Priority  int      `json:"priority"`
	DependsOn []string `json:"depends_on,omitempty"`
	Result    string   `json:"result,omitempty"`
	Updated   string   `json:"updated"`
}

func card(c board.Card) cardInfo {
	return cardInfo{ID: c.ID, Title: c.Title, Assignee: c.Assignee, Status: string(c.Status), Priority: c.Priority,
		DependsOn: c.DependsOn, Result: clip(c.Result, 2000), Updated: c.UpdatedAt.UTC().Format(time.RFC3339)}
}

var errNoBoard = errors.New("the board is not available in this process")

func (s *Server) boardCreate(ctx context.Context, _ *mcp.CallToolRequest, in boardCreateIn) (*mcp.CallToolResult, cardInfo, error) {
	if s.Board == nil {
		return nil, cardInfo{}, errNoBoard
	}
	if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Body) == "" {
		return nil, cardInfo{}, errors.New("title and body are required")
	}
	if _, ok := s.Runner.Agents[in.Assignee]; !ok {
		return nil, cardInfo{}, fmt.Errorf("unknown agent %q; configured: %s", in.Assignee, strings.Join(s.agentNames(), ", "))
	}
	c, err := s.Board.Create(ctx, board.Card{Title: in.Title, Body: in.Body, Assignee: in.Assignee, Priority: in.Priority,
		DependsOn: in.DependsOn, Parent: in.Parent, CreatedBy: board.User})
	if err != nil {
		return nil, cardInfo{}, err
	}
	if s.Wake != nil {
		s.Wake()
	}
	return nil, card(*c), nil
}

type boardListIn struct {
	Status   string `json:"status,omitempty" jsonschema:"open (default), todo, doing, blocked, done, cancelled or all"`
	Assignee string `json:"assignee,omitempty" jsonschema:"only cards assigned to this agent"`
}

type boardListOut struct {
	Cards []cardInfo `json:"cards"`
}

func (s *Server) boardList(ctx context.Context, _ *mcp.CallToolRequest, in boardListIn) (*mcp.CallToolResult, boardListOut, error) {
	if s.Board == nil {
		return nil, boardListOut{}, errNoBoard
	}
	f := board.Filter{Status: in.Status, Assignee: in.Assignee}
	if f.Status == "open" {
		f.Status = ""
	}
	list, err := s.Board.List(ctx, f)
	if err != nil {
		return nil, boardListOut{}, err
	}
	out := boardListOut{Cards: []cardInfo{}}
	for _, c := range list {
		out.Cards = append(out.Cards, card(c))
	}
	return nil, out, nil
}

type schedulesIn struct {
	Agent string `json:"agent,omitempty" jsonschema:"only this agent's schedules"`
}

type scheduleInfo struct {
	ID         string `json:"id"`
	Agent      string `json:"agent"`
	Name       string `json:"name"`
	When       string `json:"when"`
	Enabled    bool   `json:"enabled"`
	NextRun    string `json:"next_run,omitempty"`
	Prompt     string `json:"prompt,omitempty"`
	Script     bool   `json:"script,omitempty"`
	LastStatus string `json:"last_status,omitempty"`
}

type schedulesOut struct {
	Schedules []scheduleInfo `json:"schedules"`
}

func (s *Server) schedulesList(ctx context.Context, _ *mcp.CallToolRequest, in schedulesIn) (*mcp.CallToolResult, schedulesOut, error) {
	if s.Schedules == nil {
		return nil, schedulesOut{}, errors.New("schedules are not available in this process")
	}
	list, err := s.Schedules.List(ctx, in.Agent)
	if err != nil {
		return nil, schedulesOut{}, err
	}
	out := schedulesOut{Schedules: []scheduleInfo{}}
	for _, sc := range list {
		info := scheduleInfo{ID: sc.ID, Agent: sc.Agent, Name: sc.Name, When: sc.Spec, Enabled: sc.Enabled,
			Prompt: clip(sc.Prompt, 500), Script: sc.Command != "", LastStatus: clip(sc.LastStatus, 300)}
		if !sc.NextRun.IsZero() {
			info.NextRun = sc.NextRun.UTC().Format(time.RFC3339)
		}
		out.Schedules = append(out.Schedules, info)
	}
	return nil, out, nil
}

type sendIn struct {
	Account      string `json:"account" jsonschema:"channel account from Habakkuk's config, e.g. telegram"`
	Conversation string `json:"conversation" jsonschema:"chat id on that platform"`
	Thread       string `json:"thread,omitempty" jsonschema:"thread or topic, where the platform has them"`
	Text         string `json:"text" jsonschema:"the message, in Markdown"`
}

type sendOut struct {
	Queued string `json:"queued"`
}

func (s *Server) send(ctx context.Context, _ *mcp.CallToolRequest, in sendIn) (*mcp.CallToolResult, sendOut, error) {
	if in.Account == "" || in.Conversation == "" {
		return nil, sendOut{}, errors.New("account and conversation are required")
	}
	if utf8.RuneCountInString(in.Text) > maxText {
		return nil, sendOut{}, fmt.Errorf("text is longer than %d characters", maxText)
	}
	to := channel.Address{Account: in.Account, Conversation: in.Conversation, Thread: in.Thread}
	id := jobs.NewID("send")
	if err := s.Gateway.SendText(agent.WithPoster(ctx, agent.Poster{What: "an MCP client (habakkuk_send)"}), id, to, in.Text); err != nil {
		return nil, sendOut{}, err
	}
	if s.Log != nil {
		s.Log.Info("mcp habakkuk_send queued", "account", in.Account, "conversation", in.Conversation, "job", id)
	}
	return nil, sendOut{Queued: id}, nil
}
