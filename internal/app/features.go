package app

import (
	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// agentFeatures sets an agent's tool search threshold, loop guard and
// automatic titles from its config. It runs after the spec's chain is built.
func agentFeatures(ac *config.Agent, spec *agent.Spec) {
	spec.ToolSearchOver = max(ac.ToolSearchOver, 0) // -1 (off) becomes 0
	spec.ProjectFiles = ac.ProjectFiles
	// Loop guard thresholds: -1 (off) becomes 0.
	if g := ac.LoopGuard; g != nil {
		spec.Loop = agent.LoopLimits{RepeatWarn: max(g.RepeatWarn, 0), RepeatStop: max(g.RepeatStop, 0),
			IdleWarn: max(g.IdleWarn, 0), IdleStop: max(g.IdleStop, 0)}
	}
	if ac.TitleModel == "off" {
		return
	}
	spec.AutoTitle = true
	if ac.TitleModel == "" && len(spec.FastChain) > 0 {
		spec.TitleChain = spec.FastChain // a chore: the fast model writes titles
		return
	}
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
