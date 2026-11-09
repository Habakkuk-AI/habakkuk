package app

import (
	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// agentFeatures sets an agent's tool search threshold and automatic
// titles from its config. It runs after the spec's chain is built.
func agentFeatures(ac *config.Agent, spec *agent.Spec) {
	spec.ToolSearchOver = max(ac.ToolSearchOver, 0) // -1 (off) becomes 0
	if ac.TitleModel == "off" {
		return
	}
	spec.AutoTitle = true
	if ref, err := config.ParseModelRef(ac.TitleModel); err == nil { // validated at load
		first := llm.Target{Endpoint: ref.Provider, Model: ref.Model}
		spec.TitleChain = []llm.Target{first}
		for _, t := range spec.Chain {
			if t != first {
				spec.TitleChain = append(spec.TitleChain, t)
			}
		}
	}
}
