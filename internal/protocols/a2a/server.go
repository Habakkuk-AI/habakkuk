package a2a

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
)

// Server exposes agents over A2A. Each message starts a Habakkuk turn: the
// task id is the turn's job id, the context id is the session id, and the
// task's artifact is the reply. Routes:
//
//	GET  /.well-known/agent-card.json           one card, a skill per agent
//	POST /a2a                                   JSON-RPC; the agent from metadata.agent, else the default
//	GET  /a2a/{agent}/.well-known/agent-card.json  that agent's own card
//	POST /a2a/{agent}                           JSON-RPC for that agent
type Server struct {
	Runner *agent.Runner
	// Agents maps each served agent to the description other agents see.
	Agents      map[string]string
	Name        string // the combined card's name
	Description string
	Version     string // Habakkuk's version, for the card
	// PublicURL is how clients reach this server; empty uses the request's
	// host.
	PublicURL string
	// Token, when set, is the bearer token every JSON-RPC call needs. The
	// cards are public, as discovery expects.
	Token string
	// Contexts remembers the sessions A2A started, so a client can only
	// continue (and read tasks of) those, not a chat's sessions. nil allows
	// any session of a served agent.
	Contexts ContextStore
	Log      *slog.Logger
}

// ContextStore keeps keyed session bindings (web.db's api_sessions).
type ContextStore interface {
	APISession(ctx context.Context, key string) (agent, sessionID string, err error)
	BindAPISession(ctx context.Context, key, agent, sessionID string) error
	TouchAPISession(ctx context.Context, key string) error
}

func contextKey(session string) string { return "a2a:" + session }

// ours reports whether A2A started the session.
func (s *Server) ours(ctx context.Context, session string) bool {
	if s.Contexts == nil {
		return true
	}
	_, sid, err := s.Contexts.APISession(ctx, contextKey(session))
	if err != nil || sid != session {
		return false
	}
	_ = s.Contexts.TouchAPISession(ctx, contextKey(session))
	return true
}

const (
	maxBody      = 1 << 20
	maxText      = 100_000
	streamPing   = 15 * time.Second
	streamBatch  = 300 * time.Millisecond
	replyID      = "reply"
	headerPrefix = "[A2A message from another agent]\n"
)

// Handler routes the A2A endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+CardPath, func(w http.ResponseWriter, r *http.Request) { s.card(w, r, "") })
	mux.HandleFunc("GET /a2a/{agent}"+CardPath, func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("agent")
		if _, ok := s.Agents[name]; !ok {
			http.NotFound(w, r)
			return
		}
		s.card(w, r, name)
	})
	mux.HandleFunc("POST /a2a", func(w http.ResponseWriter, r *http.Request) { s.rpc(w, r, "") })
	mux.HandleFunc("POST /a2a/{agent}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("agent")
		if _, ok := s.Agents[name]; !ok {
			http.NotFound(w, r)
			return
		}
		s.rpc(w, r, name)
	})
	return mux
}

func (s *Server) base(r *http.Request) string {
	if s.PublicURL != "" {
		return strings.TrimRight(s.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// requestVersion reads A2A-Version (header or query): empty means 0.3.
func requestVersion(r *http.Request) (Version, bool) {
	v := r.Header.Get("A2A-Version")
	if v == "" {
		v = r.URL.Query().Get("A2A-Version")
	}
	switch {
	case v == "", strings.HasPrefix(v, "0.3"):
		return V03, true
	case v == "1", strings.HasPrefix(v, "1."):
		return V10, true
	}
	return V03, false
}

func (s *Server) names() []string { return slices.Sorted(maps.Keys(s.Agents)) }

// defaultAgent serves messages that name no agent: habakkuk when served,
// else the first by name.
func (s *Server) defaultAgent() string {
	if _, ok := s.Agents["habakkuk"]; ok {
		return "habakkuk"
	}
	if names := s.names(); len(names) > 0 {
		return names[0]
	}
	return ""
}

func (s *Server) card(w http.ResponseWriter, r *http.Request, only string) {
	v, _ := requestVersion(r)
	info := CardInfo{Name: s.Name, Description: s.Description, Version: s.Version, Streaming: true, Bearer: s.Token != "",
		Endpoint: s.base(r) + "/a2a"}
	if info.Description == "" {
		info.Description = "Habakkuk, a personal AI agent. Each skill is one of its agents; name it in the message metadata as \"agent\"" +
			" (default " + s.defaultAgent() + "), or use that agent's own card at /a2a/<agent>" + CardPath + "."
	}
	for _, name := range s.names() {
		if only != "" && name != only {
			continue
		}
		info.Skills = append(info.Skills, Skill{ID: name, Name: name, Description: s.describe(name), Tags: []string{"habakkuk", "agent"}})
	}
	if only != "" {
		info.Name, info.Description, info.Endpoint = only, s.describe(only), s.base(r)+"/a2a/"+only
	}
	w.Header().Set("Cache-Control", "max-age=300")
	writeJSON(w, http.StatusOK, EncodeCard(v, info))
}

func (s *Server) describe(name string) string {
	if d := s.Agents[name]; d != "" {
		return d
	}
	return "The Habakkuk agent " + name + ": it works with its own tools, memory and skills and replies in text."
}

// --- JSON-RPC ---

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func rpcResult(id json.RawMessage, result any) map[string]any {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcFail(id json.RawMessage, e *RPCError) map[string]any {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": e}
}

func rpcErr(code int, format string, args ...any) *RPCError {
	return &RPCError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// methods maps both versions' method names to an operation.
var methods = map[string]struct {
	op string
	v  Version
}{
	"message/send": {"send", V03}, "message/stream": {"stream", V03}, "tasks/get": {"get", V03},
	"tasks/cancel": {"cancel", V03}, "tasks/resubscribe": {"subscribe", V03},
	"SendMessage": {"send", V10}, "SendStreamingMessage": {"stream", V10}, "GetTask": {"get", V10},
	"CancelTask": {"cancel", V10}, "SubscribeToTask": {"subscribe", V10},
}

func sameSecret(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

func (s *Server) rpc(w http.ResponseWriter, r *http.Request, pathAgent string) {
	if s.Token != "" {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !sameSecret(tok, s.Token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="habakkuk-a2a"`)
			writeJSON(w, http.StatusUnauthorized, rpcFail(nil, rpcErr(CodeInvalidRequest, "missing or wrong bearer token")))
			return
		}
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, rpcFail(nil, rpcErr(CodeInvalidRequest, "send application/json")))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, rpcFail(nil, rpcErr(CodeInvalidRequest, "request body over 1 MB")))
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusOK, rpcFail(nil, rpcErr(CodeParse, "parse error: %v", err)))
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeJSON(w, http.StatusOK, rpcFail(req.ID, rpcErr(CodeInvalidRequest, "not a JSON-RPC 2.0 request")))
		return
	}
	if _, ok := requestVersion(r); !ok {
		writeJSON(w, http.StatusOK, rpcFail(req.ID, rpcErr(CodeVersionUnsupported, "A2A-Version %q is not supported; use 0.3 or 1.0", r.Header.Get("A2A-Version"))))
		return
	}
	m, ok := methods[req.Method]
	if !ok {
		e := rpcErr(CodeMethodNotFound, "method %q not found", req.Method)
		if strings.Contains(req.Method, "ushNotification") {
			e = rpcErr(CodePushNotSupported, "push notifications are not supported")
		}
		writeJSON(w, http.StatusOK, rpcFail(req.ID, e))
		return
	}
	ctx := r.Context()
	switch m.op {
	case "send":
		res, e := s.send(ctx, m.v, pathAgent, req.Params)
		s.reply(w, req.ID, res, e)
	case "get":
		res, e := s.getTask(ctx, req.Params)
		s.reply(w, req.ID, encodeTaskResult(m.v, res), e)
	case "cancel":
		res, e := s.cancel(ctx, req.Params)
		s.reply(w, req.ID, encodeTaskResult(m.v, res), e)
	case "stream", "subscribe":
		s.stream(w, r, m.v, m.op, pathAgent, req)
	}
}

func encodeTaskResult(v Version, t *Task) any {
	if t == nil {
		return nil
	}
	return EncodeTask(v, t)
}

func (s *Server) reply(w http.ResponseWriter, id json.RawMessage, res any, e *RPCError) {
	if e != nil {
		writeJSON(w, http.StatusOK, rpcFail(id, e))
		return
	}
	writeJSON(w, http.StatusOK, rpcResult(id, res))
}

type sendParams struct {
	Message       json.RawMessage `json:"message"`
	Configuration struct {
		Blocking          *bool `json:"blocking"`          // 0.3: false returns at once
		ReturnImmediately bool  `json:"returnImmediately"` // 1.0
		HistoryLength     *int  `json:"historyLength"`
	} `json:"configuration"`
	Metadata map[string]any `json:"metadata"`
}

// started is a message turned into a queued (or found) Habakkuk turn.
type started struct {
	agent, session, job string
	text                string
	evs                 <-chan events.Event
	unsub               func()
}

func (st *started) close() {
	if st.unsub != nil {
		st.unsub()
	}
}

// begin validates a message and queues its turn. A message whose
// messageId was seen in the same context attaches to the first turn.
func (s *Server) begin(ctx context.Context, pathAgent string, raw json.RawMessage, subscribe bool) (*started, *sendParams, *RPCError) {
	var p sendParams
	if err := json.Unmarshal(raw, &p); err != nil || len(p.Message) == 0 {
		return nil, nil, rpcErr(CodeInvalidParams, "params.message is required")
	}
	msg, err := DecodeMessage(p.Message)
	if err != nil {
		return nil, nil, rpcErr(CodeInvalidParams, "message: %v", err)
	}
	switch {
	case msg.ID == "":
		return nil, nil, rpcErr(CodeInvalidParams, "message.messageId is required")
	case msg.Role != "user":
		return nil, nil, rpcErr(CodeInvalidParams, "message.role must be user")
	case len(msg.Parts) == 0:
		return nil, nil, rpcErr(CodeInvalidParams, "message.parts is empty")
	case msg.TaskID != "":
		// Every message starts a new task; Habakkuk's tasks never wait for
		// input, so there is no open task to add to.
		return nil, nil, rpcErr(CodeUnsupported, "task %s cannot take more messages: send a new message with contextId %q (and no taskId) to continue the conversation",
			msg.TaskID, msg.ContextID)
	}
	text, e := messageText(msg)
	if e != nil {
		return nil, nil, e
	}
	name, explicit := pathAgent, pathAgent != ""
	if !explicit {
		for _, md := range []map[string]any{msg.Metadata, p.Metadata} {
			if a, ok := md["agent"].(string); ok && a != "" {
				name, explicit = a, true
				break
			}
		}
	}
	if !explicit {
		name = s.defaultAgent()
	}
	if _, ok := s.Agents[name]; !ok {
		return nil, nil, rpcErr(CodeInvalidParams, "no agent %q here; agents: %s", name, strings.Join(s.names(), ", "))
	}
	runner := s.Runner
	sum := sha256.Sum256([]byte("a2a\x00" + name + "\x00" + msg.ContextID + "\x00" + msg.ID))
	id := "turn_" + hex.EncodeToString(sum[:12])
	st := &started{agent: name, job: id, text: text}
	if j, err := runner.Queue.Get(ctx, id); err == nil && j != nil {
		var tp agent.TurnPayload
		if err := j.Decode(&tp); err != nil {
			return nil, nil, rpcErr(CodeInternal, "read task: %v", err)
		}
		st.session = tp.Session
		if subscribe && runner.Bus != nil {
			st.evs, st.unsub = runner.Bus.Subscribe(st.session)
		}
		return st, &p, nil
	}
	if msg.ContextID != "" {
		sess, err := runner.Sessions.Get(ctx, msg.ContextID)
		if err != nil || !s.ours(ctx, sess.ID) {
			return nil, nil, rpcErr(CodeInvalidParams, "unknown contextId %q; omit it to start a new conversation", msg.ContextID)
		}
		if _, served := s.Agents[sess.Agent]; !served {
			return nil, nil, rpcErr(CodeInvalidParams, "unknown contextId %q", msg.ContextID)
		}
		if sess.Agent != name {
			if explicit {
				return nil, nil, rpcErr(CodeInvalidParams, "context %s belongs to agent %s, not %s", sess.ID, sess.Agent, name)
			}
			st.agent = sess.Agent
		}
		st.session = sess.ID
	} else {
		sess, err := runner.NewSession(ctx, name)
		if err != nil {
			s.Log.Error("a2a session failed", "err", err)
			return nil, nil, rpcErr(CodeInternal, "could not start a session")
		}
		st.session = sess.ID
		if s.Contexts != nil {
			if err := s.Contexts.BindAPISession(ctx, contextKey(sess.ID), name, sess.ID); err != nil {
				s.Log.Error("a2a context failed", "err", err)
				return nil, nil, rpcErr(CodeInternal, "could not record the context")
			}
		}
	}
	if subscribe && runner.Bus != nil {
		st.evs, st.unsub = runner.Bus.Subscribe(st.session) // before queueing, so no event slips by
	}
	if _, err := runner.Begin(ctx, agent.Turn{ID: id, Session: st.session, Text: headerPrefix + text, Source: agent.SourceA2A}); err != nil {
		st.close()
		if errors.Is(err, agent.ErrBusy) {
			return nil, nil, rpcErr(CodeUnsupported, "context %s already has a task running; wait for it (tasks/get) or use another context", st.session)
		}
		s.Log.Error("a2a turn failed to queue", "err", err)
		return nil, nil, rpcErr(CodeInternal, "could not queue the task")
	}
	return st, &p, nil
}

// messageText turns a message into the turn's text. Text and data parts
// are kept, files by URL are named; inline files must be text.
func messageText(m *Message) (string, *RPCError) {
	var out []string
	for _, p := range m.Parts {
		switch {
		case p.IsText:
			out = append(out, p.Text)
		case len(p.Data) > 0:
			out = append(out, "```json\n"+string(p.Data)+"\n```")
		case p.URL != "":
			out = append(out, fmt.Sprintf("[file %q (%s) at %s]", p.Filename, p.MediaType, p.URL))
		case strings.HasPrefix(p.MediaType, "text/") && utf8.Valid(p.Raw):
			out = append(out, fmt.Sprintf("[file %q]\n%s", p.Filename, p.Raw))
		default:
			return "", rpcErr(CodeContentType, "file parts of type %q are not supported; send text, data, text files or file URLs", p.MediaType)
		}
	}
	text := strings.TrimSpace(strings.Join(out, "\n\n"))
	switch {
	case text == "":
		return "", rpcErr(CodeInvalidParams, "the message has no text")
	case utf8.RuneCountInString(text) > maxText:
		return "", rpcErr(CodeInvalidParams, "the message is longer than %d characters", maxText)
	}
	return text, nil
}

func (s *Server) send(ctx context.Context, v Version, pathAgent string, raw json.RawMessage) (any, *RPCError) {
	st, p, e := s.begin(ctx, pathAgent, raw, false)
	if e != nil {
		return nil, e
	}
	wait := !p.Configuration.ReturnImmediately && (p.Configuration.Blocking == nil || *p.Configuration.Blocking)
	if wait {
		// A client that goes away stops the wait, not the task.
		_, _ = s.Runner.Await(ctx, st.job, nil, nil)
	}
	t, e := s.taskByID(context.WithoutCancel(ctx), st.job)
	if e != nil {
		return nil, e
	}
	trimHistory(t, p.Configuration.HistoryLength)
	return EncodeEvent(v, Event{Task: t}), nil
}

func trimHistory(t *Task, n *int) {
	if n != nil && *n >= 0 && len(t.History) > *n {
		t.History = t.History[len(t.History)-*n:]
	}
}

type taskParams struct {
	ID            string `json:"id"`
	HistoryLength *int   `json:"historyLength"`
}

func (s *Server) getTask(ctx context.Context, raw json.RawMessage) (*Task, *RPCError) {
	var p taskParams
	if err := json.Unmarshal(raw, &p); err != nil || p.ID == "" {
		return nil, rpcErr(CodeInvalidParams, "params.id is required")
	}
	t, e := s.taskByID(ctx, p.ID)
	if e != nil {
		return nil, e
	}
	trimHistory(t, p.HistoryLength)
	return t, nil
}

func (s *Server) cancel(ctx context.Context, raw json.RawMessage) (*Task, *RPCError) {
	var p taskParams
	if err := json.Unmarshal(raw, &p); err != nil || p.ID == "" {
		return nil, rpcErr(CodeInvalidParams, "params.id is required")
	}
	t, e := s.taskByID(ctx, p.ID)
	if e != nil {
		return nil, e
	}
	if Terminal(t.State) {
		return nil, rpcErr(CodeTaskNotCancelable, "task %s is already %s", t.ID, t.State)
	}
	if err := s.Runner.Queue.Cancel(ctx, p.ID); err != nil {
		return nil, rpcErr(CodeInternal, "cancel: %v", err)
	}
	return s.taskByID(ctx, p.ID)
}

// taskByID reads a turn job as a task. Only turns of served agents are
// tasks; anything else is not found.
func (s *Server) taskByID(ctx context.Context, id string) (*Task, *RPCError) {
	j, err := s.Runner.Queue.Get(ctx, id)
	if err != nil || j == nil || j.Kind != agent.KindTurn {
		return nil, rpcErr(CodeTaskNotFound, "task %s not found", id)
	}
	if _, ok := s.Agents[j.Agent]; !ok {
		return nil, rpcErr(CodeTaskNotFound, "task %s not found", id)
	}
	var p agent.TurnPayload
	if err := j.Decode(&p); err != nil {
		return nil, rpcErr(CodeInternal, "read task: %v", err)
	}
	if !s.ours(ctx, p.Session) {
		return nil, rpcErr(CodeTaskNotFound, "task %s not found", id)
	}
	return taskFromJob(j, p), nil
}

func taskFromJob(j *jobs.Job, p agent.TurnPayload) *Task {
	t := &Task{ID: j.ID, ContextID: p.Session, Timestamp: time.Now()}
	user := Message{ID: j.ID + "-user", Role: "user", ContextID: p.Session, TaskID: j.ID,
		Parts: []Part{TextPart(strings.TrimPrefix(p.Text, headerPrefix))}}
	t.History = []Message{user}
	switch j.Status {
	case jobs.Pending:
		t.State = StateSubmitted
		if j.Attempts > 0 {
			t.State = StateWorking
		}
	case jobs.Running:
		t.State = StateWorking
	case jobs.Done:
		t.State = StateCompleted
		reply := Message{ID: j.ID + "-reply", Role: "agent", ContextID: p.Session, TaskID: j.ID, Parts: []Part{TextPart(j.Result)}}
		t.Artifacts = []Artifact{{ID: replyID, Name: "reply", Parts: []Part{TextPart(j.Result)}}}
		t.History = append(t.History, reply)
	case jobs.Failed:
		t.State = StateFailed
		t.Status = &Message{ID: j.ID + "-error", Role: "agent", ContextID: p.Session, TaskID: j.ID,
			Parts: []Part{TextPart("The task failed: " + j.LastError)}}
	case jobs.Canceled:
		t.State = StateCanceled
	default:
		t.State = StateUnknown
	}
	return t
}

// --- streaming ---

// sseWriter writes JSON-RPC responses as Server-Sent Events; a keepalive
// goroutine shares it.
type sseWriter struct {
	mu sync.Mutex
	w  http.ResponseWriter
	rc *http.ResponseController
	id json.RawMessage
	v  Version
}

func (s *sseWriter) event(e Event) {
	s.write(rpcResult(s.id, EncodeEvent(s.v, e)))
}

func (s *sseWriter) write(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.w, "data: %s\n\n", data)
	_ = s.rc.Flush()
}

func (s *sseWriter) ping() {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprint(s.w, ": keepalive\n\n")
	_ = s.rc.Flush()
}

// stream answers message/stream (and SendStreamingMessage) and
// tasks/resubscribe (SubscribeToTask): the task, then status updates while
// it works (with the model's text so far as the status message), then the
// reply as an artifact and the final status. The stream closes when the
// task ends.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, v Version, op, pathAgent string, req rpcRequest) {
	ctx := r.Context()
	var st *started
	if op == "subscribe" {
		t, e := s.getTask(ctx, req.Params)
		if e != nil {
			writeJSON(w, http.StatusOK, rpcFail(req.ID, e))
			return
		}
		st = &started{job: t.ID, session: t.ContextID}
		if s.Runner.Bus != nil {
			st.evs, st.unsub = s.Runner.Bus.Subscribe(st.session)
		}
	} else {
		var e *RPCError
		if st, _, e = s.begin(ctx, pathAgent, req.Params, true); e != nil {
			writeJSON(w, http.StatusOK, rpcFail(req.ID, e))
			return
		}
	}
	defer st.close()
	t, e := s.taskByID(ctx, st.job)
	if e != nil {
		writeJSON(w, http.StatusOK, rpcFail(req.ID, e))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	out := &sseWriter{w: w, rc: http.NewResponseController(w), id: req.ID, v: v}
	out.event(Event{Task: t})
	if Terminal(t.State) {
		s.finish(out, t)
		return
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		tick := time.NewTicker(streamPing)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				out.ping()
			}
		}
	}()
	out.event(Event{Status: &StatusUpdate{TaskID: t.ID, ContextID: t.ContextID, State: StateWorking}})
	var text strings.Builder
	var sent time.Time
	flush := func(force bool) {
		if text.Len() == 0 || (!force && time.Since(sent) < streamBatch) {
			return
		}
		sent = time.Now()
		out.event(Event{Status: &StatusUpdate{TaskID: t.ID, ContextID: t.ContextID, State: StateWorking,
			Message: &Message{ID: fmt.Sprintf("%s-progress-%d", t.ID, sent.UnixNano()), Role: "agent", ContextID: t.ContextID,
				TaskID: t.ID, Parts: []Part{TextPart(text.String())}, Metadata: map[string]any{"partial": true}}}})
	}
	_, _ = s.Runner.Await(ctx, st.job, st.evs, func(ev events.Event) {
		switch ev.Kind {
		case events.Text:
			text.WriteString(ev.Text)
			flush(false)
		case events.ToolStart:
			flush(true)
		}
	})
	if ctx.Err() != nil {
		return // the client went away; the task keeps running
	}
	final, e := s.taskByID(context.WithoutCancel(ctx), st.job)
	if e != nil {
		return
	}
	s.finish(out, final)
}

// finish sends a finished task's artifact and final status.
func (s *Server) finish(out *sseWriter, t *Task) {
	for _, a := range t.Artifacts {
		out.event(Event{Artifact: &ArtifactUpdate{TaskID: t.ID, ContextID: t.ContextID, Artifact: a, LastChunk: true}})
	}
	out.event(Event{Status: &StatusUpdate{TaskID: t.ID, ContextID: t.ContextID, State: t.State, Message: t.Status, Final: true}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
