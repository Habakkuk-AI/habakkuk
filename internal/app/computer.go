package app

import (
	"slices"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/tools/computer"
)

// openComputer registers the computer tools. Desktops start on an agent's
// first action (a virtual one with backend xvfb), not here.
func (a *App) openComputer(home string) {
	var agents []string
	for name, ac := range a.Cfg.Agents {
		if slices.Contains(ac.Toolsets, computer.Toolset) {
			agents = append(agents, name)
		}
	}
	m := computer.New(a.Cfg.ComputerConfig(), computer.Options{Home: home, Agents: agents, Secrets: a.SecretValues, Log: a.Log})
	computer.RegisterTools(a.Tools, m)
	a.closers = append(a.closers, m.Close)
}

// computerTools gives an agent that lists the computer toolset either the
// provider's native computer-use tool, when every model in its chain takes
// it, or the computer_* tools; computer_open comes with both. Never both
// kinds: the model would see two ways to do one thing.
func computerTools(cfg *config.Config, ac *config.Agent, spec *agent.Spec) {
	if !slices.Contains(spec.Toolsets, computer.Toolset) {
		return
	}
	toolsets := slices.Clone(spec.Toolsets)
	if !slices.Contains(toolsets, computer.OpenToolset) {
		toolsets = append(toolsets, computer.OpenToolset)
	}
	if *cfg.ComputerConfig().Native && nativeComputer(cfg, ac) {
		toolsets = slices.DeleteFunc(toolsets, func(ts string) bool { return ts == computer.Toolset })
		spec.ServerTools = append(slices.Clone(spec.ServerTools), computer.ServerTool)
	}
	spec.Toolsets = toolsets
}

// nativeComputer reports whether every model the agent may answer with is
// an Anthropic model that takes the native computer-use toolset, so a
// fallback never lands on a model without a computer tool.
func nativeComputer(cfg *config.Config, ac *config.Agent) bool {
	for _, m := range append([]string{ac.Model}, ac.Fallbacks...) {
		ref, err := config.ParseModelRef(m)
		if err != nil {
			return false
		}
		p := cfg.Providers[ref.Provider]
		if p == nil || p.Type != "anthropic" || !computer.NativeModel(ref.Model) {
			return false
		}
	}
	return true
}
