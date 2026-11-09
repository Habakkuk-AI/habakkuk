// Package bedrock adapts the AWS Bedrock Converse API (official AWS SDK,
// SigV4 signing with the standard credential chain) to Habakkuk's provider
// interface.
package bedrock

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	smithy "github.com/aws/smithy-go"
	"github.com/aws/smithy-go/auth/bearer"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

const providerType = "bedrock"

type Provider struct {
	Region  string // default from AWS_REGION or the shared config
	BaseURL string // endpoint override, e.g. a VPC endpoint
	// Credentials overrides the default credential chain (tests).
	Credentials aws.CredentialsProvider
	HTTP        *http.Client

	mu     sync.Mutex
	client *bedrockruntime.Client
}

func (p *Provider) getClient(ctx context.Context) (*bedrockruntime.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, nil
	}
	var opts []func(*awsconfig.LoadOptions) error
	if p.Region != "" {
		opts = append(opts, awsconfig.WithRegion(p.Region))
	}
	if p.Credentials != nil {
		opts = append(opts, awsconfig.WithCredentialsProvider(p.Credentials))
	}
	if p.HTTP != nil {
		opts = append(opts, awsconfig.WithHTTPClient(p.HTTP))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("bedrock: load AWS config: %w", err)
	}
	if cfg.Region == "" {
		return nil, errors.New("bedrock: no region: set region in the provider or AWS_REGION")
	}
	p.client = bedrockruntime.NewFromConfig(cfg, func(o *bedrockruntime.Options) {
		if p.BaseURL != "" {
			o.BaseEndpoint = aws.String(p.BaseURL)
		}
		o.RetryMaxAttempts = 1 // the router owns retries
	})
	return p.client, nil
}

// Stream sends one ConverseStream request. A non-empty apiKey is a Bedrock
// API key (bearer token); otherwise requests are SigV4-signed with the AWS
// credential chain.
func (p *Provider) Stream(ctx context.Context, apiKey string, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error) {
	client, err := p.getClient(ctx)
	if err != nil {
		return nil, err
	}
	in, err := buildInput(req)
	if err != nil {
		return nil, err
	}
	out, err := client.ConverseStream(ctx, in, func(o *bedrockruntime.Options) {
		if apiKey != "" {
			o.BearerAuthTokenProvider = bearer.StaticTokenProvider{Token: bearer.Token{Value: apiKey}}
			o.AuthSchemePreference = []string{"httpBearerAuth"}
		}
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	stream := out.GetStream()
	defer func() { _ = stream.Close() }()
	acc := &accumulator{blocks: map[int32]*llm.Block{}, onEvent: onEvent}
	for ev := range stream.Events() {
		acc.add(ev)
	}
	if err := stream.Err(); err != nil {
		return nil, wrapErr(err)
	}
	return acc.response(), nil
}

func buildInput(req llm.Request) (*bedrockruntime.ConverseStreamInput, error) {
	in := &bedrockruntime.ConverseStreamInput{ModelId: aws.String(req.Model)}
	if req.MaxTokens > 0 {
		in.InferenceConfig = &types.InferenceConfiguration{MaxTokens: aws.Int32(int32(min(req.MaxTokens, 1<<31-1)))}
	}
	caching := supportsCachePoints(req.Model)
	if req.System != "" {
		in.System = []types.SystemContentBlock{&types.SystemContentBlockMemberText{Value: req.System}}
		if caching {
			in.System = append(in.System, &types.SystemContentBlockMemberCachePoint{Value: types.CachePointBlock{Type: types.CachePointTypeDefault}})
		}
	}
	if len(req.Tools) > 0 {
		tc := &types.ToolConfiguration{}
		for _, t := range req.Tools {
			var schema any = map[string]any{"type": "object"}
			if len(t.Schema) > 0 {
				if err := json.Unmarshal(t.Schema, &schema); err != nil {
					return nil, fmt.Errorf("tool %s schema: %w", t.Name, err)
				}
			}
			tc.Tools = append(tc.Tools, &types.ToolMemberToolSpec{Value: types.ToolSpecification{
				Name: aws.String(t.Name), Description: aws.String(t.Description),
				InputSchema: &types.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(schema)},
			}})
		}
		in.ToolConfig = tc
	}
	for _, m := range req.Messages {
		blocks := toBlocks(req, m)
		if len(blocks) == 0 {
			continue
		}
		role := types.ConversationRoleUser
		if m.Role == llm.Assistant {
			role = types.ConversationRoleAssistant
		}
		in.Messages = append(in.Messages, types.Message{Role: role, Content: blocks})
	}
	// Cache the conversation prefix as it grows, like the Anthropic adapter.
	if caching && len(in.Messages) > 0 {
		last := &in.Messages[len(in.Messages)-1]
		last.Content = append(last.Content, &types.ContentBlockMemberCachePoint{Value: types.CachePointBlock{Type: types.CachePointTypeDefault}})
	}
	return in, nil
}

// supportsCachePoints: Bedrock rejects cache points on models without
// prompt caching, so they are only sent to Claude and Nova models.
func supportsCachePoints(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "anthropic.") || strings.Contains(m, "claude") || strings.Contains(m, "amazon.nova")
}

var imageFormats = map[string]types.ImageFormat{
	"image/png": types.ImageFormatPng, "image/jpeg": types.ImageFormatJpeg,
	"image/gif": types.ImageFormatGif, "image/webp": types.ImageFormatWebp,
}

func toBlocks(req llm.Request, m llm.Message) []types.ContentBlock {
	var out []types.ContentBlock
	for _, b := range m.Blocks {
		own := b.Provider == providerType
		switch b.Type {
		case llm.Text:
			if b.Text != "" {
				out = append(out, &types.ContentBlockMemberText{Value: b.Text})
			}
		case llm.Thinking:
			if own {
				out = append(out, &types.ContentBlockMemberReasoningContent{Value: &types.ReasoningContentBlockMemberReasoningText{
					Value: types.ReasoningTextBlock{Text: aws.String(b.Text), Signature: optString(b.Signature)}}})
			}
		case llm.RedactedThinking:
			if own {
				if data, err := base64.StdEncoding.DecodeString(b.Data); err == nil {
					out = append(out, &types.ContentBlockMemberReasoningContent{Value: &types.ReasoningContentBlockMemberRedactedContent{Value: data}})
				}
			}
		case llm.ToolUse:
			var input any = map[string]any{}
			if len(b.Input) > 0 {
				_ = json.Unmarshal(b.Input, &input)
			}
			out = append(out, &types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{
				ToolUseId: aws.String(b.ToolUseID), Name: aws.String(b.ToolName), Input: document.NewLazyDocument(input)}})
		case llm.ToolResult:
			status := types.ToolResultStatusSuccess
			if b.IsError {
				status = types.ToolResultStatusError
			}
			text := b.Text
			if text == "" {
				text = "(no output)"
			}
			out = append(out, &types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{
				ToolUseId: aws.String(b.ToolUseID), Status: status,
				Content: []types.ToolResultContentBlock{&types.ToolResultContentBlockMemberText{Value: text}}}})
		case llm.Image, llm.Document:
			var data []byte
			err := errors.New("no media loader")
			if req.Media != nil {
				data, err = req.Media(b.Media)
			}
			format, isImage := imageFormats[b.MediaType]
			switch {
			case err != nil || (b.Type == llm.Image && !isImage):
				out = append(out, &types.ContentBlockMemberText{Value: llm.MediaPlaceholder(b)})
			case b.Type == llm.Image:
				out = append(out, &types.ContentBlockMemberImage{Value: types.ImageBlock{Format: format, Source: &types.ImageSourceMemberBytes{Value: data}}})
			default:
				out = append(out, &types.ContentBlockMemberDocument{Value: types.DocumentBlock{
					Format: types.DocumentFormatPdf, Name: aws.String(documentName(b.Name)), Source: &types.DocumentSourceMemberBytes{Value: data}}})
			}
		}
	}
	return out
}

var badNameChars = regexp.MustCompile(`[^A-Za-z0-9\s\-()\[\]]+`)

// documentName meets Bedrock's rule for document names: letters, digits,
// single spaces, hyphens, parentheses and square brackets.
func documentName(name string) string {
	name = strings.TrimSuffix(name, ".pdf")
	name = strings.Join(strings.Fields(badNameChars.ReplaceAllString(name, " ")), " ")
	if name == "" {
		name = "document"
	}
	return name
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return aws.String(s)
}

type accumulator struct {
	onEvent func(llm.Event)
	blocks  map[int32]*llm.Block
	stop    types.StopReason
	usage   types.TokenUsage
}

func (a *accumulator) block(i int32, kind llm.BlockType) *llm.Block {
	b, ok := a.blocks[i]
	if !ok {
		b = &llm.Block{Type: kind}
		a.blocks[i] = b
	}
	return b
}

func (a *accumulator) add(ev types.ConverseStreamOutput) {
	switch e := ev.(type) {
	case *types.ConverseStreamOutputMemberContentBlockStart:
		if tu, ok := e.Value.Start.(*types.ContentBlockStartMemberToolUse); ok {
			b := a.block(aws.ToInt32(e.Value.ContentBlockIndex), llm.ToolUse)
			b.ToolUseID, b.ToolName = aws.ToString(tu.Value.ToolUseId), aws.ToString(tu.Value.Name)
			a.onEvent(llm.Event{Type: llm.EventToolCall, Text: b.ToolName})
		}
	case *types.ConverseStreamOutputMemberContentBlockDelta:
		i := aws.ToInt32(e.Value.ContentBlockIndex)
		switch d := e.Value.Delta.(type) {
		case *types.ContentBlockDeltaMemberText:
			a.block(i, llm.Text).Text += d.Value
			a.onEvent(llm.Event{Type: llm.EventText, Text: d.Value})
		case *types.ContentBlockDeltaMemberToolUse:
			b := a.block(i, llm.ToolUse)
			b.Input = append(b.Input, aws.ToString(d.Value.Input)...)
		case *types.ContentBlockDeltaMemberReasoningContent:
			switch r := d.Value.(type) {
			case *types.ReasoningContentBlockDeltaMemberText:
				b := a.block(i, llm.Thinking)
				b.Text += r.Value
				b.Provider = providerType
				a.onEvent(llm.Event{Type: llm.EventThinking, Text: r.Value})
			case *types.ReasoningContentBlockDeltaMemberSignature:
				b := a.block(i, llm.Thinking)
				b.Signature += r.Value
				b.Provider = providerType
			case *types.ReasoningContentBlockDeltaMemberRedactedContent:
				b := a.block(i, llm.RedactedThinking)
				b.Data = base64.StdEncoding.EncodeToString(r.Value)
				b.Provider = providerType
			}
		}
	case *types.ConverseStreamOutputMemberMessageStop:
		a.stop = e.Value.StopReason
	case *types.ConverseStreamOutputMemberMetadata:
		if e.Value.Usage != nil {
			a.usage = *e.Value.Usage
		}
	}
}

func (a *accumulator) response() *llm.Response {
	idx := make([]int32, 0, len(a.blocks))
	for i := range a.blocks {
		idx = append(idx, i)
	}
	sort.Slice(idx, func(x, y int) bool { return idx[x] < idx[y] })
	out := &llm.Response{Message: llm.Message{Role: llm.Assistant}}
	for _, i := range idx {
		b := *a.blocks[i]
		if b.Type == llm.ToolUse && (len(b.Input) == 0 || !json.Valid(b.Input)) {
			b.Input = json.RawMessage("{}")
		}
		out.Message.Blocks = append(out.Message.Blocks, b)
	}
	out.Usage = llm.Usage{
		InputTokens:      int64(aws.ToInt32(a.usage.InputTokens)),
		OutputTokens:     int64(aws.ToInt32(a.usage.OutputTokens)),
		CacheReadTokens:  int64(aws.ToInt32(a.usage.CacheReadInputTokens)),
		CacheWriteTokens: int64(aws.ToInt32(a.usage.CacheWriteInputTokens)),
	}
	switch a.stop {
	case types.StopReasonEndTurn, types.StopReasonStopSequence:
		out.StopReason = llm.StopEnd
	case types.StopReasonToolUse:
		out.StopReason = llm.StopToolUse
	case types.StopReasonMaxTokens:
		out.StopReason = llm.StopMaxTokens
	case types.StopReasonGuardrailIntervened, types.StopReasonContentFiltered:
		out.StopReason = llm.StopRefusal
	default:
		out.StopReason = llm.StopOther
	}
	if len(out.Message.ToolUses()) > 0 && out.StopReason != llm.StopMaxTokens {
		out.StopReason = llm.StopToolUse
	}
	return out
}

// statusByCode maps Bedrock exception names to HTTP statuses, for errors
// that arrive inside the event stream without one.
var statusByCode = map[string]int{
	"ThrottlingException":           http.StatusTooManyRequests,
	"ServiceQuotaExceededException": http.StatusTooManyRequests,
	"ServiceUnavailableException":   529, // overloaded: retry, then another model
	"ModelNotReadyException":        http.StatusServiceUnavailable,
	"InternalServerException":       http.StatusInternalServerError,
	"ModelStreamErrorException":     http.StatusInternalServerError,
	"ModelTimeoutException":         http.StatusRequestTimeout,
	"AccessDeniedException":         http.StatusForbidden,
	"UnrecognizedClientException":   http.StatusForbidden,
	"ExpiredTokenException":         http.StatusForbidden,
	"ResourceNotFoundException":     http.StatusNotFound,
	"ValidationException":           http.StatusBadRequest,
	"ModelErrorException":           http.StatusBadRequest,
}

func wrapErr(err error) error {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	code := apiErr.ErrorCode()
	status, ok := statusByCode[code]
	var withStatus interface{ HTTPStatusCode() int }
	if errors.As(err, &withStatus) && !ok {
		status = withStatus.HTTPStatusCode()
	}
	msg := "bedrock: " + code + ": " + apiErr.ErrorMessage()
	lower := strings.ToLower(apiErr.ErrorMessage())
	if code == "ValidationException" && (strings.Contains(lower, "too long") || strings.Contains(lower, "too many")) {
		status = http.StatusRequestEntityTooLarge
	}
	return &llm.APIError{Status: status, Message: msg, Err: err}
}
