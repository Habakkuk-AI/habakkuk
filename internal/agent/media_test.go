package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// snapTool attaches an image to its result, as view_image does.
type snapTool struct{}

func (snapTool) Risk() tools.Risk { return tools.RiskRead }
func (snapTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "snap", Description: "x", Schema: json.RawMessage(`{"type":"object"}`)}
}
func (snapTool) Run(ctx context.Context, _ *tools.Env, _ json.RawMessage) (string, error) {
	tools.AddMedia(ctx, llm.Block{Type: llm.Image, Media: "sha256:abc", MediaType: "image/png", Name: "snap.png"})
	return "snapped", nil
}

// snapModel calls snap and list_dir together, then answers saying whether
// the image followed the tool results.
type snapModel struct{ seen []llm.Block }

func (m *snapModel) Send(_ context.Context, _ []llm.Target, req llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Blocks[0].Type == llm.ToolResult {
		m.seen = last.Blocks
		return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "seen"}}}, StopReason: llm.StopEnd}, nil
	}
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{
		{Type: llm.ToolUse, ToolUseID: "a", ToolName: "snap", Input: json.RawMessage(`{}`)},
		{Type: llm.ToolUse, ToolUseID: "b", ToolName: "list_dir", Input: json.RawMessage(`{}`)},
	}}, StopReason: llm.StopToolUse}, nil
}

func TestToolMediaFollowsResults(t *testing.T) {
	ctx := context.Background()
	model := &snapModel{}
	h := newHarness(t, model)
	h.runner.Tools.Register("core", snapTool{})
	s, _ := h.runner.NewSession(ctx, "habakkuk")
	h.runner.Submit(ctx, s.ID, "take a picture")
	j, _ := h.queue.Claim(ctx, "w1", time.Minute, KindTurn)
	if _, err := h.runner.HandleTurn(ctx, j, "w1"); err != nil {
		t.Fatal(err)
	}
	b := model.seen
	if len(b) != 3 || b[0].Type != llm.ToolResult || b[0].ToolUseID != "a" || b[1].Type != llm.ToolResult ||
		b[1].ToolUseID != "b" || b[2].Type != llm.Image || b[2].Media != "sha256:abc" {
		t.Fatalf("tool message blocks: %+v", b)
	}
	msgs := h.transcript(t, s.ID)
	if got := msgs[2].Blocks; len(got) != 3 || got[2].Type != llm.Image {
		t.Fatalf("transcript keeps the image: %+v", got)
	}
}
