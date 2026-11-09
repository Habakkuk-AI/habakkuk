package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

func TestServerToolBlocksRoundTrip(t *testing.T) {
	var msg sdk.Message
	raw := `{"id":"m","type":"message","role":"assistant","model":"x","stop_reason":"end_turn","content":[
		{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"go 1.27 release"}},
		{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","url":"https://go.dev","title":"Go","encrypted_content":"abc"}]},
		{"type":"text","text":"Go 1.27 is out."}],"usage":{"input_tokens":1,"output_tokens":1}}`
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	resp := toResponse(&msg)
	if len(resp.Message.Blocks) != 3 || resp.Message.Blocks[0].Type != llm.Opaque || resp.Message.Blocks[1].Type != llm.Opaque {
		t.Fatalf("blocks: %+v", resp.Message.Blocks)
	}
	// The stored message survives a JSON round trip (the transcript) and
	// replays the server blocks verbatim alongside the server tools.
	stored, _ := json.Marshal(resp.Message)
	var back llm.Message
	json.Unmarshal(stored, &back)
	params, err := buildParams(llm.Request{Model: "m", MaxTokens: 10, ServerTools: []string{"web_search", "web_fetch"},
		Messages: []llm.Message{llm.UserText("q"), back}})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(params)
	for _, want := range []string{`"type":"server_tool_use"`, `"encrypted_content":"abc"`, `"type":"web_search_20260209"`, `"type":"web_fetch_20260209"`, `"name":"web_search"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("request missing %s:\n%s", want, out)
		}
	}
	// Another provider's opaque blocks are dropped.
	back.Blocks[0].Provider = "openai"
	params, _ = buildParams(llm.Request{Model: "m", MaxTokens: 10, Messages: []llm.Message{llm.UserText("q"), back}})
	out, _ = json.Marshal(params)
	if strings.Contains(string(out), "server_tool_use") {
		t.Fatal("foreign opaque block replayed")
	}
}

func TestMediaBlocks(t *testing.T) {
	req := llm.Request{Model: "m", MaxTokens: 10, Media: func(ref string) ([]byte, error) { return []byte("img:" + ref), nil },
		Messages: []llm.Message{{Role: llm.User, Blocks: []llm.Block{
			{Type: llm.Text, Text: "look"},
			{Type: llm.Image, Media: "sha256:1", MediaType: "image/png", Name: "a.png"},
			{Type: llm.Document, Media: "sha256:2", MediaType: "application/pdf", Name: "b.pdf"},
		}}}}
	params, err := buildParams(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(params)
	for _, want := range []string{`"type":"image"`, `"media_type":"image/png"`, `"type":"document"`, `"media_type":"application/pdf"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %s:\n%s", want, out)
		}
	}
	req.Media = nil
	params, _ = buildParams(req)
	out, _ = json.Marshal(params)
	if !strings.Contains(string(out), "could not be attached") {
		t.Fatal("missing media should become a placeholder")
	}
}
