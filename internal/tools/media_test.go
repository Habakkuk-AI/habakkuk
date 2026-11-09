package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

type mediaTool struct{ fail bool }

func (mediaTool) Risk() Risk { return RiskRead }
func (mediaTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "pic", Schema: json.RawMessage(`{"type":"object"}`)}
}
func (m mediaTool) Run(ctx context.Context, _ *Env, _ json.RawMessage) (string, error) {
	AddMedia(ctx, llm.Block{Type: llm.Image, Media: "sha256:1"}, llm.Block{Type: llm.Image, Media: "sha256:2"})
	if m.fail {
		return "", errors.New("broke after attaching")
	}
	return "two pictures", nil
}

func TestExecuteMedia(t *testing.T) {
	call := llm.Block{Type: llm.ToolUse, ToolUseID: "c", ToolName: "pic", Input: json.RawMessage(`{}`)}
	r := NewRegistry()
	r.Register("core", mediaTool{})
	res, media := r.ExecuteMedia(context.Background(), &Env{Mode: ModeFull}, call)
	if res.IsError || res.Text != "two pictures" || len(media) != 2 || media[1].Media != "sha256:2" {
		t.Fatalf("result %+v, media %+v", res, media)
	}
	if res := r.Execute(context.Background(), &Env{Mode: ModeFull}, call); res.IsError {
		t.Fatalf("Execute without a sink: %+v", res)
	}
	if AddMedia(context.Background(), llm.Block{}) {
		t.Fatal("AddMedia without a sink reported success")
	}
	r = NewRegistry()
	r.Register("core", mediaTool{fail: true})
	if res, media := r.ExecuteMedia(context.Background(), &Env{Mode: ModeFull}, call); !res.IsError || media != nil {
		t.Fatalf("an error result carries no media: %+v %+v", res, media)
	}
}
