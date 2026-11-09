package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

func TestComputerToolsetRoundTrip(t *testing.T) {
	var msg sdk.Message
	raw := `{"id":"m","type":"message","role":"assistant","model":"x","stop_reason":"tool_use","content":[
		{"type":"tool_use","id":"toolu_1","name":"left_click","toolset_name":"computer","input":{"coordinate":[10,20],"text":"shift"},"caller":{"type":"direct"}},
		{"type":"tool_use","id":"toolu_2","name":"screenshot","toolset_name":"computer","input":{},"caller":{"type":"direct"}}],
		"usage":{"input_tokens":1,"output_tokens":1}}`
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	resp := toResponse(&msg)
	b := resp.Message.Blocks
	if len(b) != 2 || b[0].Type != llm.ToolUse || b[0].ToolName != "computer" || b[0].Toolset != "computer" {
		t.Fatalf("blocks: %+v", b)
	}
	var in map[string]any
	_ = json.Unmarshal(b[0].Input, &in)
	if in["action"] != "left_click" || in["text"] != "shift" {
		t.Fatalf("input: %s", b[0].Input)
	}
	// The transcript round trip keeps the toolset; the results come back
	// with the screenshot nested in its result.
	stored, _ := json.Marshal(resp.Message)
	var back llm.Message
	_ = json.Unmarshal(stored, &back)
	results := llm.Message{Role: llm.User, Blocks: []llm.Block{
		{Type: llm.ToolResult, ToolUseID: "toolu_1", Text: "Clicked."},
		{Type: llm.ToolResult, ToolUseID: "toolu_2", Text: "Screenshot 1366x768."},
		{Type: llm.Image, Media: "sha256:1", MediaType: "image/png", Name: "screenshot.png", ToolUseID: "toolu_2"},
	}}
	params, err := buildParams(llm.Request{Model: "m", MaxTokens: 10, ServerTools: []string{"computer"},
		Media:    func(string) ([]byte, error) { return []byte("png"), nil },
		Messages: []llm.Message{llm.UserText("go"), back, results}})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(params)
	s := string(out)
	for _, want := range []string{
		`"type":"computer_toolset_20260801"`,
		`"name":"left_click"`, `"toolset_name":"computer"`, `"coordinate":[10,20]`,
		`"tool_use_id":"toolu_2","toolset_name":"computer"`,
		`"data":"cG5n"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("request missing %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, `"action"`) {
		t.Errorf("action leaked into the replayed input:\n%s", s)
	}
	// The image sits inside the result's content, once.
	var p struct {
		Messages []struct {
			Content []struct {
				Type    string            `json:"type"`
				ID      string            `json:"tool_use_id"`
				Content []json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(out, &p)
	last := p.Messages[len(p.Messages)-1].Content
	if len(last) != 2 || last[1].Type != "tool_result" || last[1].ID != "toolu_2" || len(last[1].Content) != 2 ||
		!strings.Contains(string(last[1].Content[1]), `"type":"image"`) {
		t.Fatalf("results message: %+v", last)
	}
	// Without the toolset, images stay where they were.
	plain := results
	plain.Blocks = []llm.Block{{Type: llm.ToolResult, ToolUseID: "x", Text: "ok"}, {Type: llm.Image, Media: "sha256:1", MediaType: "image/png", ToolUseID: "x"}}
	if got := toParamBlocks(llm.Request{Media: func(string) ([]byte, error) { return []byte("png"), nil }}, plain, nil); len(got) != 2 || got[1].OfImage == nil {
		t.Fatalf("plain result with media: %+v", got)
	}
}
