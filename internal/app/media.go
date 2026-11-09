package app

import (
	"slices"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/tools/mediatools"
)

// openMedia registers the media tools: view_image, read_document and
// web_search always, image_generate and speak when configured. Voice
// replies need the gateway, so this runs after the channels open.
func (a *App) openMedia() {
	kit := mediatools.New(a.Cfg, a.Secret)
	kit.Register(a.Tools)
	if kit.Speech != nil {
		channel.RegisterSpeak(a.Tools, a.Gateway, kit.Speech)
		if a.Gateway != nil {
			a.Gateway.Speech = kit.Speech
		}
	}
}

// webTools gives an agent web search: the provider's hosted search and
// fetch when its main model has them (Anthropic), otherwise the web_search
// tool, which works with any model. Never both: the hosted tool and the
// function share the name web_search.
func webTools(cfg *config.Config, ac *config.Agent, spec *agent.Spec) {
	if mediatools.HostedSearch(cfg, ac) {
		spec.ServerTools = []string{"web_search", "web_fetch"}
		return
	}
	if !slices.Contains(spec.Toolsets, mediatools.SearchToolset) {
		spec.Toolsets = append(slices.Clone(spec.Toolsets), mediatools.SearchToolset)
	}
}
