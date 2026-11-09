// Package openai adapts any OpenAI-compatible Chat Completions API (OpenAI,
// OpenRouter, DeepSeek, local servers) to Habakkuk's provider interface.
package openai

import (
	"context"
	"encoding/json"
	"errors"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

type Provider struct {
	BaseURL string
	// Headers, when set, adds headers to each request (a gateway's
	// identification headers, or ones that depend on the request).
	Headers func(req llm.Request) map[string]string
}

func (p *Provider) newClient(apiKey string, req llm.Request) sdk.Client {
	opts := []option.RequestOption{option.WithAPIKey(apiKey), option.WithMaxRetries(0)}
	if p.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(p.BaseURL))
	}
	if p.Headers != nil {
		for k, v := range p.Headers(req) {
			opts = append(opts, option.WithHeader(k, v))
		}
	}
	return sdk.NewClient(opts...)
}

// ListModels lists the models the endpoint serves (GET /models).
func (p *Provider) ListModels(ctx context.Context, apiKey string) ([]string, error) {
	client := p.newClient(apiKey, llm.Request{})
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

func (p *Provider) Stream(ctx context.Context, apiKey string, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error) {
	client := p.newClient(apiKey, req)

	params := buildParams(req)
	stream := client.Chat.Completions.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()
	acc := sdk.ChatCompletionAccumulator{}
	seenTools := map[int64]bool{}
	for stream.Next() {
		chunk := stream.Current()
		acc.AddChunk(chunk)
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				onEvent(llm.Event{Type: llm.EventText, Text: c.Delta.Content})
			}
			for _, tc := range c.Delta.ToolCalls {
				if tc.Function.Name != "" && !seenTools[tc.Index] {
					seenTools[tc.Index] = true
					onEvent(llm.Event{Type: llm.EventToolCall, Text: tc.Function.Name})
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return nil, wrapErr(err)
	}
	return toResponse(&acc), nil
}

func buildParams(req llm.Request) sdk.ChatCompletionNewParams {
	params := sdk.ChatCompletionNewParams{
		Model:               shared.ChatModel(req.Model),
		MaxCompletionTokens: sdk.Int(int64(req.MaxTokens)),
		StreamOptions:       sdk.ChatCompletionStreamOptionsParam{IncludeUsage: sdk.Bool(true)},
	}
	switch req.Effort {
	case "low", "medium", "high":
		params.ReasoningEffort = shared.ReasoningEffort(req.Effort)
	case "xhigh", "max":
		params.ReasoningEffort = shared.ReasoningEffortHigh
	}
	if req.System != "" {
		params.Messages = append(params.Messages, sdk.SystemMessage(req.System))
	}
	for _, t := range req.Tools {
		var schema map[string]any
		_ = json.Unmarshal(t.Schema, &schema)
		params.Tools = append(params.Tools, sdk.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        t.Name,
			Description: sdk.String(t.Description),
			Parameters:  shared.FunctionParameters(schema),
		}))
	}
	for _, m := range req.Messages {
		params.Messages = append(params.Messages, toParams(req, m)...)
	}
	return params
}

// toParams maps one neutral message to Chat Completions messages. Tool
// results become separate "tool" role messages; reasoning blocks from other
// providers are dropped.
func toParams(req llm.Request, m llm.Message) []sdk.ChatCompletionMessageParamUnion {
	var out []sdk.ChatCompletionMessageParamUnion
	if m.Role == llm.Assistant {
		a := sdk.ChatCompletionAssistantMessageParam{}
		if text := m.Text(); text != "" {
			a.Content.OfString = sdk.String(text)
		}
		for _, b := range m.ToolUses() {
			args := string(b.Input)
			if args == "" || !json.Valid(b.Input) {
				args = "{}"
			}
			a.ToolCalls = append(a.ToolCalls, sdk.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &sdk.ChatCompletionMessageFunctionToolCallParam{
					ID:       b.ToolUseID,
					Function: sdk.ChatCompletionMessageFunctionToolCallFunctionParam{Name: b.ToolName, Arguments: args},
				},
			})
		}
		if a.Content.OfString.Valid() || len(a.ToolCalls) > 0 {
			out = append(out, sdk.ChatCompletionMessageParamUnion{OfAssistant: &a})
		}
		return out
	}
	var parts []sdk.ChatCompletionContentPartUnionParam
	for _, b := range m.Blocks {
		switch b.Type {
		case llm.ToolResult:
			content := b.Text
			if b.IsError {
				content = "ERROR: " + content
			}
			out = append(out, sdk.ToolMessage(content, b.ToolUseID))
		case llm.Text:
			if b.Text != "" {
				parts = append(parts, sdk.TextContentPart(b.Text))
			}
		case llm.Image, llm.Document:
			data, err := req.LoadMedia(b)
			switch {
			case err != nil:
				parts = append(parts, sdk.TextContentPart(llm.MediaPlaceholder(b)))
			case b.Type == llm.Image:
				parts = append(parts, sdk.ImageContentPart(sdk.ChatCompletionContentPartImageImageURLParam{
					URL: "data:" + b.MediaType + ";base64," + data}))
			default:
				parts = append(parts, sdk.FileContentPart(sdk.ChatCompletionContentPartFileFileParam{
					FileData: sdk.String("data:" + b.MediaType + ";base64," + data), Filename: sdk.String(b.Name)}))
			}
		}
	}
	switch {
	case len(parts) == 1 && parts[0].OfText != nil:
		out = append(out, sdk.UserMessage(parts[0].OfText.Text))
	case len(parts) > 0:
		out = append(out, sdk.UserMessage(parts))
	}
	return out
}

func toResponse(acc *sdk.ChatCompletionAccumulator) *llm.Response {
	out := &llm.Response{
		Message: llm.Message{Role: llm.Assistant},
		// Chat Completions counts cached tokens inside prompt_tokens; the
		// neutral Usage keeps them apart.
		Usage: llm.Usage{
			InputTokens:     acc.Usage.PromptTokens - acc.Usage.PromptTokensDetails.CachedTokens,
			OutputTokens:    acc.Usage.CompletionTokens,
			CacheReadTokens: acc.Usage.PromptTokensDetails.CachedTokens,
		},
		StopReason: llm.StopOther,
	}
	if len(acc.Choices) == 0 {
		return out
	}
	c := acc.Choices[0]
	if c.Message.Content != "" {
		out.Message.Blocks = append(out.Message.Blocks, llm.Block{Type: llm.Text, Text: c.Message.Content})
	}
	for _, tc := range c.Message.ToolCalls {
		out.Message.Blocks = append(out.Message.Blocks, llm.Block{
			Type: llm.ToolUse, ToolUseID: tc.ID, ToolName: tc.Function.Name, Input: json.RawMessage(tc.Function.Arguments),
		})
	}
	switch c.FinishReason {
	case "stop":
		out.StopReason = llm.StopEnd
	case "tool_calls", "function_call":
		out.StopReason = llm.StopToolUse
	case "length":
		out.StopReason = llm.StopMaxTokens
	case "content_filter":
		out.StopReason = llm.StopRefusal
	}
	if len(out.Message.ToolUses()) > 0 {
		out.StopReason = llm.StopToolUse
	}
	return out
}

func wrapErr(err error) error {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		return &llm.APIError{Status: apiErr.StatusCode, Message: apiErr.Error(), Err: err}
	}
	return err
}
