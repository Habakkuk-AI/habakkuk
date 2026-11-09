package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// The OpenAI-compatible API: /v1/models, /v1/chat/completions and
// /v1/responses, behind the same bearer token as /api. Each model is one
// agent ("habakkuk/<agent>"), which runs with its own tools; tools the client
// defines are not supported. A request maps to a Habakkuk session:
//
//   - X-Habakkuk-Session names a session to continue; the request's last user
//     message is the new turn and the rest of the array is ignored.
//   - The "user" field (or, for /v1/responses, previous_response_id) does
//     the same through a key Habakkuk remembers.
//   - Otherwise the turn runs in a fresh session whose transcript is seeded
//     from the earlier messages in the request.
//
// Every response carries X-Habakkuk-Session. An Idempotency-Key header makes
// the turn's ID derive from the key, so a retried request attaches to the
// first one's turn (running or finished) and gets the same answer.

const (
	modelPrefix   = "habakkuk/"
	sessionHeader = "X-Habakkuk-Session"
	apiKeepalive  = 15 * time.Second
)

// apiError is an error in the shape OpenAI clients parse.
type apiError struct {
	status int
	typ    string // invalid_request_error | authentication_error | not_found_error | conflict_error | server_error
	msg    string
	param  string
	code   string
	final  bool // tell SDKs not to retry: the turn already ran and failed
}

func (e *apiError) Error() string { return e.msg }

func badRequest(param, format string, args ...any) *apiError {
	return &apiError{status: http.StatusBadRequest, typ: "invalid_request_error", param: param, msg: fmt.Sprintf(format, args...)}
}

func writeAPIError(w http.ResponseWriter, e *apiError) {
	if e.final {
		w.Header().Set("X-Should-Retry", "false")
	}
	writeJSON(w, e.status, map[string]any{"error": e.body()})
}

func (e *apiError) body() map[string]any {
	b := map[string]any{"message": e.msg, "type": e.typ, "param": nil, "code": nil}
	if e.param != "" {
		b["param"] = e.param
	}
	if e.code != "" {
		b["code"] = e.code
	}
	return b
}

// v1 guards an OpenAI route: the bearer token, and a JSON body on POST so
// browsers must preflight cross-site requests (which this server never
// approves). Errors come back in OpenAI's shape.
func (s *Server) v1(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" {
			tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || !sameSecret(tok, s.Token) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="habakkuk"`)
				writeAPIError(w, &apiError{status: http.StatusUnauthorized, typ: "authentication_error",
					code: "invalid_api_key", msg: "missing or wrong token"})
				return
			}
		}
		if r.Method == http.MethodPost {
			if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
				writeAPIError(w, &apiError{status: http.StatusUnsupportedMediaType, typ: "invalid_request_error", msg: "send application/json"})
				return
			}
		}
		next(w, r)
	}
}

type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (s *Server) modelNames() []string {
	names := make([]string, 0, len(s.Gateway.Runner.Agents))
	for name := range s.Gateway.Runner.Agents {
		names = append(names, modelPrefix+name)
	}
	slices.Sort(names)
	return names
}

func (s *Server) listModels(w http.ResponseWriter, _ *http.Request) {
	data := []modelObject{}
	for _, id := range s.modelNames() {
		data = append(data, modelObject{ID: id, Object: "model", OwnedBy: "habakkuk"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) getModel(w http.ResponseWriter, r *http.Request) {
	name, err := s.agentFor(r.PathValue("id"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, modelObject{ID: modelPrefix + name, Object: "model", OwnedBy: "habakkuk"})
}

// agentFor maps a model id to an agent: "habakkuk/<agent>", or a bare agent
// name.
func (s *Server) agentFor(model string) (string, *apiError) {
	agents := s.Gateway.Runner.Agents
	if name, ok := strings.CutPrefix(model, modelPrefix); ok {
		if _, known := agents[name]; known {
			return name, nil
		}
	}
	if _, known := agents[model]; known {
		return model, nil
	}
	if model == "" {
		return "", badRequest("model", "model is required: one of %s", strings.Join(s.modelNames(), ", "))
	}
	return "", &apiError{status: http.StatusNotFound, typ: "invalid_request_error", param: "model", code: "model_not_found",
		msg: fmt.Sprintf("the model %q does not exist; use one of %s", model, strings.Join(s.modelNames(), ", "))}
}

// convo is a request's conversation, before it is mapped to a session.
type convo struct {
	endpoint string // "chat" or "responses", for idempotency keys
	agent    string
	history  []llm.Message // earlier messages, to seed a fresh session
	text     string        // the new user message
	session  string        // a session to continue (header or previous response)
	key      string        // the client's "user" field
	idem     string        // Idempotency-Key
}

// apiTurn is a request's turn once it is queued (or found, on a retry).
type apiTurn struct {
	agent   string
	session string
	job     string
	created time.Time
	evs     <-chan events.Event // live events, when streaming
	unsub   func()
}

func (t *apiTurn) close() {
	if t.unsub != nil {
		t.unsub()
	}
}

// begin maps the conversation to a session and queues its turn. With an
// Idempotency-Key whose turn exists, it attaches to that turn instead.
func (s *Server) begin(ctx context.Context, c convo, stream bool) (*apiTurn, *apiError) {
	runner := s.Gateway.Runner
	id := ""
	if c.idem != "" {
		if len(c.idem) > 256 {
			return nil, badRequest("", "Idempotency-Key is longer than 256 characters")
		}
		sum := sha256.Sum256([]byte("openai\x00" + c.endpoint + "\x00" + c.idem))
		id = "turn_" + hex.EncodeToString(sum[:12])
		if j, err := s.Gateway.Queue.Get(ctx, id); err == nil && j != nil {
			var p agent.TurnPayload
			if err := j.Decode(&p); err != nil {
				return nil, &apiError{status: http.StatusInternalServerError, typ: "server_error", msg: err.Error()}
			}
			t := &apiTurn{agent: j.Agent, session: p.Session, job: j.ID, created: j.CreatedAt}
			s.subscribe(t, stream)
			return t, nil
		}
	}
	sessionID, err := s.sessionFor(ctx, c)
	if err != nil {
		return nil, err
	}
	if id == "" {
		id = jobs.NewID("turn")
	}
	t := &apiTurn{agent: c.agent, session: sessionID, job: id, created: time.Now()}
	s.subscribe(t, stream) // before queueing, so no event slips by
	if _, err := runner.Begin(ctx, agent.Turn{ID: id, Session: sessionID, Text: c.text}); err != nil {
		t.close()
		if errors.Is(err, agent.ErrBusy) {
			return nil, &apiError{status: http.StatusConflict, typ: "conflict_error", code: "session_busy", msg: err.Error()}
		}
		s.Log.Error("api turn failed to queue", "err", err)
		return nil, &apiError{status: http.StatusInternalServerError, typ: "server_error", msg: "could not queue the turn"}
	}
	return t, nil
}

func (s *Server) subscribe(t *apiTurn, stream bool) {
	if bus := s.Gateway.Runner.Bus; stream && bus != nil {
		t.evs, t.unsub = bus.Subscribe(t.session)
	}
}

// sessionFor finds the session a conversation continues, or starts one
// seeded with the conversation's history.
func (s *Server) sessionFor(ctx context.Context, c convo) (string, *apiError) {
	runner := s.Gateway.Runner
	if c.session != "" {
		sess, err := runner.Sessions.Get(ctx, c.session)
		if err != nil {
			return "", &apiError{status: http.StatusNotFound, typ: "not_found_error", code: "session_not_found",
				msg: fmt.Sprintf("session %q not found", c.session)}
		}
		if sess.Agent != c.agent {
			return "", badRequest("model", "session %s belongs to %s%s, not %s%s", sess.ID, modelPrefix, sess.Agent, modelPrefix, c.agent)
		}
		return sess.ID, nil
	}
	key := ""
	if c.key != "" {
		if len(c.key) > 256 {
			return "", badRequest("user", "user is longer than 256 characters")
		}
		key = "user:" + c.key
		ag, sid, err := s.Store.APISession(ctx, key)
		if err != nil {
			s.Log.Error("read api session failed", "err", err)
			return "", &apiError{status: http.StatusInternalServerError, typ: "server_error", msg: "could not read the session map"}
		}
		// A key keeps its session until the client asks for another agent
		// (or retention removed the session).
		if sid != "" && ag == c.agent {
			if _, err := runner.Sessions.Get(ctx, sid); err == nil {
				if err := s.Store.TouchAPISession(ctx, key); err != nil {
					s.Log.Warn("touch api session failed", "err", err)
				}
				return sid, nil
			}
		}
	}
	sess, err := runner.NewSession(ctx, c.agent)
	if err != nil {
		s.Log.Error("api session failed", "err", err)
		return "", &apiError{status: http.StatusInternalServerError, typ: "server_error", msg: "could not start a session"}
	}
	if err := runner.Seed(ctx, sess.ID, c.history); err != nil {
		s.Log.Error("seed api session failed", "err", err)
		return "", &apiError{status: http.StatusInternalServerError, typ: "server_error", msg: "could not record the conversation"}
	}
	if key != "" {
		if err := s.Store.BindAPISession(ctx, key, c.agent, sess.ID); err != nil {
			s.Log.Warn("bind api session failed", "err", err)
		}
	}
	return sess.ID, nil
}

// finish waits for a turn's reply. A client that goes away stops the wait,
// not the turn: retrying with the same Idempotency-Key picks it up again.
func (s *Server) finish(ctx context.Context, t *apiTurn, on func(events.Event)) (string, *apiError) {
	final, err := s.Gateway.Runner.Await(ctx, t.job, t.evs, on)
	if err == nil {
		return final, nil
	}
	if ctx.Err() != nil {
		return "", &apiError{status: 499, typ: "server_error", msg: "client went away"}
	}
	return "", &apiError{status: http.StatusInternalServerError, typ: "server_error", code: "turn_failed", msg: err.Error(), final: true}
}

// message is one message of a request, reduced to its role and text.
type message struct {
	role string // system (system and developer), user or assistant
	text string
}

// conversation splits a request's messages into the earlier history and
// the new user message (the trailing user messages, joined). System and
// developer messages become a note at the start, since the agent's own
// system prompt is fixed for the session.
func conversation(msgs []message, instructions string) ([]llm.Message, string, *apiError) {
	var notes []string
	if instructions != "" {
		notes = append(notes, instructions)
	}
	var rest []message
	for _, m := range msgs {
		if m.role == "system" {
			notes = append(notes, m.text)
			continue
		}
		rest = append(rest, m)
	}
	end := len(rest)
	for end > 0 && rest[end-1].role == "user" {
		end--
	}
	if end == len(rest) {
		return nil, "", badRequest("messages", "the last message must be from the user")
	}
	var parts []string
	for _, m := range rest[end:] {
		parts = append(parts, m.text)
	}
	text := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if text == "" {
		return nil, "", badRequest("messages", "the last user message is empty")
	}
	var history []llm.Message
	if len(notes) > 0 {
		note := "[Instructions from the API client]\n" + strings.Join(notes, "\n\n")
		if end == 0 {
			text = note + "\n\n" + text
		} else {
			history = append(history, llm.UserText(note))
		}
	}
	for _, m := range rest[:end] {
		history = append(history, llm.Message{Role: llm.Role(m.role), Blocks: []llm.Block{{Type: llm.Text, Text: m.text}}})
	}
	return history, text, nil
}

// contentText reads message content: a string, null, or an array of text
// parts. Images, audio and files are refused rather than dropped silently.
func contentText(raw json.RawMessage, param string) (string, *apiError) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", badRequest(param, "content must be a string or an array of text parts")
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text", "output_text":
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		default:
			return "", badRequest(param, "content part type %q is not supported; send text", p.Type)
		}
	}
	return b.String(), nil
}

// noTools refuses client-side tools: the agent runs with its own.
func noTools(param string, raw json.RawMessage) *apiError {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", "[]":
		return nil
	}
	return badRequest(param, "client-side tools are not supported: a Habakkuk agent runs with its own tools; remove %q", param)
}

func validRole(role string) (string, bool) {
	switch role {
	case "system", "developer":
		return "system", true
	case "user", "assistant":
		return role, true
	}
	return "", false
}

// sse writes a Server-Sent Events stream; a keepalive goroutine shares it.
type sse struct {
	mu sync.Mutex
	w  http.ResponseWriter
	rc *http.ResponseController
}

func startSSE(w http.ResponseWriter, session string) *sse {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	h.Set(sessionHeader, session)
	w.WriteHeader(http.StatusOK)
	s := &sse{w: w, rc: http.NewResponseController(w)}
	_ = s.rc.Flush()
	return s
}

// send writes one event; name may be empty.
func (s *sse) send(name string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if name != "" {
		fmt.Fprintf(s.w, "event: %s\n", name)
	}
	fmt.Fprintf(s.w, "data: %s\n\n", data)
	_ = s.rc.Flush()
}

func (s *sse) raw(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprint(s.w, text)
	_ = s.rc.Flush()
}

// keepalive writes a comment now and then so proxies keep the stream open
// while tools run; the returned func stops it.
func (s *sse) keepalive() func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(apiKeepalive)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				s.raw(": keepalive\n\n")
			}
		}
	}()
	return func() { close(done) }
}

// textStream turns a turn's events into the text a client sees: model text
// as it arrives, with a blank line between the text before a tool call and
// the text after it.
type textStream struct {
	emit     func(string)
	wrote    bool
	pendBrk  bool
	collated strings.Builder
}

func (t *textStream) on(e events.Event) {
	switch e.Kind {
	case events.Text:
		if e.Text == "" {
			return
		}
		if t.pendBrk && t.wrote {
			t.write("\n\n")
		}
		t.pendBrk = false
		t.write(e.Text)
	case events.ToolStart:
		t.pendBrk = true
	}
}

func (t *textStream) write(s string) {
	t.wrote = true
	t.collated.WriteString(s)
	t.emit(s)
}

// end sends the final reply when no text came through the bus (the turn ran
// in another process, or this request attached to a finished turn).
func (t *textStream) end(final string) {
	if !t.wrote && final != "" {
		t.write(final)
	}
}

func usageOf(u llm.Usage) (prompt, completion int64) {
	return u.Prompt(), u.OutputTokens
}

// apiID turns a turn job id into a response id with the given prefix.
func apiID(prefix, job string) string {
	return prefix + strings.TrimPrefix(job, "turn_")
}
