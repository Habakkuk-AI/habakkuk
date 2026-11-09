package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

const (
	maxName        = 64 // what model APIs accept: ^[a-zA-Z0-9_-]{1,64}$
	maxDescription = 1000
	maxResult      = 1 << 20 // the registry moves anything over its inline budget to a blob
)

// tool is one MCP tool as a Habakkuk tool.
type tool struct {
	s      *server
	remote string
	spec   llm.ToolSpec
	risk   tools.Risk
}

func (t *tool) Spec() llm.ToolSpec { return t.spec }
func (t *tool) Risk() tools.Risk   { return t.risk }

func (t *tool) Run(ctx context.Context, _ *tools.Env, input json.RawMessage) (string, error) {
	return t.s.call(ctx, t.remote, input)
}

// toolFor builds the Habakkuk tool for a remote tool. Caller holds s.mu.
func (s *server) toolFor(t *sdk.Tool) *tool {
	name, ok := s.names[t.Name]
	if !ok {
		name = toolName(s.name, t.Name)
		for _, taken := range s.names {
			if taken == name { // two remote names sanitized alike
				name = hashed(name, s.name+"/"+t.Name)
				break
			}
		}
		s.names[t.Name] = name
	}
	desc := truncate(strings.TrimSpace(t.Description), maxDescription)
	if desc == "" {
		desc = fmt.Sprintf("Tool %s from the %s MCP server.", t.Name, s.name)
	}
	risk := s.risk
	if s.approve[t.Name] {
		risk = tools.RiskExec
	}
	return &tool{s: s, remote: t.Name, risk: risk, spec: llm.ToolSpec{Name: name, Description: desc, Schema: inputSchema(t.InputSchema)}}
}

var invalidName = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// toolName is mcp__<server>__<tool> with characters models reject replaced
// by "_". Names over the limit are cut and given a short hash of the full
// name, so the result is deterministic and distinct.
func toolName(server, remote string) string {
	raw := "mcp__" + server + "__" + remote
	name := invalidName.ReplaceAllString(raw, "_")
	if len(name) > maxName {
		name = hashed(name, raw)
	}
	return name
}

func hashed(name, key string) string {
	sum := sha256.Sum256([]byte(key))
	suffix := "_" + hex.EncodeToString(sum[:4])
	if len(name) > maxName-len(suffix) {
		name = name[:maxName-len(suffix)]
	}
	return name + suffix
}

// inputSchema returns the tool's JSON Schema with stable key order, adding
// "type":"object" when the server left it out.
func inputSchema(v any) json.RawMessage {
	var m map[string]any
	if b, err := json.Marshal(v); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	if m == nil {
		m = map[string]any{}
	}
	if _, ok := m["type"]; !ok {
		m["type"] = "object"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return b
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// formatResult turns a result into text: text content as is, a short line
// for anything else, and structured content as JSON unless the text already
// says the same thing.
func formatResult(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		switch c := c.(type) {
		case *sdk.TextContent:
			parts = append(parts, c.Text)
		case *sdk.ImageContent:
			parts = append(parts, fmt.Sprintf("[image %s, %d bytes, not shown]", c.MIMEType, len(c.Data)))
		case *sdk.AudioContent:
			parts = append(parts, fmt.Sprintf("[audio %s, %d bytes, not shown]", c.MIMEType, len(c.Data)))
		case *sdk.ResourceLink:
			parts = append(parts, fmt.Sprintf("[resource link %s: %s]", c.Name, c.URI))
		case *sdk.EmbeddedResource:
			r := c.Resource
			switch {
			case r == nil:
				parts = append(parts, "[empty embedded resource]")
			case r.Text != "":
				parts = append(parts, fmt.Sprintf("[resource %s]\n%s", r.URI, r.Text))
			default:
				parts = append(parts, fmt.Sprintf("[resource %s, %s, %d bytes, not shown]", r.URI, r.MIMEType, len(r.Blob)))
			}
		default:
			parts = append(parts, fmt.Sprintf("[%T content, not shown]", c))
		}
	}
	if res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil && !sameJSON(b, parts) {
			parts = append(parts, string(b))
		}
	}
	out := strings.Join(parts, "\n")
	if len(out) > maxResult {
		out = truncate(out, maxResult) + "\n[result truncated]"
	}
	return out
}

// sameJSON reports whether the only text part is this JSON, which servers
// commonly send twice.
func sameJSON(b []byte, parts []string) bool {
	if len(parts) != 1 {
		return false
	}
	var buf bytes.Buffer
	if json.Compact(&buf, []byte(parts[0])) != nil {
		return false
	}
	var x, y any
	if json.Unmarshal(buf.Bytes(), &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}
