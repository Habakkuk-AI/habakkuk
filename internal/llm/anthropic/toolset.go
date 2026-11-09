package anthropic

import (
	"encoding/json"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// Client toolsets (computer_toolset_20260801) are tools the provider
// defines and Habakkuk runs. The model calls a member ("left_click") with
// toolset_name "computer"; Habakkuk stores that as one call of the tool named
// after the toolset, with the member's name as "action" in its input, and
// splits it back when the conversation is replayed. A member's result names
// the toolset too, and carries the call's screenshot inside it.

// computerToolset is the ServerTools name that enables the native
// computer-use toolset.
const computerToolset = "computer"

// fromToolsetUse turns a toolset member's call into a Habakkuk tool call.
func fromToolsetUse(b sdk.ToolUseBlock) llm.Block {
	in := map[string]json.RawMessage{}
	if len(b.Input) > 0 {
		_ = json.Unmarshal(b.Input, &in) // members take objects; anything else becomes {}
	}
	name, _ := json.Marshal(b.Name)
	in["action"] = name
	input, _ := json.Marshal(in)
	return llm.Block{Type: llm.ToolUse, ToolUseID: b.ID, ToolName: b.ToolsetName, Toolset: b.ToolsetName, Input: input}
}

// toToolsetUse is the replay of a call fromToolsetUse made.
func toToolsetUse(b llm.Block) sdk.ContentBlockParamUnion {
	in := map[string]json.RawMessage{}
	if len(b.Input) > 0 {
		_ = json.Unmarshal(b.Input, &in)
	}
	var name string
	_ = json.Unmarshal(in["action"], &name)
	delete(in, "action")
	return sdk.ContentBlockParamUnion{OfToolUse: &sdk.ToolUseBlockParam{
		ID: b.ToolUseID, Name: name, Input: in, ToolsetName: sdk.String(b.Toolset),
	}}
}

// toolsetCalls maps the ids of toolset member calls in the conversation to
// their toolset.
func toolsetCalls(msgs []llm.Message) map[string]string {
	var out map[string]string
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == llm.ToolUse && b.Toolset != "" {
				if out == nil {
					out = map[string]string{}
				}
				out[b.ToolUseID] = b.Toolset
			}
		}
	}
	return out
}

// toolsetResult is a toolset member's result: its text, then the images
// the call attached (blocks of m carrying its ToolUseID), which the API
// wants inside the result rather than after it.
func toolsetResult(req llm.Request, m llm.Message, b llm.Block, toolset string) sdk.ContentBlockParamUnion {
	text := b.Text
	if text == "" {
		text = "OK"
	}
	content := []sdk.ToolResultBlockParamContentUnion{{OfText: &sdk.TextBlockParam{Text: text}}}
	for _, mb := range m.Blocks {
		if mb.Type != llm.Image || mb.ToolUseID != b.ToolUseID {
			continue
		}
		data, err := req.LoadMedia(mb)
		if err != nil {
			content = append(content, sdk.ToolResultBlockParamContentUnion{OfText: &sdk.TextBlockParam{Text: llm.MediaPlaceholder(mb)}})
			continue
		}
		content = append(content, sdk.ToolResultBlockParamContentUnion{OfImage: &sdk.ImageBlockParam{
			Source: sdk.ImageBlockParamSourceUnion{OfBase64: &sdk.Base64ImageSourceParam{
				Data: data, MediaType: sdk.Base64ImageSourceMediaType(mb.MediaType)}}}})
	}
	return sdk.ContentBlockParamUnion{OfToolResult: &sdk.ToolResultBlockParam{
		ToolUseID: b.ToolUseID, ToolsetName: sdk.String(toolset), IsError: sdk.Bool(b.IsError), Content: content,
	}}
}

// nestedMedia reports whether an image block belongs inside a toolset
// result of the same message.
func nestedMedia(m llm.Message, b llm.Block, toolsets map[string]string) bool {
	if b.Type != llm.Image || b.ToolUseID == "" || toolsets[b.ToolUseID] == "" {
		return false
	}
	for _, r := range m.Blocks {
		if r.Type == llm.ToolResult && r.ToolUseID == b.ToolUseID {
			return true
		}
	}
	return false
}
