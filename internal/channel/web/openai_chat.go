package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

type chatCompletionRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Stream        bool          `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	User      string          `json:"user"`
	N         *int            `json:"n"`
	Tools     json.RawMessage `json:"tools"`
	Functions json.RawMessage `json:"functions"`
}

type chatMessage struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ToolCalls json.RawMessage `json:"tool_calls"`
}

type chatUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type chatChoice struct {
	Index        int        `json:"index"`
	Message      *chatReply `json:"message,omitempty"`
	Delta        *chatReply `json:"delta,omitempty"`
	FinishReason *string    `json:"finish_reason"`
	Logprobs     *struct{}  `json:"logprobs"`
}

type chatReply struct {
	Role    string  `json:"role,omitempty"`
	Content *string `json:"content,omitempty"`
	Refusal *string `json:"refusal,omitempty"`
}

type chatCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

// chatCompletions serves POST /v1/chat/completions.
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req chatCompletionRequest
	if !readAPIJSON(w, r, &req) {
		return
	}
	c, aerr := s.chatConvo(r, &req)
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
	model := modelPrefix + t.agent
	id := apiID("chatcmpl-", t.job)
	if req.Stream {
		s.streamChat(w, r, t, id, model, req.StreamOptions != nil && req.StreamOptions.IncludeUsage)
		return
	}
	final, aerr := s.finish(r.Context(), t, nil)
	if aerr != nil {
		if aerr.status != 499 {
			writeAPIError(w, aerr)
		}
		return
	}
	prompt, completion := usageOf(s.Gateway.Runner.TurnUsage(r.Context(), t.session, t.job))
	stop := "stop"
	w.Header().Set(sessionHeader, t.session)
	writeJSON(w, http.StatusOK, chatCompletion{
		ID: id, Object: "chat.completion", Created: t.created.Unix(), Model: model,
		Choices: []chatChoice{{Message: &chatReply{Role: "assistant", Content: &final}, FinishReason: &stop}},
		Usage:   &chatUsage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion},
	})
}

func (s *Server) chatConvo(r *http.Request, req *chatCompletionRequest) (convo, *apiError) {
	c := convo{endpoint: "chat", key: req.User, session: r.Header.Get(sessionHeader), idem: r.Header.Get("Idempotency-Key")}
	if e := noTools("tools", req.Tools); e != nil {
		return c, e
	}
	if e := noTools("functions", req.Functions); e != nil {
		return c, e
	}
	if req.N != nil && *req.N != 1 {
		return c, badRequest("n", "n must be 1")
	}
	name, e := s.agentFor(req.Model)
	if e != nil {
		return c, e
	}
	c.agent = name
	if len(req.Messages) == 0 {
		return c, badRequest("messages", "messages is empty")
	}
	msgs := make([]message, 0, len(req.Messages))
	for i, m := range req.Messages {
		role, ok := validRole(m.Role)
		if !ok {
			return c, badRequest("messages", "message %d: role %q is not supported (client-side tools are not); use system, developer, user or assistant", i, m.Role)
		}
		if e := noTools("messages", m.ToolCalls); e != nil {
			return c, e
		}
		text, e := contentText(m.Content, "messages")
		if e != nil {
			return c, e
		}
		msgs = append(msgs, message{role: role, text: text})
	}
	c.history, c.text, e = conversation(msgs, "")
	return c, e
}

func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, t *apiTurn, id, model string, includeUsage bool) {
	out := startSSE(w, t.session)
	stopKeepalive := out.keepalive()
	defer stopKeepalive()
	chunk := func(delta *chatReply, finish *string) chatCompletion {
		return chatCompletion{ID: id, Object: "chat.completion.chunk", Created: t.created.Unix(), Model: model,
			Choices: []chatChoice{{Delta: delta, FinishReason: finish}}}
	}
	empty := ""
	out.send("", chunk(&chatReply{Role: "assistant", Content: &empty}, nil))
	ts := &textStream{emit: func(text string) { out.send("", chunk(&chatReply{Content: &text}, nil)) }}
	final, aerr := s.finish(r.Context(), t, ts.on)
	if aerr != nil {
		if aerr.status != 499 {
			out.send("", map[string]any{"error": aerr.body()})
		}
		return
	}
	ts.end(final)
	stop := "stop"
	out.send("", chunk(&chatReply{}, &stop))
	if includeUsage {
		prompt, completion := usageOf(s.Gateway.Runner.TurnUsage(r.Context(), t.session, t.job))
		out.send("", chatCompletion{ID: id, Object: "chat.completion.chunk", Created: t.created.Unix(), Model: model,
			Choices: []chatChoice{}, Usage: &chatUsage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion}})
	}
	out.raw("data: [DONE]\n\n")
}

// readAPIJSON reads a request body, answering errors in OpenAI's shape.
func readAPIJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body, ok := readBody(w, r)
	if !ok {
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		msg := err.Error()
		if i := strings.Index(msg, "Go struct field"); i > 0 {
			msg = strings.TrimSpace(msg[:i])
		}
		writeAPIError(w, badRequest("", "bad JSON: %s", msg))
		return false
	}
	return true
}
