package security

import (
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// Endpoint is one outbound destination Habakkuk may contact.
type Endpoint struct {
	Kind    string // model, channel, search, media, memory, telemetry, mcp, a2a, integration, hub, update, agent
	Name    string // what in config.yaml turns it on
	Target  string // host or URL; "" when it depends on what the agent is asked
	When    string // when the call happens
	Enabled bool   // on in this config
}

// String renders one report line.
func (e Endpoint) String() string {
	state := "off"
	if e.Enabled {
		state = "on "
	}
	target := e.Target
	if target == "" {
		target = "-"
	}
	return fmt.Sprintf("[%s] %-11s %-28s %-40s %s", state, e.Kind, e.Name, target, e.When)
}

// channelHosts are the servers each channel type talks to by default; an
// api_url, homeserver or url setting replaces them.
var channelHosts = map[string]string{
	"telegram": "api.telegram.org", "slack": "slack.com (Socket Mode)", "discord": "discord.com, gateway.discord.gg",
	"whatsapp": "WhatsApp's servers (linked device)", "whatsappagent": "api.whatsapp.com", "signal": "signal-cli at 127.0.0.1:8080",
	"sms": "api.twilio.com", "msteams": "Bot Framework (botframework.com, login.microsoftonline.com)",
	"googlechat": "chat.googleapis.com, oauth2.googleapis.com", "line": "api.line.me, api-data.line.me",
	"ntfy": "ntfy.sh", "whatsappcloud": "graph.facebook.com", "messenger": "graph.facebook.com",
	"instagram": "graph.facebook.com", "feishu": "open.feishu.cn / open.larksuite.com", "dingtalk": "api.dingtalk.com, oapi.dingtalk.com",
	"wecom": "qyapi.weixin.qq.com", "twitch": "Twitch (api.twitch.tv, irc)",
}

var searchHosts = map[string]string{
	"brave": "api.search.brave.com", "tavily": "api.tavily.com", "exa": "api.exa.ai", "duckduckgo": "html.duckduckgo.com",
}

var mediaHosts = map[string]string{
	"openai": "api.openai.com", "gemini": "generativelanguage.googleapis.com", "fal": "queue.fal.run",
	"elevenlabs": "api.elevenlabs.io", "edge": "speech.platform.bing.com (Edge read-aloud)",
}

// Network lists every outbound destination Habakkuk can contact and
// whether this config turns it on. Nothing is contacted unless config.yaml
// (or a command the user runs) asks for it: Habakkuk sends no usage
// statistics, checks for updates only on `hk update`, and has no
// web search provider until search: names one.
func Network(c *config.Config) []Endpoint {
	var out []Endpoint
	add := func(e Endpoint) { out = append(out, e) }

	add(Endpoint{Kind: "telemetry", Name: "(usage statistics)", When: "never: Habakkuk has no usage telemetry"})
	add(Endpoint{Kind: "update", Name: "hk update", Target: "api.github.com, github.com", When: "only when you run hk update"})

	for _, name := range sortedKeys(c.Providers) {
		p := c.Providers[name]
		target := p.BaseURL
		switch {
		case p.Auth == "chatgpt":
			target = "chatgpt.com, auth.openai.com"
		case p.Auth == "copilot":
			target = "api.githubcopilot.com, api.github.com"
		case target == "" && p.Type == "anthropic":
			target = "api.anthropic.com"
		case target == "" && p.Type == "gemini":
			target = "generativelanguage.googleapis.com"
		case target == "" && p.Type == "bedrock":
			target = "bedrock-runtime." + firstNonEmpty(p.Region, "<AWS_REGION>") + ".amazonaws.com"
		}
		add(Endpoint{Kind: "model", Name: "providers." + name, Target: host(target), When: "every model call of agents that use it", Enabled: true})
	}

	for _, name := range sortedKeys(c.Channels) {
		ch := c.Channels[name]
		if ch == nil || ch.Type == "web" {
			continue
		}
		target := channelHosts[ch.Type]
		switch {
		case ch.APIURL != "":
			target = host(ch.APIURL)
		case ch.Homeserver != "":
			target = host(ch.Homeserver)
		case ch.Email != nil:
			target = strings.Join(slices.DeleteFunc([]string{ch.Email.IMAP, ch.Email.SMTP}, func(s string) bool { return s == "" }), ", ")
		case target == "":
			target = "the " + ch.Type + " server in its settings"
		}
		add(Endpoint{Kind: "channel", Name: "channels." + name + " (" + ch.Type + ")", Target: target, When: "while hk serve runs", Enabled: true})
	}

	if s := c.Search; s != nil {
		target := searchHosts[s.Provider]
		if s.URL != "" {
			target = host(s.URL)
		}
		add(Endpoint{Kind: "search", Name: "search (" + s.Provider + ")", Target: target, When: "when an agent calls web_search", Enabled: true})
	} else {
		add(Endpoint{Kind: "search", Name: "search", When: "off until search: names a provider (brave, tavily, exa, searxng, duckduckgo)"})
	}
	var hosted []string
	var fetchers []string
	for _, name := range c.AgentNames() {
		a := c.Agents[name]
		if a.Web != nil && *a.Web {
			hosted = append(hosted, name)
		}
		if slices.Contains(a.Toolsets, "core") || slices.Contains(a.Toolsets, "browser") {
			fetchers = append(fetchers, name)
		}
	}
	add(Endpoint{Kind: "agent", Name: "agents.*.web", Target: "the agent's model provider",
		When: "hosted web search and fetch, run by the provider, for " + listOr(hosted, "no agent"), Enabled: len(hosted) > 0})
	add(Endpoint{Kind: "agent", Name: "fetch_url, browser", Target: "public URLs the agent chooses",
		When: "when an agent reads a page (" + listOr(fetchers, "no agent") + "); private addresses refused unless allowed", Enabled: len(fetchers) > 0})

	type feature struct{ kind, name, provider, base string }
	var media []feature
	if t := c.TTS; t != nil {
		media = append(media, feature{"media", "tts", t.Provider, t.BaseURL})
	}
	if t := c.Images; t != nil {
		media = append(media, feature{"media", "images", t.Provider, t.BaseURL})
	}
	if t := c.Video; t != nil {
		media = append(media, feature{"media", "video", t.Provider, t.BaseURL})
	}
	if t := c.Realtime; t != nil {
		media = append(media, feature{"media", "realtime", firstNonEmpty(t.Provider, "openai"), t.BaseURL})
	}
	if c.Memory != nil && c.Memory.Embeddings != nil {
		e := c.Memory.Embeddings
		base := ""
		if p := c.Providers[e.Provider]; p != nil {
			base = p.BaseURL
		}
		media = append(media, feature{"memory", "memory.embeddings", e.Provider, base})
	}
	for _, m := range media {
		target := mediaHosts[m.provider]
		if m.base != "" {
			target = host(m.base)
		}
		add(Endpoint{Kind: m.kind, Name: m.name + " (" + m.provider + ")", Target: target, When: "when that feature is used", Enabled: true})
	}
	if t := c.Transcription; t != nil {
		target := ""
		if p := c.Providers[t.Provider]; p != nil {
			target = host(p.BaseURL)
		}
		add(Endpoint{Kind: "media", Name: "transcription (" + t.Provider + ")", Target: target, When: "for each voice note", Enabled: true})
	}

	if t := c.Telemetry; t != nil && t.OTLPEndpoint != "" {
		what := "spans (no message text)"
		if t.IncludeContent {
			what = "spans with message text and tool output"
		}
		add(Endpoint{Kind: "telemetry", Name: "telemetry.otlp_endpoint", Target: host(t.OTLPEndpoint), When: what, Enabled: true})
	} else {
		add(Endpoint{Kind: "telemetry", Name: "telemetry.otlp_endpoint", When: "off: no traces leave this machine"})
	}

	for _, name := range sortedKeys(c.MCP) {
		m := c.MCP[name]
		if m == nil {
			continue
		}
		if m.URL != "" {
			add(Endpoint{Kind: "mcp", Name: "mcp_servers." + name, Target: host(m.URL), When: "on start and on each tool call", Enabled: true})
		} else {
			add(Endpoint{Kind: "mcp", Name: "mcp_servers." + name, Target: "local program " + m.Command, When: "whatever that program contacts", Enabled: true})
		}
	}
	if c.A2A != nil {
		for _, name := range sortedKeys(c.A2A.Peers) {
			if p := c.A2A.Peers[name]; p != nil {
				add(Endpoint{Kind: "a2a", Name: "a2a.peers." + name, Target: host(p.URL), When: "when an agent calls that peer", Enabled: true})
			}
		}
	}
	if h := c.HomeAssistant; h != nil {
		add(Endpoint{Kind: "integration", Name: "homeassistant", Target: host(h.URL), When: "tool calls and the event listener", Enabled: true})
	}
	regs := c.SkillRegistries()
	var names []string
	for _, r := range regs {
		names = append(names, host(r.URL))
	}
	add(Endpoint{Kind: "hub", Name: "skills.registries", Target: listOr(names, ""),
		When: "only on hk skills/plugins search, install, update", Enabled: len(regs) > 0})
	add(Endpoint{Kind: "hub", Name: "skills install github:...", Target: "api.github.com, codeload.github.com",
		When: "only when you install or update from GitHub"})
	sort.SliceStable(out, func(i, j int) bool { return out[i].Enabled && !out[j].Enabled })
	return out
}

func host(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}

func listOr(xs []string, none string) string {
	if len(xs) == 0 {
		return none
	}
	return strings.Join(xs, ", ")
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
