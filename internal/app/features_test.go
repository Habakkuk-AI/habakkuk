package app

import (
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

func TestAgentFeatures(t *testing.T) {
	chain := []llm.Target{{Endpoint: "p", Model: "big"}, {Endpoint: "q", Model: "small"}}
	spec := &agent.Spec{Chain: chain}
	agentFeatures(&config.Agent{ToolSearchOver: 60, TitleModel: "q/small"}, spec)
	if spec.ToolSearchOver != 60 || !spec.AutoTitle || len(spec.TitleChain) != 2 || spec.TitleChain[0].Model != "small" || spec.TitleChain[1].Model != "big" {
		t.Fatalf("spec = %+v", spec)
	}
	spec = &agent.Spec{Chain: chain}
	agentFeatures(&config.Agent{ToolSearchOver: -1, TitleModel: "off"}, spec)
	if spec.ToolSearchOver != 0 || spec.AutoTitle {
		t.Fatalf("off: %+v", spec)
	}
	spec = &agent.Spec{Chain: chain}
	agentFeatures(&config.Agent{}, spec)
	if !spec.AutoTitle || spec.TitleChain != nil {
		t.Fatalf("default: %+v", spec)
	}
}

func TestVideoToolAndAgentDefaultsWired(t *testing.T) {
	a, _, _ := openTestApp(t, "\nvideo: {provider: fal}\n")
	names := map[string]bool{}
	for _, s := range a.Tools.Specs([]string{"core", "delegate"}) {
		names[s.Name] = true
	}
	if !names["video_generate"] || !names["delegate"] {
		t.Fatalf("tools: %v", names)
	}
	if spec := a.Runner.Agents["habakkuk"]; spec.ToolSearchOver != config.DefaultToolSearchOver || !spec.AutoTitle {
		t.Fatalf("spec: %+v", spec)
	}
}
