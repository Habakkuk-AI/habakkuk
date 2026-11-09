// Package anthropic adapts the Claude Messages API (official Go SDK) to
// Habakkuk's provider interface.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

const providerType = "anthropic"

type Provider struct {
	BaseURL string
}

func (p *Provider) Stream(ctx context.Context, apiKey string, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error) {
	opts := []option.RequestOption{option.WithAPIKey(apiKey), option.WithMaxRetries(0)}
	if p.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(p.BaseURL))
	}
	client := sdk.NewClient(opts...)

	params, err := buildParams(req)
	if err != nil {
		return nil, err
	}
	stream := client.Messages.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()
	msg := sdk.Message{}
	for stream.Next() {
		ev := stream.Current()
		if err := msg.Accumulate(ev); err != nil {
			return nil, fmt.Errorf("accumulate stream: %w", err)
		}
		switch e := ev.AsAny().(type) {
		case sdk.ContentBlockDeltaEvent:
			switch d := e.Delta.AsAny().(type) {
			case sdk.TextDelta:
				onEvent(llm.Event{Type: llm.EventText, Text: d.Text})
			case sdk.ThinkingDelta:
				onEvent(llm.Event{Type: llm.EventThinking, Text: d.Thinking})
			}
		case sdk.ContentBlockStartEvent:
			switch b := e.ContentBlock.AsAny().(type) {
			case sdk.ToolUseBlock:
				onEvent(llm.Event{Type: llm.EventToolCall, Text: b.Name})
			case sdk.ServerToolUseBlock:
				onEvent(llm.Event{Type: llm.EventToolCall, Text: string(b.Name)})
			}
		}
	}
	if err := stream.Err(); err != nil {
		return nil, wrapErr(err)
	}
	return toResponse(&msg), nil
}

// ListModels lists the models the key can use.
func (p *Provider) ListModels(ctx context.Context, apiKey string) ([]string, error) {
	opts := []option.RequestOption{option.WithAPIKey(apiKey), option.WithMaxRetries(0)}
	if p.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(p.BaseURL))
	}
	client := sdk.NewClient(opts...)
	iter := client.Models.ListAutoPaging(ctx, sdk.ModelListParams{})
	var out []string
	for iter.Next() {
		out = append(out, iter.Current().ID)
	}
	if err := iter.Err(); err != nil {
		return nil, wrapErr(err)
	}
	return out, nil
}

func buildParams(req llm.Request) (sdk.MessageNewParams, error) {
	params := sdk.MessageNewParams{
		Model:     sdk.Model(req.Model),
		MaxTokens: int64(req.MaxTokens),
		// Top-level cache control places a breakpoint on the last cacheable
		// block, so the conversation prefix is cached as it grows.
		CacheControl: sdk.NewCacheControlEphemeralParam(),
	}
	if req.System != "" {
		params.System = []sdk.TextBlockParam{{Text: req.System, CacheControl: sdk.NewCacheControlEphemeralParam()}}
	}
	if req.Effort != "" {
		params.OutputConfig = sdk.OutputConfigParam{Effort: sdk.OutputConfigEffort(req.Effort)}
	}
	for _, t := range req.Tools {
		var schema struct {
			Properties any      `json:"properties"`
			Required   []string `json:"required"`
		}
		if err := json.Unmarshal(t.Schema, &schema); err != nil {
			return params, fmt.Errorf("tool %s schema: %w", t.Name, err)
		}
		tp := sdk.ToolParam{
			Name:                t.Name,
			Description:         sdk.String(t.Description),
			InputSchema:         sdk.ToolInputSchemaParam{Properties: schema.Properties, Required: schema.Required},
			EagerInputStreaming: sdk.Bool(true),
		}
		params.Tools = append(params.Tools, sdk.ToolUnionParam{OfTool: &tp})
	}
	for _, name := range req.ServerTools {
		switch name {
		case "web_search":
			params.Tools = append(params.Tools, sdk.ToolUnionParam{OfWebSearchTool20260209: &sdk.WebSearchTool20260209Param{}})
		case "web_fetch":
			params.Tools = append(params.Tools, sdk.ToolUnionParam{OfWebFetchTool20260209: &sdk.WebFetchTool20260209Param{}})
		case computerToolset:
			params.Tools = append(params.Tools, sdk.ToolUnionParam{OfComputerToolset20260801: &sdk.ComputerToolset20260801Param{}})
		}
	}
	toolsets := toolsetCalls(req.Messages)
	for _, m := range req.Messages {
		blocks := toParamBlocks(req, m, toolsets)
		if len(blocks) == 0 {
			continue
		}
		if m.Role == llm.Assistant {
			params.Messages = append(params.Messages, sdk.NewAssistantMessage(blocks...))
		} else {
			params.Messages = append(params.Messages, sdk.NewUserMessage(blocks...))
		}
	}
	return params, nil
}

func toParamBlocks(req llm.Request, m llm.Message, toolsets map[string]string) []sdk.ContentBlockParamUnion {
	var out []sdk.ContentBlockParamUnion
	for _, b := range m.Blocks {
		switch b.Type {
		case llm.Text:
			if b.Text != "" {
				out = append(out, sdk.NewTextBlock(b.Text))
			}
		case llm.Thinking:
			// Reasoning blocks are bound to the provider that wrote them.
			if b.Provider == providerType {
				out = append(out, sdk.NewThinkingBlock(b.Signature, b.Text))
			}
		case llm.RedactedThinking:
			if b.Provider == providerType {
				out = append(out, sdk.NewRedactedThinkingBlock(b.Data))
			}
		case llm.ToolUse:
			if b.Toolset != "" {
				out = append(out, toToolsetUse(b))
				continue
			}
			var input any = map[string]any{}
			if len(b.Input) > 0 && json.Valid(b.Input) {
				input = json.RawMessage(b.Input)
			}
			out = append(out, sdk.NewToolUseBlock(b.ToolUseID, input, b.ToolName))
		case llm.ToolResult:
			if ts := toolsets[b.ToolUseID]; ts != "" {
				out = append(out, toolsetResult(req, m, b, ts))
				continue
			}
			out = append(out, sdk.NewToolResultBlock(b.ToolUseID, b.Text, b.IsError))
		case llm.Opaque:
			if b.Provider == providerType {
				var u sdk.ContentBlockUnion
				if err := json.Unmarshal(b.Raw, &u); err == nil {
					out = append(out, u.ToParam())
				}
			}
		case llm.Image, llm.Document:
			if nestedMedia(m, b, toolsets) {
				continue // inside its toolset result
			}
			data, err := req.LoadMedia(b)
			switch {
			case err != nil:
				out = append(out, sdk.NewTextBlock(llm.MediaPlaceholder(b)))
			case b.Type == llm.Image:
				out = append(out, sdk.NewImageBlockBase64(b.MediaType, data))
			default:
				out = append(out, sdk.NewDocumentBlock(sdk.Base64PDFSourceParam{Data: data}))
			}
		}
	}
	return out
}

func toResponse(m *sdk.Message) *llm.Response {
	out := &llm.Response{
		Message: llm.Message{Role: llm.Assistant},
		Usage: llm.Usage{
			InputTokens:      m.Usage.InputTokens,
			OutputTokens:     m.Usage.OutputTokens,
			CacheReadTokens:  m.Usage.CacheReadInputTokens,
			CacheWriteTokens: m.Usage.CacheCreationInputTokens,
		},
	}
	for _, block := range m.Content {
		switch b := block.AsAny().(type) {
		case sdk.TextBlock:
			out.Message.Blocks = append(out.Message.Blocks, llm.Block{Type: llm.Text, Text: b.Text})
		case sdk.ThinkingBlock:
			out.Message.Blocks = append(out.Message.Blocks, llm.Block{Type: llm.Thinking, Text: b.Thinking, Signature: b.Signature, Provider: providerType})
		case sdk.RedactedThinkingBlock:
			out.Message.Blocks = append(out.Message.Blocks, llm.Block{Type: llm.RedactedThinking, Data: b.Data, Provider: providerType})
		case sdk.ToolUseBlock:
			if b.ToolsetName != "" {
				out.Message.Blocks = append(out.Message.Blocks, fromToolsetUse(b))
				continue
			}
			out.Message.Blocks = append(out.Message.Blocks, llm.Block{Type: llm.ToolUse, ToolUseID: b.ID, ToolName: b.Name, Input: b.Input})
		default:
			// Server tool calls and their results: keep verbatim for replay.
			if raw := block.RawJSON(); raw != "" {
				out.Message.Blocks = append(out.Message.Blocks, llm.Block{Type: llm.Opaque, Raw: json.RawMessage(raw), Provider: providerType})
			}
		}
	}
	switch m.StopReason {
	case sdk.StopReasonEndTurn, sdk.StopReasonStopSequence:
		out.StopReason = llm.StopEnd
	case sdk.StopReasonToolUse:
		out.StopReason = llm.StopToolUse
	case sdk.StopReasonMaxTokens:
		out.StopReason = llm.StopMaxTokens
	case sdk.StopReasonRefusal:
		out.StopReason = llm.StopRefusal
	case sdk.StopReasonPauseTurn:
		out.StopReason = llm.StopPause
	default:
		out.StopReason = llm.StopOther
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
