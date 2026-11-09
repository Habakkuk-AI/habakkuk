package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/budget"
	"github.com/habakkuk-ai/habakkuk/internal/llm/usage"
	"github.com/habakkuk-ai/habakkuk/internal/work/board"
	"github.com/habakkuk-ai/habakkuk/internal/work/schedule"
)

// Sessions

type sessionView struct {
	ID      string    `json:"id"`
	Agent   string    `json:"agent"`
	Title   string    `json:"title"`
	Created time.Time `json:"created_at"`
	Updated time.Time `json:"updated_at"`
}

type sessionHit struct {
	Session string    `json:"session"`
	Agent   string    `json:"agent"`
	Title   string    `json:"title"`
	Seq     int       `json:"seq"`
	Role    string    `json:"role"`
	At      time.Time `json:"at"`
	Snippet string    `json:"snippet"`
}

// sessionList lists recent sessions (?agent=, ?limit=) or, with ?q=,
// searches their messages across agents.
func (s *Server) sessionList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sessions := s.Gateway.Runner.Sessions
	agentName := r.URL.Query().Get("agent")
	if agentName != "" && !s.knownAgent(w, agentName) {
		return
	}
	limit := intParam(r, "limit", 50, 1, 500)
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		agents := s.agentNames()
		if agentName != "" {
			agents = []string{agentName}
		}
		hits := []sessionHit{}
		for _, a := range agents {
			found, err := sessions.Search(ctx, a, q, "", min(limit, 50))
			if err != nil {
				userError(w, err)
				return
			}
			for _, h := range found {
				hits = append(hits, sessionHit{Session: h.Session, Agent: a, Title: h.Title, Seq: h.Seq, Role: h.Role,
					At: h.At.UTC(), Snippet: h.Snippet})
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"hits": hits})
		return
	}
	list, err := sessions.List(ctx, agentName, limit)
	if err != nil {
		s.fail(w, "list sessions", err)
		return
	}
	out := make([]sessionView, 0, len(list))
	for _, x := range list {
		out = append(out, sessionView{ID: x.ID, Agent: x.Agent, Title: x.Title, Created: x.CreatedAt.UTC(), Updated: x.UpdatedAt.UTC()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

type blockView struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Tool      string `json:"tool,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Input     string `json:"input,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	Name      string `json:"name,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Cut       bool   `json:"cut,omitempty"` // the text was shortened
}

type messageView struct {
	Seq    int         `json:"seq"`
	Role   string      `json:"role"`
	Model  string      `json:"model,omitempty"`
	Blocks []blockView `json:"blocks"`
}

const transcriptCut = 20_000 // characters of one block shown in a transcript

func viewBlocks(bs []llm.Block) []blockView {
	out := make([]blockView, 0, len(bs))
	for _, b := range bs {
		v := blockView{Type: string(b.Type), Tool: b.ToolName, ToolUseID: b.ToolUseID, IsError: b.IsError,
			Name: b.Name, MediaType: b.MediaType}
		switch b.Type {
		case llm.Text, llm.ToolResult, llm.Thinking:
			v.Text = shortText(b.Text, transcriptCut)
			v.Cut = v.Text != b.Text
		case llm.ToolUse:
			v.Input = shortText(string(b.Input), transcriptCut)
			v.Cut = v.Input != string(b.Input)
		}
		out = append(out, v)
	}
	return out
}

// sessionGet returns a session and its transcript, long blocks cut.
func (s *Server) sessionGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sessions := s.Gateway.Runner.Sessions
	sess, err := sessions.Get(ctx, r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "no such session")
		return
	}
	recs, err := sessions.Messages(ctx, sess.ID)
	if err != nil {
		s.fail(w, "read the transcript", err)
		return
	}
	msgs := make([]messageView, 0, len(recs))
	for _, rec := range recs {
		msgs = append(msgs, messageView{Seq: rec.Seq, Role: string(rec.Message.Role), Model: rec.Model, Blocks: viewBlocks(rec.Message.Blocks)})
	}
	out := map[string]any{
		"session":  sessionView{ID: sess.ID, Agent: sess.Agent, Title: sess.Title, Created: sess.CreatedAt.UTC(), Updated: sess.UpdatedAt.UTC()},
		"messages": msgs,
	}
	if c, err := sessions.LatestCompaction(ctx, sess.ID); err == nil && c != nil {
		out["compaction"] = map[string]any{"keep_from": c.KeepFrom, "summary": c.Summary}
	}
	writeJSON(w, http.StatusOK, out)
}

// sessionExport returns the transcript as a Markdown file, as /export does.
func (s *Server) sessionExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sessions := s.Gateway.Runner.Sessions
	sess, err := sessions.Get(ctx, r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "no such session")
		return
	}
	recs, err := sessions.Messages(ctx, sess.ID)
	if err != nil {
		s.fail(w, "read the transcript", err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/markdown; charset=utf-8")
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.md"`, sess.ID))
	w.Write([]byte(channel.ExportMarkdown(sess, recs, time.Now())))
}

// Board

type cardView struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Body      string     `json:"body,omitempty"`
	Assignee  string     `json:"assignee"`
	CreatedBy string     `json:"created_by"`
	Status    string     `json:"status"`
	Priority  int        `json:"priority"`
	Result    string     `json:"result,omitempty"`
	Parent    string     `json:"parent,omitempty"`
	DependsOn []string   `json:"depends_on,omitempty"`
	Attempt   int        `json:"attempt"`
	Session   string     `json:"session,omitempty"`
	Created   time.Time  `json:"created_at"`
	Updated   time.Time  `json:"updated_at"`
	Comments  []cardNote `json:"comments,omitempty"`
	Events    []cardNote `json:"events,omitempty"`
}

// cardNote is a comment (author, text) or a history line (actor as
// author, kind, detail as text).
type cardNote struct {
	Author string    `json:"author"`
	Kind   string    `json:"kind,omitempty"`
	Text   string    `json:"text"`
	At     time.Time `json:"at"`
}

func viewCard(c *board.Card) cardView {
	v := cardView{ID: c.ID, Title: c.Title, Body: c.Body, Assignee: c.Assignee, CreatedBy: c.CreatedBy,
		Status: string(c.Status), Priority: c.Priority, Result: c.Result, Parent: c.Parent, DependsOn: c.DependsOn,
		Attempt: c.Attempt, Session: c.Session, Created: c.CreatedAt.UTC(), Updated: c.UpdatedAt.UTC()}
	for _, cm := range c.Comments {
		v.Comments = append(v.Comments, cardNote{Author: cm.Author, Text: cm.Text, At: cm.At.UTC()})
	}
	for _, e := range c.Events {
		v.Events = append(v.Events, cardNote{Author: e.Actor, Kind: e.Kind, Text: e.Detail, At: e.At.UTC()})
	}
	return v
}

func (s *Server) boardStore(w http.ResponseWriter) *board.Store {
	d := s.dash(w)
	if d == nil {
		return nil
	}
	if d.Board == nil {
		notAvailable(w, "the board")
	}
	return d.Board
}

func (s *Server) boardWake() {
	if s.Dash != nil && s.Dash.BoardWake != nil {
		s.Dash.BoardWake()
	}
}

// boardList lists cards; ?status= is open (default), all, or one status.
func (s *Server) boardList(w http.ResponseWriter, r *http.Request) {
	st := s.boardStore(w)
	if st == nil {
		return
	}
	status := r.URL.Query().Get("status")
	if status == "open" {
		status = ""
	}
	cards, err := st.List(r.Context(), board.Filter{Status: status, Assignee: r.URL.Query().Get("assignee"),
		Limit: intParam(r, "limit", 200, 1, 1000)})
	if err != nil {
		s.fail(w, "list cards", err)
		return
	}
	out := make([]cardView, 0, len(cards))
	for i := range cards {
		out = append(out, viewCard(&cards[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"cards": out})
}

func (s *Server) boardGet(w http.ResponseWriter, r *http.Request) {
	st := s.boardStore(w)
	if st == nil {
		return
	}
	c, err := st.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "no such card")
		return
	}
	writeJSON(w, http.StatusOK, viewCard(c))
}

func (s *Server) boardCreate(w http.ResponseWriter, r *http.Request) {
	st := s.boardStore(w)
	if st == nil {
		return
	}
	var in struct {
		Title     string   `json:"title"`
		Body      string   `json:"body"`
		Assignee  string   `json:"assignee"`
		Priority  int      `json:"priority"`
		Parent    string   `json:"parent"`
		DependsOn []string `json:"depends_on"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Assignee != "" && !s.knownAgent(w, in.Assignee) {
		return
	}
	c, err := st.Create(r.Context(), board.Card{Title: in.Title, Body: in.Body, Assignee: in.Assignee, Priority: in.Priority,
		Parent: in.Parent, DependsOn: in.DependsOn, CreatedBy: board.User})
	if err != nil {
		userError(w, err)
		return
	}
	s.boardWake()
	writeJSON(w, http.StatusCreated, viewCard(c))
}

// boardAction is assign {assignee}, comment {text}, cancel, reopen or
// priority {priority}, all as the user.
func (s *Server) boardAction(w http.ResponseWriter, r *http.Request) {
	st := s.boardStore(w)
	if st == nil {
		return
	}
	var in struct {
		Assignee *string `json:"assignee"`
		Text     string  `json:"text"`
		Priority *int    `json:"priority"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	ctx, id := r.Context(), r.PathValue("id")
	var err error
	switch r.PathValue("action") {
	case "assign":
		if in.Assignee == nil {
			httpError(w, http.StatusBadRequest, "give an assignee")
			return
		}
		if *in.Assignee != "" && !s.knownAgent(w, *in.Assignee) {
			return
		}
		_, err = st.Update(ctx, id, board.User, board.Update{Assignee: in.Assignee})
	case "priority":
		if in.Priority == nil {
			httpError(w, http.StatusBadRequest, "give a priority")
			return
		}
		_, err = st.Update(ctx, id, board.User, board.Update{Priority: in.Priority})
	case "comment":
		err = st.Comment(ctx, id, board.User, in.Text)
	case "cancel":
		_, err = st.Cancel(ctx, id, board.User)
	case "reopen":
		_, err = st.Update(ctx, id, board.User, board.Update{Status: board.Todo})
	default:
		httpError(w, http.StatusNotFound, "unknown action")
		return
	}
	if err != nil {
		userError(w, err)
		return
	}
	s.boardWake()
	c, err := st.Get(ctx, id)
	if err != nil {
		s.fail(w, "read the card", err)
		return
	}
	writeJSON(w, http.StatusOK, viewCard(c))
}

// Schedules

type scheduleView struct {
	ID         string          `json:"id"`
	Agent      string          `json:"agent"`
	Name       string          `json:"name"`
	Spec       string          `json:"spec"`
	Prompt     string          `json:"prompt,omitempty"`
	Command    string          `json:"command,omitempty"`
	OnOutput   string          `json:"on_output,omitempty"`
	DeliverTo  json.RawMessage `json:"deliver_to,omitempty"`
	Enabled    bool            `json:"enabled"`
	NextRun    *time.Time      `json:"next_run,omitempty"`
	LastRun    *time.Time      `json:"last_run,omitempty"`
	LastStatus string          `json:"last_status,omitempty"`
	LastOutput string          `json:"last_output,omitempty"`
	FailStreak int             `json:"fail_streak,omitempty"`
	Created    time.Time       `json:"created_at"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() || t.UnixMilli() == 0 {
		return nil
	}
	u := t.UTC()
	return &u
}

func viewSchedule(sc *schedule.Schedule) scheduleView {
	v := scheduleView{ID: sc.ID, Agent: sc.Agent, Name: sc.Name, Spec: sc.Spec, Prompt: sc.Prompt, Command: sc.Command,
		Enabled: sc.Enabled, NextRun: timePtr(sc.NextRun), LastRun: timePtr(sc.LastRun), LastStatus: sc.LastStatus,
		LastOutput: sc.LastOutput, FailStreak: sc.FailStreak, Created: sc.CreatedAt.UTC()}
	if sc.Command != "" {
		v.OnOutput = sc.OnOutput
	}
	if len(sc.DeliverTo) > 0 && json.Valid(sc.DeliverTo) {
		v.DeliverTo = sc.DeliverTo
	}
	return v
}

func (s *Server) schedules(w http.ResponseWriter) (*schedule.Store, *schedule.Scheduler) {
	d := s.dash(w)
	if d == nil {
		return nil, nil
	}
	if d.Schedules == nil || d.Scheduler == nil {
		notAvailable(w, "schedules")
		return nil, nil
	}
	return d.Schedules, d.Scheduler
}

func (s *Server) scheduleList(w http.ResponseWriter, r *http.Request) {
	st, _ := s.schedules(w)
	if st == nil {
		return
	}
	agentName := r.URL.Query().Get("agent")
	list, err := st.List(r.Context(), agentName)
	if err != nil {
		s.fail(w, "list schedules", err)
		return
	}
	out := make([]scheduleView, 0, len(list))
	for i := range list {
		out = append(out, viewSchedule(&list[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": out})
}

// scheduleCreate adds a prompt schedule. Script schedules run shell
// commands unattended, so they stay a CLI-only thing (habakkuk schedules add
// -run), as with the agents' schedule_create tool.
func (s *Server) scheduleCreate(w http.ResponseWriter, r *http.Request) {
	st, _ := s.schedules(w)
	if st == nil {
		return
	}
	var in struct {
		Agent     string `json:"agent"`
		Name      string `json:"name"`
		When      string `json:"when"`
		Prompt    string `json:"prompt"`
		DeliverTo string `json:"deliver_to"` // ACCOUNT:CONVERSATION[:THREAD]; empty keeps results in the session
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !s.knownAgent(w, in.Agent) {
		return
	}
	sc := schedule.Schedule{Agent: in.Agent, Name: strings.TrimSpace(in.Name), Spec: in.When, Prompt: strings.TrimSpace(in.Prompt)}
	if in.DeliverTo != "" {
		to, err := s.replyAddress(in.DeliverTo)
		if err != nil {
			userError(w, err)
			return
		}
		sc.DeliverTo, _ = json.Marshal(to)
	}
	created, err := st.Create(r.Context(), sc)
	if err != nil {
		userError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, viewSchedule(created))
}

// replyAddress parses ACCOUNT:CONVERSATION[:THREAD] for a configured
// channel account. A web conversation id may itself hold colons, so for
// the web account everything after the account is the conversation.
func (s *Server) replyAddress(spec string) (channel.Address, error) {
	account, rest, ok := strings.Cut(strings.TrimSpace(spec), ":")
	if !ok || rest == "" {
		return channel.Address{}, fmt.Errorf("deliver_to %q: use ACCOUNT:CONVERSATION", spec)
	}
	if _, known := s.Gateway.Channels[account]; !known {
		return channel.Address{}, fmt.Errorf("deliver_to: no channel account %q", account)
	}
	to := channel.Address{Account: account, Conversation: rest}
	if account != s.Account {
		if conv, thread, ok := strings.Cut(rest, ":"); ok {
			to.Conversation, to.Thread = conv, thread
		}
	} else if !conversationID.MatchString(rest) {
		return channel.Address{}, fmt.Errorf("deliver_to: bad web conversation id %q", rest)
	}
	return to, nil
}

// scheduleAction is pause, resume or run (one extra run now).
func (s *Server) scheduleAction(w http.ResponseWriter, r *http.Request) {
	st, sch := s.schedules(w)
	if st == nil {
		return
	}
	ctx, id := r.Context(), r.PathValue("id")
	if _, err := st.Get(ctx, id); err != nil {
		httpError(w, http.StatusNotFound, "no such schedule")
		return
	}
	out := map[string]any{}
	var err error
	switch r.PathValue("action") {
	case "pause":
		err = st.SetEnabled(ctx, id, false)
	case "resume":
		err = st.SetEnabled(ctx, id, true)
	case "run":
		var job string
		job, err = sch.RunNow(ctx, id)
		out["job"] = job
	default:
		httpError(w, http.StatusNotFound, "unknown action")
		return
	}
	if err != nil {
		userError(w, err)
		return
	}
	sc, err := st.Get(ctx, id)
	if err != nil {
		s.fail(w, "read the schedule", err)
		return
	}
	out["schedule"] = viewSchedule(sc)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) scheduleDelete(w http.ResponseWriter, r *http.Request) {
	st, _ := s.schedules(w)
	if st == nil {
		return
	}
	if err := st.Delete(r.Context(), r.PathValue("id")); err != nil {
		httpError(w, http.StatusNotFound, "no such schedule")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Memory

type memoryView struct {
	Slug    string    `json:"slug"`
	Title   string    `json:"title"`
	Tags    []string  `json:"tags"`
	Updated time.Time `json:"updated_at"`
	Created time.Time `json:"created_at,omitzero"`
	Body    string    `json:"body,omitempty"`
	Snippet string    `json:"snippet,omitempty"`
}

func viewMemory(m memory.Memory) memoryView {
	return memoryView{Slug: m.Slug, Title: m.Title, Tags: nonNil(m.Tags), Updated: m.Updated.UTC(), Created: m.Created.UTC(), Body: m.Body}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func (s *Server) memoryStore(w http.ResponseWriter, agentName string) *memory.Store {
	d := s.dash(w)
	if d == nil {
		return nil
	}
	if d.Memory == nil {
		notAvailable(w, "memory")
		return nil
	}
	if !s.knownAgent(w, agentName) {
		return nil
	}
	return d.Memory
}

// memoryList lists an agent's notes, newest first, or searches them with
// ?q= (and ?tags=a,b).
func (s *Server) memoryList(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent")
	st := s.memoryStore(w, agentName)
	if st == nil {
		return
	}
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var tags []string
	for t := range strings.SplitSeq(r.URL.Query().Get("tags"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	out := []memoryView{}
	if q != "" || len(tags) > 0 {
		hits, err := st.Search(ctx, agentName, q, tags, intParam(r, "limit", 50, 1, 50))
		if err != nil {
			userError(w, err)
			return
		}
		for _, h := range hits {
			out = append(out, memoryView{Slug: h.Slug, Title: h.Title, Tags: nonNil(h.Tags), Updated: h.Updated, Snippet: h.Snippet})
		}
		writeJSON(w, http.StatusOK, map[string]any{"memories": out, "total": len(out)})
		return
	}
	list, total, err := st.Recent(ctx, agentName, intParam(r, "limit", 200, 1, 1000))
	if err != nil {
		s.fail(w, "list memories", err)
		return
	}
	for _, e := range list {
		out = append(out, memoryView{Slug: e.Slug, Title: e.Title, Tags: nonNil(e.Tags), Updated: e.Updated})
	}
	writeJSON(w, http.StatusOK, map[string]any{"memories": out, "total": total})
}

func (s *Server) memoryGet(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent")
	st := s.memoryStore(w, agentName)
	if st == nil {
		return
	}
	m, err := st.Read(r.Context(), agentName, r.PathValue("slug"))
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, viewMemory(m))
}

// memorySave creates a note (POST) or replaces one (PUT). A note's slug
// comes from its title, so renaming one saves it under the new slug and
// moves the old file to the trash.
func (s *Server) memorySave(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent")
	st := s.memoryStore(w, agentName)
	if st == nil {
		return
	}
	var in struct {
		Title   string    `json:"title"`
		Content string    `json:"content"`
		Tags    *[]string `json:"tags"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	ctx, old := dashActor(r), r.PathValue("slug")
	if old != "" {
		if _, err := st.Read(ctx, agentName, old); err != nil {
			httpError(w, http.StatusNotFound, err.Error())
			return
		}
	}
	var tags []string
	if in.Tags != nil {
		tags = nonNil(*in.Tags)
	}
	m, created, err := st.Save(ctx, agentName, in.Title, in.Content, tags)
	if err != nil {
		userError(w, err)
		return
	}
	if old != "" && old != m.Slug {
		if _, err := st.Forget(ctx, agentName, old); err != nil {
			s.Log.Warn("dashboard: forget renamed memory failed", "slug", old, "err", err)
		}
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, viewMemory(m))
}

// memoryForget moves a note to the agent's memory/.trash/.
func (s *Server) memoryForget(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent")
	st := s.memoryStore(w, agentName)
	if st == nil {
		return
	}
	if _, err := st.Forget(dashActor(r), agentName, r.PathValue("slug")); err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Skills

type skillView struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Shared      bool     `json:"shared"`
	Body        string   `json:"body,omitempty"`
	Files       []string `json:"files,omitempty"`
}

func (s *Server) skillList(w http.ResponseWriter, r *http.Request) {
	d := s.dash(w)
	if d == nil {
		return
	}
	if d.Skills == nil {
		notAvailable(w, "skills")
		return
	}
	agentName := r.PathValue("agent")
	if !s.knownAgent(w, agentName) {
		return
	}
	list, err := d.Skills.List(agentName)
	if err != nil {
		s.fail(w, "list skills", err)
		return
	}
	out := make([]skillView, 0, len(list))
	for _, sk := range list {
		out = append(out, skillView{Name: sk.Name, Description: sk.Description, Shared: sk.Shared})
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": out})
}

func (s *Server) skillGet(w http.ResponseWriter, r *http.Request) {
	d := s.dash(w)
	if d == nil {
		return
	}
	if d.Skills == nil {
		notAvailable(w, "skills")
		return
	}
	agentName := r.PathValue("agent")
	if !s.knownAgent(w, agentName) {
		return
	}
	sk, body, files, err := d.Skills.View(agentName, r.PathValue("name"))
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, skillView{Name: sk.Name, Description: sk.Description, Shared: sk.Shared, Body: body, Files: files})
}

// Usage

type usageRow struct {
	Key        string  `json:"key"`
	Calls      int     `json:"calls"`
	Input      int64   `json:"input_tokens"`
	Output     int64   `json:"output_tokens"`
	CacheRead  int64   `json:"cache_read_tokens"`
	CacheWrite int64   `json:"cache_write_tokens"`
	Cost       float64 `json:"cost"`
	Unpriced   int     `json:"unpriced"`
}

func (u *usageRow) add(c usage.Call, cost float64, priced bool) {
	u.Calls++
	u.Input += c.InputTokens
	u.Output += c.OutputTokens
	u.CacheRead += c.CacheReadTokens
	u.CacheWrite += c.CacheWriteTokens
	u.Cost += cost
	if !priced {
		u.Unpriced++
	}
}

func sortedRows(m map[string]*usageRow) []usageRow {
	out := make([]usageRow, 0, len(m))
	for _, r := range m {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b usageRow) int {
		if a.Cost != b.Cost {
			if a.Cost > b.Cost {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Key, b.Key)
	})
	return out
}

// usageReport totals tokens and cost over ?days= (default 30, at most 400)
// by day (in the configured timezone), by model and by agent.
func (s *Server) usageReport(w http.ResponseWriter, r *http.Request) {
	d := s.dash(w)
	if d == nil {
		return
	}
	if d.Usage == nil {
		notAvailable(w, "usage")
		return
	}
	loc := s.location()
	days := intParam(r, "days", 30, 1, 400)
	now := time.Now().In(loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(days - 1))
	agentName := r.URL.Query().Get("agent")
	calls, err := d.Usage.Calls(r.Context(), usage.Filter{Since: start, Agent: agentName})
	if err != nil {
		s.fail(w, "read usage", err)
		return
	}
	byDay := map[string]*usageRow{}
	var dayKeys []string
	for t := start; !t.After(now); t = t.AddDate(0, 0, 1) {
		k := t.Format(time.DateOnly)
		byDay[k] = &usageRow{Key: k}
		dayKeys = append(dayKeys, k)
	}
	byModel, byAgent := map[string]*usageRow{}, map[string]*usageRow{}
	total := &usageRow{Key: "total"}
	row := func(m map[string]*usageRow, k string) *usageRow {
		if m[k] == nil {
			m[k] = &usageRow{Key: k}
		}
		return m[k]
	}
	for _, c := range calls {
		cost, priced := d.Pricer.Cost(c)
		total.add(c, cost, priced)
		row(byDay, c.At.In(loc).Format(time.DateOnly)).add(c, cost, priced)
		row(byModel, c.Provider+"/"+c.Model).add(c, cost, priced)
		a := c.Agent
		if a == "" {
			a = "(none)"
		}
		row(byAgent, a).add(c, cost, priced)
	}
	daysOut := make([]usageRow, 0, len(dayKeys))
	for _, k := range dayKeys {
		daysOut = append(daysOut, *byDay[k])
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"since": start.Format(time.DateOnly), "timezone": loc.String(), "total": total,
		"days": daysOut, "by_model": sortedRows(byModel), "by_agent": sortedRows(byAgent),
	})
}

// budgetReport is spend today and this month against the spending limits,
// for the install and each agent (?agent= keeps one).
func (s *Server) budgetReport(w http.ResponseWriter, r *http.Request) {
	d := s.dash(w)
	if d == nil {
		return
	}
	if d.Budget == nil {
		notAvailable(w, "spending limits")
		return
	}
	rep := d.Budget()
	if name := r.URL.Query().Get("agent"); name != "" {
		kept := []budget.Scope{}
		for _, a := range rep.Agents {
			if a.Agent == name {
				kept = append(kept, a)
			}
		}
		rep.Agents = kept
	}
	if rep.Agents == nil {
		rep.Agents = []budget.Scope{}
	}
	writeJSON(w, http.StatusOK, rep)
}

// Approvals

func (s *Server) approvals(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"approvals": nonNil(s.Gateway.Approvals())})
}

// approve answers a waiting approval: {"approve": true} allows the call,
// false denies it.
func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Approve *bool `json:"approve"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Approve == nil {
		httpError(w, http.StatusBadRequest, `give {"approve": true} or {"approve": false}`)
		return
	}
	if !s.Gateway.Decide(r.PathValue("id"), *in.Approve) {
		httpError(w, http.StatusNotFound, "that approval is no longer waiting")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"approved": *in.Approve})
}
