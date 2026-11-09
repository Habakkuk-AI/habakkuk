package agent

import (
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// markHostedContent taints the turn when the model used the provider's
// hosted web search or fetch: their results reach the model inside opaque
// blocks, not through a Habakkuk tool, so the registry never sees them.
func markHostedContent(env *tools.Env, m llm.Message) {
	for _, b := range m.Blocks {
		if b.Type == llm.Opaque {
			env.Taint.Mark()
			return
		}
	}
}
