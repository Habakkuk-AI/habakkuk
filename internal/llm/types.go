// Package llm defines Habakkuk's provider-neutral model interface. Adapters in
// subpackages translate to each vendor's API; the Router adds retry,
// credential rotation and model fallback on top.
package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

type Role string

const (
	User      Role = "user"
	Assistant Role = "assistant"
)

type BlockType string

const (
	Text       BlockType = "text"
	ToolUse    BlockType = "tool_use"
	ToolResult BlockType = "tool_result"
	Thinking   BlockType = "thinking"
	// RedactedThinking carries opaque reasoning data that must be replayed
	// unchanged to the provider that produced it.
	RedactedThinking BlockType = "redacted_thinking"
	// Image and Document carry media by blob reference; the bytes are loaded
	// through Request.Media only when a request is built.
	Image    BlockType = "image"
	Document BlockType = "document"
	// Opaque is a provider-specific block (server tool calls and results,
	// such as web search) kept verbatim and replayed only to that provider.
	Opaque BlockType = "opaque"
)

// Block is one piece of message content. Only the fields relevant to Type
// are set.
type Block struct {
	Type BlockType `json:"type"`
	Text string    `json:"text,omitempty"`

	// tool_use and tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	ToolName  string          `json:"tool_name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	// Tainted marks a tool_result that brought outside content into the
	// turn (Habakkuk's own record; never sent to a provider), so a turn
	// resumed after a restart is tainted again.
	Tainted bool `json:"tainted,omitempty"`
	// Toolset marks a call to a member of a provider-defined client toolset
	// (the native computer-use tool): ToolName is the toolset, which Habakkuk
	// runs as one tool, and Input carries the member's name as "action".
	// An image or document a tool attached may carry its call's ToolUseID,
	// so a provider that nests media in tool results can put it there.
	Toolset string `json:"toolset,omitempty"`

	// thinking and redacted_thinking: replayed only to the same provider type.
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
	Provider  string `json:"provider,omitempty"`

	// image and document
	Media     string `json:"media,omitempty"` // blob reference, "sha256:<hex>"
	MediaType string `json:"media_type,omitempty"`
	Name      string `json:"name,omitempty"`

	// opaque
	Raw json.RawMessage `json:"raw,omitempty"`
}

type Message struct {
	Role   Role    `json:"role"`
	Blocks []Block `json:"blocks"`
}

func (m Message) ToolUses() []Block {
	var out []Block
	for _, b := range m.Blocks {
		if b.Type == ToolUse {
			out = append(out, b)
		}
	}
	return out
}

func (m Message) Text() string {
	s := ""
	for _, b := range m.Blocks {
		if b.Type == Text {
			s += b.Text
		}
	}
	return s
}

func UserText(s string) Message {
	return Message{Role: User, Blocks: []Block{{Type: Text, Text: s}}}
}

type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema object for the input
}

type Request struct {
	Model     string
	System    string // frozen per session; providers mark it cacheable
	Messages  []Message
	Tools     []ToolSpec
	MaxTokens int
	Effort    string
	// ServerTools names provider-hosted tools to enable where supported
	// ("web_search", "web_fetch"), and provider-defined tools Habakkuk runs
	// itself ("computer", the native computer-use toolset). Providers
	// without them ignore the list.
	ServerTools []string
	// Media loads the bytes of an image or document block.
	Media func(ref string) ([]byte, error)
}

// LoadMedia returns a block's bytes base64-encoded, or an error when the
// request has no loader or the blob is gone.
func (r Request) LoadMedia(b Block) (string, error) {
	if r.Media == nil {
		return "", errors.New("no media loader")
	}
	data, err := r.Media(b.Media)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// MediaPlaceholder is the text a provider sends instead of media it cannot
// load or accept.
func MediaPlaceholder(b Block) string {
	return fmt.Sprintf("[%s %q could not be attached]", b.Type, b.Name)
}

type StopReason string

const (
	StopEnd       StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopRefusal   StopReason = "refusal"
	StopPause     StopReason = "pause_turn"
	StopOther     StopReason = "other"
)

// Usage counts a response's tokens. InputTokens excludes cached input:
// CacheReadTokens and CacheWriteTokens are counted separately, so Prompt is
// the size of the whole request.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
}

// Prompt is the number of tokens the request filled in the context window.
func (u Usage) Prompt() int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
}

type Response struct {
	Message    Message
	StopReason StopReason
	Usage      Usage
	Model      string // the model that actually answered, as "<provider>/<model>"
}

type EventType string

const (
	EventText     EventType = "text"
	EventThinking EventType = "thinking"
	EventToolCall EventType = "tool_call"
	// EventRetry tells the client that partial output so far is void and the
	// request is being retried (possibly on another model).
	EventRetry EventType = "retry"
)

type Event struct {
	Type EventType
	Text string
}

// Provider streams one request against one credential. Implementations must
// not retry internally; the Router owns retry policy.
type Provider interface {
	Stream(ctx context.Context, apiKey string, req Request, onEvent func(Event)) (*Response, error)
}
