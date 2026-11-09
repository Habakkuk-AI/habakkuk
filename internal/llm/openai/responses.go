package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// responsesType marks reasoning blocks written by the Responses API; they
// carry encrypted reasoning and are replayed only to it.
const responsesType = "responses"

// ChatGPTBaseURL serves the Responses API to ChatGPT subscription logins.
const ChatGPTBaseURL = "https://chatgpt.com/backend-api/codex"

// Responses adapts the OpenAI Responses API. Requests are stateless
// (store=false): reasoning is replayed from its encrypted content, so the
// transcript stays the only source of truth.
type Responses struct {
	BaseURL string
	// ChatGPT switches to the subscription backend's rules: the account id
	// header, no output token limit, and instructions always present.
	ChatGPT   bool
	AccountID func() string // ChatGPT account id for the chatgpt-account-id header
	UserAgent string
}

func (p *Responses) client(ctx context.Context, apiKey string) sdk.Client {
	opts := []option.RequestOption{option.WithAPIKey(apiKey), option.WithMaxRetries(0)}
	base := p.BaseURL
	if base == "" && p.ChatGPT {
		base = ChatGPTBaseURL
	}
	if base != "" {
		opts = append(opts, option.WithBaseURL(base))
	}
	if p.UserAgent != "" {
		opts = append(opts, option.WithHeader("User-Agent", p.UserAgent))
	}
	if p.ChatGPT {
		opts = append(opts, option.WithHeader("originator", "habakkuk"), option.WithHeader("OpenAI-Beta", "responses=experimental"))
		if p.AccountID != nil {
			if id := p.AccountID(); id != "" {
				opts = append(opts, option.WithHeader("chatgpt-account-id", id))
			}
		}
		if s := llm.CallerFrom(ctx).Session; s != "" {
			opts = append(opts, option.WithHeader("session-id", s))
		}
	}
	return sdk.NewClient(opts...)
}

func (p *Responses) Stream(ctx context.Context, apiKey string, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error) {
	client := p.client(ctx, apiKey)
	params, err := p.buildParams(ctx, req)
	if err != nil {
		return nil, err
	}
	stream := client.Responses.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()
	var final *responses.Response
	var items []outputItem
	for stream.Next() {
		ev := stream.Current()
		switch ev.Type {
		case "response.output_text.delta":
			onEvent(llm.Event{Type: llm.EventText, Text: ev.Delta})
		case "response.reasoning_summary_text.delta":
			onEvent(llm.Event{Type: llm.EventThinking, Text: ev.Delta})
		case "response.output_item.added":
			if ev.Item.Type == "function_call" {
				onEvent(llm.Event{Type: llm.EventToolCall, Text: ev.Item.Name})
			}
		case "response.output_item.done":
			var it outputItem
			if err := json.Unmarshal([]byte(ev.Item.RawJSON()), &it); err == nil {
				items = append(items, it)
			}
		case "response.completed", "response.incomplete":
			r := ev.Response
			final = &r
		case "response.failed":
			return nil, responseError(string(ev.Response.Error.Code), ev.Response.Error.Message)
		case "error":
			return nil, responseError(ev.Code, ev.Message)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, wrapErr(err)
	}
	if final == nil {
		return nil, &llm.APIError{Status: 502, Message: "responses: stream ended without a completed response"}
	}
	return toResponsesResult(final, items), nil
}

func (p *Responses) buildParams(ctx context.Context, req llm.Request) (responses.ResponseNewParams, error) {
	params := responses.ResponseNewParams{
		Model:             shared.ResponsesModel(req.Model),
		Store:             param.NewOpt(false),
		ParallelToolCalls: param.NewOpt(true),
		Include:           []responses.ResponseIncludable{responses.ResponseIncludableReasoningEncryptedContent},
	}
	instructions := req.System
	if instructions == "" && p.ChatGPT {
		instructions = "You are a helpful assistant."
	}
	if instructions != "" {
		params.Instructions = param.NewOpt(instructions)
	}
	if req.MaxTokens > 0 && !p.ChatGPT {
		params.MaxOutputTokens = param.NewOpt(int64(req.MaxTokens))
	}
	if s := llm.CallerFrom(ctx).Session; s != "" {
		params.PromptCacheKey = param.NewOpt(s)
	}
	if effort := reasoningEffort(req.Effort); effort != "" {
		params.Reasoning = shared.ReasoningParam{Effort: effort, Summary: shared.ReasoningSummaryAuto}
	}
	for _, t := range req.Tools {
		var schema map[string]any
		if err := json.Unmarshal(t.Schema, &schema); err != nil {
			return params, fmt.Errorf("tool %s schema: %w", t.Name, err)
		}
		params.Tools = append(params.Tools, responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{
			Name: t.Name, Description: param.NewOpt(t.Description), Parameters: schema, Strict: param.NewOpt(false),
		}})
	}
	var input responses.ResponseInputParam
	for _, m := range req.Messages {
		input = append(input, toInputItems(req, m)...)
	}
	params.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: input}
	return params, nil
}

func reasoningEffort(e string) shared.ReasoningEffort {
	switch e {
	case "low", "medium", "high", "xhigh":
		return shared.ReasoningEffort(e)
	case "max":
		return shared.ReasoningEffortXhigh
	}
	return ""
}

func toInputItems(req llm.Request, m llm.Message) []responses.ResponseInputItemUnionParam {
	var out []responses.ResponseInputItemUnionParam
	if m.Role == llm.Assistant {
		for _, b := range m.Blocks {
			switch b.Type {
			case llm.Text:
				if b.Text != "" {
					out = append(out, responses.ResponseInputItemParamOfMessage(b.Text, responses.EasyInputMessageRoleAssistant))
				}
			case llm.Thinking:
				// Encrypted reasoning only means something to this API.
				if b.Provider == responsesType && b.Signature != "" {
					r := responses.ResponseReasoningItemParam{ID: b.Data, EncryptedContent: param.NewOpt(b.Signature),
						Summary: []responses.ResponseReasoningItemSummaryParam{}}
					if b.Text != "" {
						r.Summary = append(r.Summary, responses.ResponseReasoningItemSummaryParam{Text: b.Text})
					}
					out = append(out, responses.ResponseInputItemUnionParam{OfReasoning: &r})
				}
			case llm.ToolUse:
				args := string(b.Input)
				if args == "" || !json.Valid(b.Input) {
					args = "{}"
				}
				out = append(out, responses.ResponseInputItemParamOfFunctionCall(args, b.ToolUseID, b.ToolName))
			}
		}
		return out
	}
	var parts responses.ResponseInputMessageContentListParam
	for _, b := range m.Blocks {
		switch b.Type {
		case llm.ToolResult:
			text := b.Text
			if b.IsError {
				text = "ERROR: " + text
			}
			item := responses.ResponseInputItemParamOfFunctionCallOutput(text)
			item.OfFunctionCallOutput.CallID = param.NewOpt(b.ToolUseID)
			out = append(out, item)
		case llm.Text:
			if b.Text != "" {
				parts = append(parts, responses.ResponseInputContentParamOfInputText(b.Text))
			}
		case llm.Image, llm.Document:
			data, err := req.LoadMedia(b)
			switch {
			case err != nil:
				parts = append(parts, responses.ResponseInputContentParamOfInputText(llm.MediaPlaceholder(b)))
			case b.Type == llm.Image:
				parts = append(parts, responses.ResponseInputContentUnionParam{OfInputImage: &responses.ResponseInputImageParam{
					Detail: responses.ResponseInputImageDetailAuto, ImageURL: param.NewOpt("data:" + b.MediaType + ";base64," + data)}})
			default:
				parts = append(parts, responses.ResponseInputContentUnionParam{OfInputFile: &responses.ResponseInputFileParam{
					FileData: param.NewOpt("data:" + b.MediaType + ";base64," + data), Filename: param.NewOpt(b.Name)}})
			}
		}
	}
	if len(parts) > 0 {
		out = append(out, responses.ResponseInputItemParamOfMessage(parts, responses.EasyInputMessageRoleUser))
	}
	return out
}

// outputItem is the part of a Responses output item Habakkuk keeps.
type outputItem struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	CallID  string `json:"call_id"`
	Name    string `json:"name"`
	Args    string `json:"arguments"`
	Content []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
	EncryptedContent string `json:"encrypted_content"`
}

func toResponsesResult(r *responses.Response, items []outputItem) *llm.Response {
	cached := r.Usage.InputTokensDetails.CachedTokens
	out := &llm.Response{
		Message: llm.Message{Role: llm.Assistant},
		Usage: llm.Usage{
			InputTokens:     r.Usage.InputTokens - cached,
			OutputTokens:    r.Usage.OutputTokens,
			CacheReadTokens: cached,
		},
		StopReason: llm.StopEnd,
	}
	refused := false
	for _, it := range items {
		switch it.Type {
		case "reasoning":
			var sum []string
			for _, s := range it.Summary {
				sum = append(sum, s.Text)
			}
			out.Message.Blocks = append(out.Message.Blocks, llm.Block{Type: llm.Thinking, Text: strings.Join(sum, "\n\n"),
				Signature: it.EncryptedContent, Data: it.ID, Provider: responsesType})
		case "message":
			for _, c := range it.Content {
				switch c.Type {
				case "output_text":
					out.Message.Blocks = append(out.Message.Blocks, llm.Block{Type: llm.Text, Text: c.Text})
				case "refusal":
					refused = true
					out.Message.Blocks = append(out.Message.Blocks, llm.Block{Type: llm.Text, Text: c.Refusal})
				}
			}
		case "function_call":
			args := json.RawMessage(it.Args)
			if len(args) == 0 || !json.Valid(args) {
				args = json.RawMessage("{}")
			}
			out.Message.Blocks = append(out.Message.Blocks, llm.Block{Type: llm.ToolUse, ToolUseID: it.CallID, ToolName: it.Name, Input: args})
		}
	}
	switch {
	case r.Status == "incomplete" && r.IncompleteDetails.Reason == "max_output_tokens":
		out.StopReason = llm.StopMaxTokens
	case r.Status == "incomplete" && r.IncompleteDetails.Reason == "content_filter", refused:
		out.StopReason = llm.StopRefusal
	case len(out.Message.ToolUses()) > 0:
		out.StopReason = llm.StopToolUse
	}
	return out
}

// responseError maps an error reported inside the stream to the router's
// classes.
func responseError(code, message string) error {
	status := 400
	switch {
	case strings.Contains(code, "rate_limit"), strings.Contains(code, "usage_limit"):
		status = 429
	case code == "insufficient_quota":
		status = 402
	case code == "context_length_exceeded":
		status = 413
	case code == "server_error", code == "":
		status = 500
	case strings.Contains(code, "overloaded"), code == "server_is_overloaded":
		status = 529
	}
	return &llm.APIError{Status: status, Message: fmt.Sprintf("responses: %s: %s", code, message)}
}

// ListModels lists the models the account can use. The subscription
// backend has its own listing.
func (p *Responses) ListModels(ctx context.Context, apiKey string) ([]string, error) {
	client := p.client(ctx, apiKey)
	if !p.ChatGPT {
		iter := client.Models.ListAutoPaging(ctx)
		var out []string
		for iter.Next() {
			out = append(out, iter.Current().ID)
		}
		if err := iter.Err(); err != nil {
			return nil, wrapErr(err)
		}
		return out, nil
	}
	var res struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if err := client.Get(ctx, "models?client_version=1.0.0", nil, &res); err != nil {
		return nil, wrapErr(err)
	}
	var out []string
	for _, m := range res.Models {
		if m.Visibility != "hide" && m.Visibility != "hidden" {
			out = append(out, m.Slug)
		}
	}
	return out, nil
}
