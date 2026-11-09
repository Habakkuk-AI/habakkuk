package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
)

type responsesRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       string          `json:"instructions"`
	PreviousResponseID string          `json:"previous_response_id"`
	Stream             bool            `json:"stream"`
	User               string          `json:"user"`
	Background         bool            `json:"background"`
	Tools              json.RawMessage `json:"tools"`
	Conversation       json.RawMessage `json:"conversation"`
}

type responseObject struct {
	ID                 string          `json:"id"`
	Object             string          `json:"object"`
	CreatedAt          int64           `json:"created_at"`
	Status             string          `json:"status"` // in_progress | completed | failed
	Model              string          `json:"model"`
	Output             []responseItem  `json:"output"`
	Error              map[string]any  `json:"error"`
	IncompleteDetails  any             `json:"incomplete_details"`
	Instructions       any             `json:"instructions"`
	PreviousResponseID *string         `json:"previous_response_id"`
	ParallelToolCalls  bool            `json:"parallel_tool_calls"`
	ToolChoice         string          `json:"tool_choice"`
	Tools              []any           `json:"tools"`
	Metadata           map[string]any  `json:"metadata"`
	Usage              *responsesUsage `json:"usage,omitempty"`
	Text               map[string]any  `json:"text"`
	Store              bool            `json:"store"`
	Truncation         string          `json:"truncation"`
	User               *string         `json:"user,omitempty"`
	CompletedAt        *int64          `json:"completed_at,omitempty"`
	Reasoning          map[string]any  `json:"reasoning,omitempty"`
	ServiceTier        string          `json:"service_tier,omitempty"`
}

type responseItem struct {
	Type    string         `json:"type"` // message
	ID      string         `json:"id"`
	Status  string         `json:"status"`
	Role    string         `json:"role"`
	Content []responsePart `json:"content"`
}

type responsePart struct {
	Type        string `json:"type"` // output_text
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type responsesUsage struct {
	InputTokens         int64            `json:"input_tokens"`
	InputTokensDetails  map[string]int64 `json:"input_tokens_details"`
	OutputTokens        int64            `json:"output_tokens"`
	OutputTokensDetails map[string]int64 `json:"output_tokens_details"`
	TotalTokens         int64            `json:"total_tokens"`
}

// responses serves POST /v1/responses: minimal, but in the shape OpenAI
// clients expect. A response id names the turn, so previous_response_id
// continues that turn's session.
func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	var req responsesRequest
	if !readAPIJSON(w, r, &req) {
		return
	}
	c, aerr := s.responsesConvo(r, &req)
	if aerr != nil {
		writeAPIError(w, aerr)
		return
	}
	t, aerr := s.begin(context.WithoutCancel(r.Context()), c, req.Stream)
	if aerr != nil {
		writeAPIError(w, aerr)
		return
	}
	defer t.close()
	rb := &responseBuilder{
		id: apiID("resp_", t.job), item: apiID("msg_", t.job), model: modelPrefix + t.agent,
		created: t.created.Unix(), instructions: req.Instructions, prev: req.PreviousResponseID, user: req.User,
	}
	if req.Stream {
		s.streamResponse(w, r, t, rb)
		return
	}
	final, aerr := s.finish(r.Context(), t, nil)
	if aerr != nil {
		if aerr.status != 499 {
			writeAPIError(w, aerr)
		}
		return
	}
	w.Header().Set(sessionHeader, t.session)
	writeJSON(w, http.StatusOK, rb.completed(final, s.responsesUsage(r.Context(), t)))
}

func (s *Server) responsesUsage(ctx context.Context, t *apiTurn) *responsesUsage {
	u := s.Gateway.Runner.TurnUsage(ctx, t.session, t.job)
	in, out := usageOf(u)
	return &responsesUsage{
		InputTokens: in, OutputTokens: out, TotalTokens: in + out,
		InputTokensDetails:  map[string]int64{"cached_tokens": u.CacheReadTokens},
		OutputTokensDetails: map[string]int64{"reasoning_tokens": 0},
	}
}

func (s *Server) responsesConvo(r *http.Request, req *responsesRequest) (convo, *apiError) {
	c := convo{endpoint: "responses", key: req.User, session: r.Header.Get(sessionHeader), idem: r.Header.Get("Idempotency-Key")}
	if e := noTools("tools", req.Tools); e != nil {
		return c, e
	}
	switch strings.TrimSpace(string(req.Conversation)) {
	case "", "null":
	default:
		return c, badRequest("conversation", "conversation is not supported; use previous_response_id or the %s header", sessionHeader)
	}
	if req.Background {
		return c, badRequest("background", "background responses are not supported; the turn is durable anyway, so retry with the same Idempotency-Key to pick it up")
	}
	name, e := s.agentFor(req.Model)
	if e != nil {
		return c, e
	}
	c.agent = name
	if c.session == "" && req.PreviousResponseID != "" {
		sid, e := s.responseSession(r.Context(), req.PreviousResponseID)
		if e != nil {
			return c, e
		}
		c.session = sid
	}
	msgs, e := responsesInput(req.Input)
	if e != nil {
		return c, e
	}
	c.history, c.text, e = conversation(msgs, req.Instructions)
	return c, e
}

// responseSession finds the session of an earlier response.
func (s *Server) responseSession(ctx context.Context, id string) (string, *apiError) {
	notFound := &apiError{status: http.StatusNotFound, typ: "invalid_request_error", param: "previous_response_id",
		code: "previous_response_not_found", msg: "previous response " + id + " not found"}
	rest, ok := strings.CutPrefix(id, "resp_")
	if !ok || rest == "" {
		return "", notFound
	}
	j, err := s.Gateway.Queue.Get(ctx, "turn_"+rest)
	if err != nil || j == nil {
		return "", notFound
	}
	var p agent.TurnPayload
	if j.Decode(&p) != nil || p.Session == "" {
		return "", notFound
	}
	return p.Session, nil
}

// responsesInput reads "input": a string, or a list of message items.
func responsesInput(raw json.RawMessage) ([]message, *apiError) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, badRequest("input", "input is required")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []message{{role: "user", text: s}}, nil
	}
	var items []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, badRequest("input", "input must be a string or a list of messages")
	}
	msgs := make([]message, 0, len(items))
	for i, it := range items {
		if it.Type != "" && it.Type != "message" {
			return nil, badRequest("input", "input item %d: type %q is not supported (client-side tools are not); send messages", i, it.Type)
		}
		role, ok := validRole(it.Role)
		if !ok {
			return nil, badRequest("input", "input item %d: role %q is not supported", i, it.Role)
		}
		text, e := contentText(it.Content, "input")
		if e != nil {
			return nil, e
		}
		msgs = append(msgs, message{role: role, text: text})
	}
	return msgs, nil
}

// responseBuilder makes the response object and its stream events.
type responseBuilder struct {
	id, item, model    string
	created            int64
	instructions, prev string
	user               string
	seq                int
}

func (b *responseBuilder) object(status string) responseObject {
	o := responseObject{
		ID: b.id, Object: "response", CreatedAt: b.created, Status: status, Model: b.model, Output: []responseItem{},
		ToolChoice: "auto", Tools: []any{}, Metadata: map[string]any{}, Store: true, Truncation: "disabled",
		Text: map[string]any{"format": map[string]any{"type": "text"}},
	}
	if b.instructions != "" {
		o.Instructions = b.instructions
	}
	if b.prev != "" {
		o.PreviousResponseID = &b.prev
	}
	if b.user != "" {
		o.User = &b.user
	}
	return o
}

func (b *responseBuilder) message(status, text string) responseItem {
	content := []responsePart{}
	if status == "completed" {
		content = []responsePart{{Type: "output_text", Text: text, Annotations: []any{}}}
	}
	return responseItem{Type: "message", ID: b.item, Status: status, Role: "assistant", Content: content}
}

func (b *responseBuilder) completed(text string, usage *responsesUsage) responseObject {
	o := b.object("completed")
	o.Output = []responseItem{b.message("completed", text)}
	o.Usage = usage
	now := time.Now().Unix()
	o.CompletedAt = &now
	return o
}

// event is one stream event: its type and sequence number, plus fields.
func (b *responseBuilder) event(typ string, fields map[string]any) map[string]any {
	fields["type"] = typ
	fields["sequence_number"] = b.seq
	b.seq++
	return fields
}

func (s *Server) streamResponse(w http.ResponseWriter, r *http.Request, t *apiTurn, b *responseBuilder) {
	out := startSSE(w, t.session)
	stopKeepalive := out.keepalive()
	defer stopKeepalive()
	send := func(typ string, fields map[string]any) { out.send(typ, b.event(typ, fields)) }

	send("response.created", map[string]any{"response": b.object("in_progress")})
	send("response.in_progress", map[string]any{"response": b.object("in_progress")})
	send("response.output_item.added", map[string]any{"output_index": 0, "item": b.message("in_progress", "")})
	send("response.content_part.added", map[string]any{"item_id": b.item, "output_index": 0, "content_index": 0,
		"part": responsePart{Type: "output_text", Text: "", Annotations: []any{}}})
	ts := &textStream{emit: func(text string) {
		send("response.output_text.delta", map[string]any{"item_id": b.item, "output_index": 0, "content_index": 0,
			"delta": text, "logprobs": []any{}})
	}}
	final, aerr := s.finish(r.Context(), t, ts.on)
	if aerr != nil {
		if aerr.status == 499 {
			return
		}
		o := b.object("failed")
		o.Error = map[string]any{"code": "server_error", "message": aerr.msg}
		send("response.failed", map[string]any{"response": o})
		return
	}
	ts.end(final)
	text := ts.collated.String()
	part := responsePart{Type: "output_text", Text: text, Annotations: []any{}}
	send("response.output_text.done", map[string]any{"item_id": b.item, "output_index": 0, "content_index": 0, "text": text, "logprobs": []any{}})
	send("response.content_part.done", map[string]any{"item_id": b.item, "output_index": 0, "content_index": 0, "part": part})
	send("response.output_item.done", map[string]any{"output_index": 0, "item": b.message("completed", text)})
	send("response.completed", map[string]any{"response": b.completed(text, s.responsesUsage(r.Context(), t))})
}
