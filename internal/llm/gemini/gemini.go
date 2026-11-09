// Package gemini adapts the Google Gemini API (generateContent with
// server-sent events) to Habakkuk's provider interface. There is no official
// Go SDK dependency: the REST API is small and stable.
package gemini

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

const (
	providerType   = "gemini"
	DefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"
	// skipSignature is the documented placeholder for function calls whose
	// thought signature Gemini never produced (history from another model).
	skipSignature = "skip_thought_signature_validator"
	// localIDPrefix marks call ids Habakkuk made up for models that send none;
	// they are not sent back.
	localIDPrefix = "gemini_"
)

type Provider struct {
	BaseURL string       // default DefaultBaseURL
	HTTP    *http.Client // default http.DefaultClient
}

func (p *Provider) base() string {
	if p.BaseURL != "" {
		return strings.TrimRight(p.BaseURL, "/")
	}
	return DefaultBaseURL
}

func (p *Provider) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return http.DefaultClient
}

// Wire types: only the fields Habakkuk uses.

type part struct {
	Text             string            `json:"text,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
	InlineData       *blob             `json:"inlineData,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
}

type blob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type functionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type functionResponse struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

type functionDeclaration struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description,omitempty"`
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
}

type tool struct {
	FunctionDeclarations []functionDeclaration `json:"functionDeclarations,omitempty"`
}

type thinkingConfig struct {
	IncludeThoughts bool   `json:"includeThoughts,omitempty"`
	ThinkingBudget  *int   `json:"thinkingBudget,omitempty"`
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`
}

type generationConfig struct {
	MaxOutputTokens int             `json:"maxOutputTokens,omitempty"`
	ThinkingConfig  *thinkingConfig `json:"thinkingConfig,omitempty"`
}

type request struct {
	SystemInstruction *content         `json:"systemInstruction,omitempty"`
	Contents          []content        `json:"contents"`
	Tools             []tool           `json:"tools,omitempty"`
	GenerationConfig  generationConfig `json:"generationConfig"`
}

type usageMetadata struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount"`
}

type chunk struct {
	Candidates []struct {
		Content      content `json:"content"`
		FinishReason string  `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	UsageMetadata *usageMetadata `json:"usageMetadata"`
	Error         *apiError      `json:"error"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

func (p *Provider) Stream(ctx context.Context, apiKey string, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error) {
	body, err := json.Marshal(buildRequest(req))
	if err != nil {
		return nil, err
	}
	u := p.base() + "/models/" + url.PathEscape(req.Model) + ":streamGenerateContent?alt=sse"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	hreq.Header.Set("x-goog-api-key", apiKey)
	resp, err := p.client().Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, readError(resp)
	}
	acc := &accumulator{onEvent: onEvent}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 64<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		var c chunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			return nil, fmt.Errorf("gemini: bad stream chunk: %w", err)
		}
		if c.Error != nil {
			return nil, toAPIError(c.Error.Code, c.Error)
		}
		acc.add(c)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return acc.response(), nil
}

// buildRequest maps a neutral request to generateContent.
func buildRequest(req llm.Request) request {
	out := request{GenerationConfig: generationConfig{MaxOutputTokens: req.MaxTokens}}
	if req.System != "" {
		out.SystemInstruction = &content{Parts: []part{{Text: req.System}}}
	}
	if len(req.Tools) > 0 {
		var decls []functionDeclaration
		for _, t := range req.Tools {
			decls = append(decls, functionDeclaration{Name: t.Name, Description: t.Description, ParametersJSONSchema: t.Schema})
		}
		out.Tools = []tool{{FunctionDeclarations: decls}}
	}
	out.GenerationConfig.ThinkingConfig = thinking(req.Model, req.Effort)
	names := map[string]string{} // tool use id -> tool name, for responses
	for _, m := range req.Messages {
		c := toContent(req, m, names)
		if len(c.Parts) > 0 {
			out.Contents = append(out.Contents, c)
		}
	}
	return out
}

// thinking maps Habakkuk's effort to Gemini's: a budget in tokens for the 2.x
// models, a level for Gemini 3 and later. Thought summaries are always
// requested so the client can show them.
func thinking(model, effort string) *thinkingConfig {
	m := strings.ToLower(model)
	if !strings.HasPrefix(m, "gemini-") {
		return nil
	}
	tc := &thinkingConfig{IncludeThoughts: true}
	if strings.HasPrefix(m, "gemini-2") {
		budget := map[string]int{"low": 1024, "medium": 8192, "high": 24576, "xhigh": -1, "max": -1}
		if b, ok := budget[effort]; ok {
			tc.ThinkingBudget = &b
		}
		return tc
	}
	switch effort {
	case "low", "medium", "high":
		tc.ThinkingLevel = effort
	case "xhigh", "max":
		tc.ThinkingLevel = "high"
	}
	return tc
}

func toContent(req llm.Request, m llm.Message, names map[string]string) content {
	c := content{Role: "user"}
	if m.Role == llm.Assistant {
		c.Role = "model"
	}
	signedCall := false
	for _, b := range m.Blocks {
		own := b.Provider == providerType
		switch b.Type {
		case llm.Text:
			if b.Text != "" || (own && b.Signature != "") {
				p := part{Text: b.Text}
				if own {
					p.ThoughtSignature = b.Signature
				}
				c.Parts = append(c.Parts, p)
			}
		case llm.Thinking:
			if own {
				c.Parts = append(c.Parts, part{Text: b.Text, Thought: true, ThoughtSignature: b.Signature})
			}
		case llm.ToolUse:
			names[b.ToolUseID] = b.ToolName
			args := b.Input
			if len(args) == 0 || !json.Valid(args) {
				args = json.RawMessage("{}")
			}
			p := part{FunctionCall: &functionCall{ID: wireID(b.ToolUseID), Name: b.ToolName, Args: args}}
			if own && b.Signature != "" {
				p.ThoughtSignature = b.Signature
				signedCall = true
			} else if !signedCall {
				// Gemini 3 rejects a step whose first call has no signature;
				// calls written by another model get the documented bypass.
				p.ThoughtSignature = skipSignature
				signedCall = true
			}
			c.Parts = append(c.Parts, p)
		case llm.ToolResult:
			resp := map[string]any{"result": b.Text}
			if b.IsError {
				resp = map[string]any{"error": b.Text}
			}
			name := names[b.ToolUseID]
			if name == "" {
				name = "tool"
			}
			c.Parts = append(c.Parts, part{FunctionResponse: &functionResponse{ID: wireID(b.ToolUseID), Name: name, Response: resp}})
		case llm.Image, llm.Document:
			data, err := req.LoadMedia(b)
			if err != nil {
				c.Parts = append(c.Parts, part{Text: llm.MediaPlaceholder(b)})
				continue
			}
			c.Parts = append(c.Parts, part{InlineData: &blob{MimeType: b.MediaType, Data: data}})
		}
	}
	return c
}

func wireID(id string) string {
	if strings.HasPrefix(id, localIDPrefix) {
		return ""
	}
	return id
}

// accumulator folds stream chunks into one response.
type accumulator struct {
	onEvent func(llm.Event)
	blocks  []llm.Block
	finish  string
	blocked string
	usage   usageMetadata
	calls   int
}

func (a *accumulator) add(c chunk) {
	if c.UsageMetadata != nil {
		a.usage = *c.UsageMetadata
	}
	if c.PromptFeedback != nil && c.PromptFeedback.BlockReason != "" {
		a.blocked = c.PromptFeedback.BlockReason
	}
	if len(c.Candidates) == 0 {
		return
	}
	cand := c.Candidates[0]
	if cand.FinishReason != "" {
		a.finish = cand.FinishReason
	}
	for _, p := range cand.Content.Parts {
		switch {
		case p.FunctionCall != nil:
			a.calls++
			id := p.FunctionCall.ID
			if id == "" {
				id = fmt.Sprintf("%s%d", localIDPrefix, a.calls)
			}
			args := p.FunctionCall.Args
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			a.onEvent(llm.Event{Type: llm.EventToolCall, Text: p.FunctionCall.Name})
			a.blocks = append(a.blocks, llm.Block{Type: llm.ToolUse, ToolUseID: id, ToolName: p.FunctionCall.Name,
				Input: args, Signature: p.ThoughtSignature, Provider: providerType})
		case p.Thought:
			if p.Text != "" {
				a.onEvent(llm.Event{Type: llm.EventThinking, Text: p.Text})
			}
			a.appendText(llm.Thinking, p.Text, p.ThoughtSignature)
		case p.Text != "" || p.ThoughtSignature != "":
			if p.Text != "" {
				a.onEvent(llm.Event{Type: llm.EventText, Text: p.Text})
			}
			a.appendText(llm.Text, p.Text, p.ThoughtSignature)
		}
	}
}

// appendText merges streamed text into the previous block of the same kind
// unless that block is already sealed by a signature. A signature (which may
// arrive on an empty part) seals the block it lands on.
func (a *accumulator) appendText(kind llm.BlockType, text, sig string) {
	if n := len(a.blocks); n > 0 {
		last := &a.blocks[n-1]
		if last.Type == kind && last.Signature == "" {
			last.Text += text
			last.Signature = sig
			return
		}
	}
	a.blocks = append(a.blocks, llm.Block{Type: kind, Text: text, Signature: sig, Provider: providerType})
}

func (a *accumulator) response() *llm.Response {
	u := a.usage
	out := &llm.Response{
		Message: llm.Message{Role: llm.Assistant, Blocks: a.blocks},
		Usage: llm.Usage{
			// promptTokenCount includes the cached part; thoughts are billed
			// as output.
			InputTokens:     u.PromptTokenCount + u.ToolUsePromptTokenCount - u.CachedContentTokenCount,
			OutputTokens:    u.CandidatesTokenCount + u.ThoughtsTokenCount,
			CacheReadTokens: u.CachedContentTokenCount,
		},
	}
	switch a.finish {
	case "STOP", "":
		out.StopReason = llm.StopEnd
	case "MAX_TOKENS":
		out.StopReason = llm.StopMaxTokens
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "LANGUAGE":
		out.StopReason = llm.StopRefusal
	default:
		out.StopReason = llm.StopOther
	}
	if a.blocked != "" {
		out.StopReason = llm.StopRefusal
		if len(out.Message.Blocks) == 0 {
			out.Message.Blocks = []llm.Block{{Type: llm.Text, Text: "(the prompt was blocked: " + a.blocked + ")"}}
		}
	}
	if a.calls > 0 && out.StopReason != llm.StopMaxTokens {
		out.StopReason = llm.StopToolUse
	}
	return out
}

func readError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// Errors come as one JSON object, or as a JSON array of one.
	var e struct {
		Error *apiError `json:"error"`
	}
	if err := json.Unmarshal(data, &e); err != nil || e.Error == nil {
		var list []struct {
			Error *apiError `json:"error"`
		}
		if json.Unmarshal(data, &list) == nil && len(list) > 0 {
			e.Error = list[0].Error
		}
	}
	if e.Error == nil {
		e.Error = &apiError{Message: strings.TrimSpace(string(data))}
	}
	return toAPIError(resp.StatusCode, e.Error)
}

// toAPIError maps Gemini's status codes onto the router's classes where the
// HTTP status alone would mislead.
func toAPIError(status int, e *apiError) error {
	msg := "gemini: " + e.Message
	if e.Status != "" {
		msg = "gemini: " + e.Status + ": " + e.Message
	}
	lower := strings.ToLower(e.Message)
	switch {
	case e.Status == "FAILED_PRECONDITION" && strings.Contains(lower, "billing"),
		status == 402:
		status = http.StatusPaymentRequired
	case strings.Contains(lower, "exceeds the maximum number of tokens"), strings.Contains(lower, "input token count"):
		status = http.StatusRequestEntityTooLarge
	}
	if status == 0 {
		status = e.Code
	}
	return &llm.APIError{Status: status, Message: msg, Err: errors.New(e.Message)}
}

// ListModels returns the models that support generateContent.
func (p *Provider) ListModels(ctx context.Context, apiKey string) ([]string, error) {
	var out []string
	token := ""
	for page := 0; page < 20; page++ {
		u := p.base() + "/models?pageSize=1000"
		if token != "" {
			u += "&pageToken=" + url.QueryEscape(token)
		}
		hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		hreq.Header.Set("x-goog-api-key", apiKey)
		resp, err := p.client().Do(hreq)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			err := readError(resp)
			resp.Body.Close()
			return nil, err
		}
		var list struct {
			Models []struct {
				Name    string   `json:"name"`
				Methods []string `json:"supportedGenerationMethods"`
			} `json:"models"`
			NextPageToken string `json:"nextPageToken"`
		}
		err = json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, m := range list.Models {
			for _, meth := range m.Methods {
				if meth == "generateContent" || meth == "streamGenerateContent" {
					out = append(out, strings.TrimPrefix(m.Name, "models/"))
					break
				}
			}
		}
		if token = list.NextPageToken; token == "" {
			break
		}
	}
	return out, nil
}
