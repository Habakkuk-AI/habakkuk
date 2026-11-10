// Package config loads Habakkuk's YAML configuration. Decoding is strict: an
// unknown or misspelled key is an error, never silently ignored. Secrets are
// never stored in config; providers name the environment variables that hold
// their keys.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
)

const CurrentVersion = 1

type Config struct {
	Version   int                   `yaml:"version"`
	Home      string                `yaml:"-"`
	Report    *LoadReport           `yaml:"-"` // migrations applied in memory, unknown keys ignored (migrate.go)
	Providers map[string]*Provider  `yaml:"providers"`
	Agents    map[string]*Agent     `yaml:"agents"`
	Channels  map[string]*Channel   `yaml:"channels"` // keyed by account name
	MCP       map[string]*MCPServer `yaml:"mcp_servers"`
	// Transcription turns voice notes into text through an OpenAI-compatible
	// provider's /audio/transcriptions endpoint.
	Transcription *Transcription `yaml:"transcription"`
	Timezone      string         `yaml:"timezone"` // for schedules, e.g. America/Toronto; default the server's
	Workers       Workers        `yaml:"workers"`
	Retention     Retention      `yaml:"retention"`
	HTTP          *HTTP          `yaml:"http"` // chat page, API and webhooks; nil is off
	MCPServer     *MCPServe      `yaml:"mcp_server"`

	TTS    *TTS    `yaml:"tts"`    // speech for the speak tool and voice replies; nil is off
	Images *Images `yaml:"images"` // the image_generate tool; nil is off
	Video  *Video  `yaml:"video"`  // the video_generate tool; nil is off
	Search *Search `yaml:"search"` // web_search for models without hosted search; nil is off

	Realtime *Realtime `yaml:"realtime"` // the dashboard's Live voice mode; nil is off
	Talk     *Talk     `yaml:"talk"`     // habakkuk talk, the local voice loop; nil is defaults

	// Hooks run commands on turn, tool, message, session and schedule events.
	Hooks []*Hook `yaml:"hooks"`
	// Pricing overrides or adds list prices, keyed by model id or
	// "<provider>/<model>", for `habakkuk usage` and /usage.
	Pricing map[string]*Price `yaml:"pricing"`
	// Browser configures the browser toolset; nil is a headless launch
	// with defaults.
	Browser *Browser `yaml:"browser"`
	// Computer configures the computer toolset (screen, mouse, keyboard).
	Computer *Computer `yaml:"computer"`
	// Meeting configures meeting attendance (meeting.go); nil is defaults.
	Meeting *Meeting `yaml:"meeting"`
	// Memory adds semantic search and background review to agents' memory;
	// nil is full-text search only.
	Memory *Memory `yaml:"memory"`

	// Operations (ops.go): /metrics, trace export, log rotation, tool
	// safety and secret managers.
	Metrics   Metrics    `yaml:"metrics"`
	Telemetry *Telemetry `yaml:"telemetry"`
	Logging   Logging    `yaml:"logging"`
	Tools     ToolSafety `yaml:"tools"`
	Secrets   *Secrets   `yaml:"secrets"`
	// Skills configures skill registries and the skill curator.
	Skills *Skills `yaml:"skills"`
	// Plugins are the enabled plugins merged in at load (plugins.go).
	Plugins []*Plugin `yaml:"-"`
	// A2A serves the Agent2Agent protocol and configures peer agents
	// (integrations.go); nil is off.
	A2A *A2A `yaml:"a2a"`
	// HomeAssistant is the homeassistant toolset and event triggers
	// (integrations.go); nil is off.
	HomeAssistant *HomeAssistant `yaml:"homeassistant"`
	// Language is the language of Habakkuk's own chat and CLI text (en, fr,
	// es, pt, de, yo, ar, zh, ja, hi); a channel's language overrides it.
	// Default English. Models answer in the user's language regardless.
	Language string `yaml:"language"`
	// Nodes lets other devices lend capabilities (nodes.go); nil is off.
	Nodes *Nodes `yaml:"nodes"`
	// Budget caps model spending for the whole install (budget.go); nil
	// is no cap.
	Budget *Budget `yaml:"budget"`
	// Judge is an optional decision model (judge.go); nil is none.
	Judge *Judge `yaml:"judge"`
}

type Provider struct {
	Type    string   `yaml:"type"` // anthropic | openai (any OpenAI-compatible API) | responses | gemini | bedrock
	BaseURL string   `yaml:"base_url"`
	KeysEnv []string `yaml:"keys_env"` // tried in order; rotated on rate limits and auth failures
	// Preset fills type, base_url and keys_env for a known provider (see
	// providers.go); a provider named after a preset uses it implicitly.
	Preset string `yaml:"preset"`
	// Auth uses a subscription login from `habakkuk auth login` instead of
	// keys: chatgpt or copilot.
	Auth   string `yaml:"auth"`
	Region string `yaml:"region"` // bedrock: AWS region (default AWS_REGION or the shared config)
}

type Agent struct {
	Model     string   `yaml:"model"`     // "<provider>/<model id>"
	Fallbacks []string `yaml:"fallbacks"` // same format, tried in order
	// FastModel ("<provider>/<model>") is a cheaper model for light turns
	// and chores (titles, summaries, memory review). Routing decides which
	// turns use it: auto (default), fast or smart. EscalateAfter is how many
	// tool rounds a turn may take on it before moving to Model (default 4).
	FastModel     string `yaml:"fast_model"`
	Routing       string `yaml:"routing"`
	EscalateAfter int    `yaml:"escalate_after"`
	Effort        string `yaml:"effort"`
	MaxTokens     int    `yaml:"max_tokens"`
	MaxIterations int    `yaml:"max_iterations"`
	Permission    string `yaml:"permission"` // read-only | ask | workspace | full
	// Approvals decides calls the permission mode does not allow outright:
	// human (default), reviewer (a model alone) or reviewer_then_human.
	Approvals     string   `yaml:"approvals"`
	ReviewerModel string   `yaml:"reviewer_model"` // "<provider>/<model>"; default the agent's own chain
	Workspace     string   `yaml:"workspace"`
	Toolsets      []string `yaml:"toolsets"`
	// Web enables the provider's hosted web search and fetch tools where the
	// model supports them (default true).
	Web *bool `yaml:"web"`
	// CompactAt is the prompt size in tokens past which a session's early
	// messages are summarized before the next model call. Default 600000
	// when the main model's provider is anthropic, 150000 otherwise; -1
	// turns automatic compaction off.
	CompactAt int `yaml:"compact_at"`
	// Sandbox is where exec and background processes run: none (this
	// machine, default), docker or ssh, with the matching section.
	Sandbox string         `yaml:"sandbox"`
	Docker  *DockerSandbox `yaml:"docker"`
	SSH     *SSHSandbox    `yaml:"ssh"`
	// Checkpoints snapshot the workspace before a turn's first change, for
	// habakkuk rollback and /undo (default true); CheckpointMaxMB caps one
	// snapshot's new and changed files (default 200).
	Checkpoints     *bool `yaml:"checkpoints"`
	CheckpointMaxMB int   `yaml:"checkpoint_max_mb"`
	// ToolSearchOver: with more tools than this (default 60; -1 off), the
	// model sees only the core tools plus tool_search and tool_describe.
	ToolSearchOver int `yaml:"tool_search_over"`
	// TitleModel ("<provider>/<model>") writes a short title for each new
	// session after its first turn; default the agent's own chain, "off"
	// for none.
	TitleModel string `yaml:"title_model"`
	// Home is the chat where the agent tells its owner about work that
	// started elsewhere (a delegated task, a board card, a schedule, an A2A
	// task, another channel): CHANNEL:CONVERSATION[:THREAD], as
	// `habakkuk send` takes it. It enables the message_home tool.
	Home string `yaml:"home"`
	// ReportHome posts to the home chat automatically: off, results (the
	// default with a home: a short result when such work finishes) or all
	// (also a line when it starts).
	ReportHome string `yaml:"report_home"`
	// Budget caps this agent's model spending (budget.go); nil is no cap.
	Budget *Budget `yaml:"budget"`
	// Safety for channel and tainted turns: trust, channel_allow,
	// allow_tainted_exec (security.go).
	AgentSecurity `yaml:",inline"`
}

// Channel is one messaging account: a Telegram bot, a Slack app (one
// socket for every workspace it is installed in), a linked WhatsApp number,
// a Discord bot, a Signal number served by signal-cli, or a Matrix user.
type Channel struct {
	Type         string            `yaml:"type"`           // telegram | slack | whatsapp | whatsappagent | web | discord | signal | matrix | email | sms | msteams | googlechat | bluebubbles | line | mattermost | irc | ntfy
	Agent        string            `yaml:"agent"`          // default agent for conversations on this account
	Routes       map[string]string `yaml:"routes"`         // conversation (or Slack team) id -> agent
	TokenEnv     string            `yaml:"token_env"`      // telegram, discord: bot token; matrix: access token; whatsappagent: the agent's API key
	BotTokensEnv []string          `yaml:"bot_tokens_env"` // slack: one bot token per workspace
	AppTokenEnv  string            `yaml:"app_token_env"`  // slack: app-level token for Socket Mode
	AllowFrom    []string          `yaml:"allow_from"`     // sender ids allowed to talk; "*" allows everyone
	Groups       string            `yaml:"groups"`         // off | mention | all (default mention)
	SessionIdle  time.Duration     `yaml:"session_idle"`   // start a fresh session after this much silence (default 24h, web never; 0 never)
	APIURL       string            `yaml:"api_url"`        // override the platform API base URL; signal: the signal-cli daemon (default http://127.0.0.1:8080)
	Account      string            `yaml:"account"`        // signal: the bot's number (+15551234567); matrix: its user id (optional, checked against the token)
	Homeserver   string            `yaml:"homeserver"`     // matrix: client-server API base URL
	// VoiceReplies speaks replies as voice notes: off (default), mirror
	// (when the user sent a voice note) or always. Needs tts.
	VoiceReplies string `yaml:"voice_replies"`
	VoiceText    *bool  `yaml:"voice_text"` // also send the reply as text (default true)
	Progress     string `yaml:"progress"`   // draft (default) | off: a long turn's live progress message
	// Reactions marks the user's message with an emoji while a turn runs
	// and when it ends, where the platform has reactions.
	Reactions bool `yaml:"reactions"`
	// Language overrides the global language for Habakkuk's own text on
	// this account.
	Language string `yaml:"language"`

	// Settings for other types, in a block named after the type (platforms.go).
	Email       *Email       `yaml:"email"`
	SMS         *SMS         `yaml:"sms"`
	Teams       *Teams       `yaml:"msteams"`
	GoogleChat  *GoogleChat  `yaml:"googlechat"`
	BlueBubbles *BlueBubbles `yaml:"bluebubbles"`
	LINE        *LINE        `yaml:"line"`
	Mattermost  *Mattermost  `yaml:"mattermost"`
	IRC         *IRC         `yaml:"irc"`
	Ntfy        *Ntfy        `yaml:"ntfy"`
	// whatsappcloud, messenger, instagram, feishu, dingtalk, wecom, zulip, rocketchat, nextcloudtalk, twitch (platforms2.go).
	Platforms2 `yaml:",inline"`
}

type Transcription struct {
	Provider string `yaml:"provider"` // a provider of type openai
	Model    string `yaml:"model"`
	Prompt   string `yaml:"prompt"` // vocabulary hints: names and terms to spell correctly
}

// MCPServer is one Model Context Protocol server whose tools agents can use
// by listing the toolset "mcp:<name>". It is either a command Habakkuk spawns
// (stdio) or a URL it connects to (Streamable HTTP). Secrets are named, never
// stored: env and headers map to variables in Habakkuk's environment or
// $HABAKKUK_HOME/.env.
type MCPServer struct {
	Command string            `yaml:"command"` // stdio: program to run
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`     // stdio: variable in the child -> variable holding its value
	URL     string            `yaml:"url"`     // http: Streamable HTTP endpoint
	Headers map[string]string `yaml:"headers"` // http: header -> variable holding its value, or a template like "Bearer ${VAR}"
	Risk    string            `yaml:"risk"`    // read | write | network | exec (default network)
	Approve []string          `yaml:"approve"` // tools that always need approval
	Allow   []string          `yaml:"allow"`   // expose only these tools (default all)
	Timeout time.Duration     `yaml:"timeout"` // per call (default 120s)
	// Auth "oauth" signs in with the MCP authorization flow (habakkuk mcp
	// login NAME); url servers only. The fields below tune it.
	Auth            string   `yaml:"auth"`
	ClientID        string   `yaml:"client_id"`         // oauth: a pre-registered client (default: dynamic registration)
	ClientSecretEnv string   `yaml:"client_secret_env"` // oauth: variable holding that client's secret, if it has one
	Scopes          []string `yaml:"scopes"`            // oauth: scopes to request (default: what the server asks for)
	CallbackPort    int      `yaml:"callback_port"`     // oauth: fixed loopback port for the sign-in redirect (default: any free port)
}

type Workers struct {
	Concurrency int           `yaml:"concurrency"`
	Lease       time.Duration `yaml:"lease"`
}

type Retention struct {
	Days int `yaml:"days"` // transcripts and finished jobs older than this are pruned; 0 disables
}

// ModelRef is a parsed "<provider>/<model>" reference.
type ModelRef struct {
	Provider string
	Model    string
}

func (r ModelRef) String() string { return r.Provider + "/" + r.Model }

func ParseModelRef(s string) (ModelRef, error) {
	p, m, ok := strings.Cut(s, "/")
	if !ok || p == "" || m == "" {
		return ModelRef{}, fmt.Errorf("model %q: want <provider>/<model>", s)
	}
	return ModelRef{Provider: p, Model: m}, nil
}

// DefaultHome is $HABAKKUK_HOME, else ~/.habakkuk (%APPDATA%\Habakkuk on
// Windows).
func DefaultHome() string { return platform.DataDir() }

func Load(home string) (*Config, error) {
	path := filepath.Join(home, "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no config at %s yet: run `habakkuk setup` to make one (about a minute)", path)
		}
		return nil, err
	}
	cfg, err := parseHome(data, home, true)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.Home = home
	return cfg, nil
}

func Parse(data []byte) (*Config, error) { return ParseHome(data, "") }

// ParseHome parses a config that lives in home, merging in the plugins
// enabled there ("" merges none). Unknown keys are errors here; Load, which
// starts Habakkuk, only reports them (see migrate.go).
func ParseHome(data []byte, home string) (*Config, error) { return parseHome(data, home, false) }

func parseHome(data []byte, home string, lenient bool) (*Config, error) {
	var cfg Config
	report, err := decodeConfig(data, &cfg, lenient)
	if err != nil {
		return nil, err
	}
	cfg.Report = report
	defaulted := map[string]bool{} // agents on the default toolsets get plugin toolsets too
	for name, a := range cfg.Agents {
		if a != nil && len(a.Toolsets) == 0 {
			defaulted[name] = true
		}
	}
	cfg.applyDefaults()
	if home != "" {
		if err := cfg.mergePlugins(home, defaulted); err != nil {
			return nil, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Judge != nil {
		c.Judge.applyDefaults()
	}
	c.applyProviderDefaults()
	if c.Workers.Concurrency == 0 {
		c.Workers.Concurrency = 4
	}
	if c.Workers.Lease == 0 {
		c.Workers.Lease = 45 * time.Second
	}
	if c.Retention.Days == 0 {
		c.Retention.Days = 90
	}
	c.applyHTTPDefaults()
	c.applyMediaDefaults()
	c.applyMemoryDefaults()
	c.applyOpsDefaults()
	c.applyIntegrationDefaults()
	c.applyHomeDefaults()
	for _, ch := range c.Channels {
		c.applyPlatformDefaults(ch)
		c.applyPlatform2Defaults(ch)
		if ch.Groups == "" {
			ch.Groups = "mention"
		}
		if ch.SessionIdle == 0 && ch.Type != "web" { // web conversations are picked by id
			ch.SessionIdle = 24 * time.Hour
		}
		if ch.Agent == "" {
			if _, ok := c.Agents["habakkuk"]; ok {
				ch.Agent = "habakkuk"
			} else if len(c.Agents) == 1 {
				ch.Agent = c.AgentNames()[0]
			}
		}
	}
	for _, m := range c.MCP {
		if m == nil {
			continue
		}
		if m.Risk == "" {
			m.Risk = "network"
		}
		if m.Timeout == 0 {
			m.Timeout = 120 * time.Second
		}
	}
	for _, a := range c.Agents {
		if a.MaxTokens == 0 {
			a.MaxTokens = 64000
		}
		if a.FastModel != "" && a.Routing == "" {
			a.Routing = "auto"
		}
		if a.EscalateAfter == 0 {
			a.EscalateAfter = 4
		}
		if a.MaxIterations == 0 {
			a.MaxIterations = 200
		}
		if a.Web == nil {
			on := true
			a.Web = &on
		}
		if a.Permission == "" {
			a.Permission = "workspace"
		}
		if a.CompactAt == 0 {
			a.CompactAt = 150000
			if ref, err := ParseModelRef(a.Model); err == nil && c.Providers[ref.Provider] != nil &&
				c.Providers[ref.Provider].Type == "anthropic" {
				a.CompactAt = 600000
			}
		}
		if a.Approvals == "" {
			a.Approvals = "human"
		}
		if len(a.Toolsets) == 0 {
			a.Toolsets = []string{"core", "memory", "skills", "schedule", "delegate", "board"}
		}
		a.applySandboxDefaults()
	}
}

func (c *Config) Validate() error {
	var errs []error
	if c.Version != CurrentVersion {
		errs = append(errs, fmt.Errorf("version %d is not supported (want %d); run `habakkuk config migrate`, or update Habakkuk if this config is from a newer release", c.Version, CurrentVersion))
	}
	errs = append(errs, c.validateProviders()...)
	if c.Judge != nil {
		errs = append(errs, c.Judge.validate()...)
	}
	if len(c.Agents) == 0 {
		errs = append(errs, errors.New("no agents configured"))
	}
	for name, a := range c.Agents {
		models := append([]string{a.Model}, a.Fallbacks...)
		if a.ReviewerModel != "" {
			models = append(models, a.ReviewerModel)
		}
		if a.FastModel != "" {
			models = append(models, a.FastModel)
		}
		for _, m := range models {
			ref, err := ParseModelRef(m)
			if err != nil {
				errs = append(errs, fmt.Errorf("agent %s: %w", name, err))
				continue
			}
			if _, ok := c.Providers[ref.Provider]; !ok {
				errs = append(errs, fmt.Errorf("agent %s: unknown provider %q in %q", name, ref.Provider, m))
			}
		}
		switch a.Permission {
		case "read-only", "ask", "workspace", "full":
		default:
			errs = append(errs, fmt.Errorf("agent %s: permission %q must be read-only, ask, workspace or full", name, a.Permission))
		}
		switch a.Approvals {
		case "human", "reviewer", "reviewer_then_human":
		default:
			errs = append(errs, fmt.Errorf("agent %s: approvals %q must be human, reviewer or reviewer_then_human", name, a.Approvals))
		}
		switch a.Routing {
		case "", "auto", "fast", "smart":
			if a.Routing != "" && a.FastModel == "" {
				errs = append(errs, fmt.Errorf("agent %s: routing %q needs a fast_model", name, a.Routing))
			}
		default:
			errs = append(errs, fmt.Errorf("agent %s: routing %q must be auto, fast or smart", name, a.Routing))
		}
		if a.EscalateAfter < 1 {
			errs = append(errs, fmt.Errorf("agent %s: escalate_after must be at least 1", name))
		}
		switch a.Effort {
		case "", "low", "medium", "high", "xhigh", "max":
		default:
			errs = append(errs, fmt.Errorf("agent %s: effort %q is not a known level", name, a.Effort))
		}
		if a.CompactAt != -1 && a.CompactAt < 1000 {
			errs = append(errs, fmt.Errorf("agent %s: compact_at must be at least 1000 tokens, or -1 to turn compaction off", name))
		}
		for _, ts := range a.Toolsets {
			if server, ok := strings.CutPrefix(ts, "mcp:"); ok && c.MCP[server] == nil {
				errs = append(errs, fmt.Errorf("agent %s: toolset %q names no configured MCP server", name, ts))
			}
		}
		errs = append(errs, validateSandbox(name, a)...)
		errs = append(errs, a.validateSecurity(name)...)
	}
	for name, m := range c.MCP {
		errs = append(errs, validateMCP(name, m)...)
	}
	for name, ch := range c.Channels {
		errs = append(errs, c.validateChannel(name, ch)...)
	}
	errs = append(errs, c.validateHTTP()...)
	errs = append(errs, c.validateMCPServe()...)
	errs = append(errs, c.validateMedia()...)
	errs = append(errs, c.validateExtras()...)
	errs = append(errs, c.validateBrowser()...)
	errs = append(errs, c.validateComputer()...)
	errs = append(errs, c.validateMeeting()...)
	errs = append(errs, c.validateMemory()...)
	errs = append(errs, c.validateOps()...)
	errs = append(errs, c.validateSkills()...)
	errs = append(errs, c.validateIntegrations()...)
	errs = append(errs, c.validateNodes()...)
	errs = append(errs, c.validateHomes()...)
	errs = append(errs, c.validateBudget()...)
	if c.Timezone != "" {
		if _, err := time.LoadLocation(c.Timezone); err != nil {
			errs = append(errs, fmt.Errorf("timezone: %w", err))
		}
	}
	if t := c.Transcription; t != nil {
		if p, ok := c.Providers[t.Provider]; !ok || p.Type != "openai" {
			errs = append(errs, fmt.Errorf("transcription: provider %q must be a configured provider of type openai", t.Provider))
		}
		if t.Model == "" {
			errs = append(errs, errors.New("transcription: model is required"))
		}
	}
	return errors.Join(errs...)
}

func (c *Config) validateChannel(name string, ch *Channel) []error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("channel %s: "+format, append([]any{name}, args...)...))
	}
	switch ch.Type {
	case "telegram":
		if ch.TokenEnv == "" {
			bad("token_env is required for telegram")
		}
	case "slack":
		if ch.AppTokenEnv == "" || len(ch.BotTokensEnv) == 0 {
			bad("slack needs app_token_env and bot_tokens_env")
		}
	case "discord":
		if ch.TokenEnv == "" {
			bad("token_env is required for discord")
		}
	case "whatsappagent":
		if ch.TokenEnv == "" {
			bad("token_env is required for whatsappagent: the agent's API key from WhatsApp (Settings > Agents)")
		}
		if ch.APIURL != "" && !isHTTPURL(ch.APIURL) {
			bad("api_url %q must start with http:// or https://", ch.APIURL)
		}
	case "signal":
		if !strings.HasPrefix(ch.Account, "+") {
			bad("signal needs account, the bot's number in international format (+15551234567)")
		}
		if ch.APIURL != "" && !isHTTPURL(ch.APIURL) {
			bad("api_url %q must start with http:// or https://", ch.APIURL)
		}
	case "matrix":
		if !isHTTPURL(ch.Homeserver) || ch.TokenEnv == "" {
			bad("matrix needs homeserver (an http:// or https:// URL) and token_env")
		}
		if ch.Account != "" && (!strings.HasPrefix(ch.Account, "@") || !strings.Contains(ch.Account, ":")) {
			bad("account %q must be a Matrix user id like @habakkuk:example.org", ch.Account)
		}
	case "whatsapp", "web":
	case "email", "sms", "msteams", "googlechat", "bluebubbles", "line", "mattermost", "irc", "ntfy":
	case "whatsappcloud", "messenger", "instagram", "feishu", "dingtalk", "wecom", "zulip", "rocketchat", "nextcloudtalk", "twitch":
	default:
		bad("type %q must be telegram, slack, whatsapp, whatsappagent, web, discord, signal, matrix, email, sms, msteams, googlechat, bluebubbles, line, mattermost, irc or ntfy", ch.Type)
	}
	errs = append(errs, c.validatePlatform(name, ch)...)
	errs = append(errs, c.validatePlatform2(name, ch)...)
	if _, ok := c.Agents[ch.Agent]; !ok {
		bad("agent %q is not configured", ch.Agent)
	}
	for conv, agent := range ch.Routes {
		if _, ok := c.Agents[agent]; !ok {
			bad("route %s: agent %q is not configured", conv, agent)
		}
	}
	switch ch.Groups {
	case "off", "mention", "all":
	default:
		bad("groups %q must be off, mention or all", ch.Groups)
	}
	if ch.SessionIdle < 0 {
		bad("session_idle must not be negative")
	}
	return errs
}

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

var mcpServerName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

func validateMCP(name string, m *MCPServer) []error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("mcp server %s: "+format, append([]any{name}, args...)...))
	}
	if !mcpServerName.MatchString(name) {
		bad("name must be 1-32 letters, digits, _ or -")
	}
	if m == nil {
		bad("set command or url")
		return errs
	}
	switch {
	case (m.Command == "") == (m.URL == ""):
		bad("set exactly one of command or url")
	case m.Command != "" && len(m.Headers) > 0:
		bad("headers apply to url servers only")
	case m.URL != "" && (len(m.Args) > 0 || len(m.Env) > 0):
		bad("args and env apply to command servers only")
	case m.URL != "" && !strings.HasPrefix(m.URL, "http://") && !strings.HasPrefix(m.URL, "https://"):
		bad("url %q must start with http:// or https://", m.URL)
	}
	for k, v := range m.Env {
		if k == "" || v == "" {
			bad("env %q: name the child variable and the variable holding its value", k)
		}
	}
	for k, v := range m.Headers {
		if k == "" || v == "" {
			bad("header %q: name the header and the variable holding its value", k)
		}
	}
	switch m.Risk {
	case "read", "write", "network", "exec":
	default:
		bad("risk %q must be read, write, network or exec", m.Risk)
	}
	if m.Timeout < 0 {
		bad("timeout must not be negative")
	}
	return errs
}

// AgentFor picks the agent for a conversation: an exact route, then a route
// for the conversation's prefix (a Slack team), then the account default.
func (ch *Channel) AgentFor(conversation string) string {
	if a, ok := ch.Routes[conversation]; ok {
		return a
	}
	if prefix, _, ok := strings.Cut(conversation, "/"); ok {
		if a, ok := ch.Routes[prefix]; ok {
			return a
		}
	}
	return ch.Agent
}

// AgentDir holds an agent's persona and instruction files (SOUL.md, AGENTS.md).
func (c *Config) AgentDir(name string) string {
	return filepath.Join(c.Home, "agents", name)
}

// WorkspaceDir resolves an agent's workspace, expanding a leading "~/".
func (c *Config) WorkspaceDir(name string) string {
	a := c.Agents[name]
	ws := a.Workspace
	if ws == "" {
		return filepath.Join(c.AgentDir(name), "workspace")
	}
	if rest, ok := strings.CutPrefix(ws, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return ws
}

func (c *Config) AgentNames() []string {
	names := make([]string, 0, len(c.Agents))
	for n := range c.Agents {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

const Template = `version: 1

# Each provider names the environment variables holding its API keys.
# Several keys rotate automatically on rate limits and auth failures.
providers:
  anthropic:
    type: anthropic
    keys_env: [ANTHROPIC_API_KEY]
  openrouter:
    type: openai
    base_url: https://openrouter.ai/api/v1
    keys_env: [OPENROUTER_API_KEY]
  # Known services need only a name or a preset (see habakkuk models):
  # groq: {}
  # local:
  #   preset: ollama
  # Or sign in with a subscription (habakkuk auth login chatgpt|copilot):
  # chatgpt:
  #   auth: chatgpt

agents:
  habakkuk:
    model: anthropic/claude-opus-5-5
    fallbacks: [anthropic/claude-sonnet-5-5]
    effort: high
    permission: workspace   # read-only | ask | workspace | full
    workspace: ~/habakkuk-workspace
    # Who approves calls the permission mode does not allow outright (exec):
    # human | reviewer | reviewer_then_human. A reviewer is one model call that
    # allows routine work and denies destructive or exfiltrating actions, so
    # schedules and sub-agents can run without you.
    # approvals: reviewer_then_human
    # reviewer_model: anthropic/claude-sonnet-5-5   # default: the agent's own models
    # Where exec runs: none (here) | docker | ssh. Checkpoints snapshot the
    # workspace before each turn's changes (habakkuk rollback, /undo in chat).
    # sandbox: docker
    # docker: {image: debian:stable-slim, network: true, cpus: "2", memory: 2g}
    # ssh: {host: build.example.com, user: habakkuk, key_env: BUILD_SSH_KEY, workdir: /srv/habakkuk}
    # checkpoints: true
    # checkpoint_max_mb: 200
    # Home chat: where this agent tells you about work that started elsewhere
    # (delegated by another agent, a board card, a schedule, A2A, another
    # channel), as CHANNEL:CONVERSATION[:THREAD]; a whatsappagent channel
    # takes <channel>:creator. It also gives the agent the message_home tool.
    # report_home: off | results (default: a short result when such work
    # finishes) | all (also "Working on: ..." when it starts).
    # home: wagent:creator
    # report_home: results
    # This agent's spending limits in US dollars (see budget: below).
    # budget: {daily: 2, monthly: 40, per_turn: 0.50}
    # Safety (habakkuk security shows the effective policy). Messages from a
    # chat channel need approval for exec, process, write (outside the
    # workspace), computer and browser (private addresses), in every mode;
    # senders let in by allow_from "*" never get them. After reading outside
    # content (web, email, webhooks) exec and outside writes need a human,
    # even in full mode.
    # trust: channel            # channel | full (your own chat messages act like the CLI)
    # channel_allow: [write]    # or lift only some: exec, process, write, computer, browser
    # allow_tainted_exec: false

# Messaging channels. Uncomment one, put its tokens in .env, then run
# ` + "`habakkuk serve`" + `. Unknown senders get a pairing code for ` + "`habakkuk pair`" + `.
# channels:
#   telegram:
#     type: telegram
#     token_env: TELEGRAM_BOT_TOKEN
#     allow_from: []        # Telegram user ids; or approve senders with habakkuk pair
#   slack:
#     type: slack
#     app_token_env: SLACK_APP_TOKEN
#     bot_tokens_env: [SLACK_BOT_TOKEN]   # one per workspace the app is installed in
#   whatsapp:
#     type: whatsapp        # link with: habakkuk whatsapp link -phone <number>
#   wagent:
#     type: whatsappagent   # an agent made in WhatsApp under Settings > Agents; talks only to its creator
#     token_env: WHATSAPP_AGENT_KEY   # the agent's API key (Chat info > API key); schedules can deliver to wagent:creator
#   discord:
#     type: discord
#     token_env: DISCORD_BOT_TOKEN   # enable the Message Content intent in the developer portal
#   signal:
#     type: signal          # needs a running signal-cli daemon in HTTP mode (see the README)
#     account: "+15551234567"
#     api_url: http://127.0.0.1:8080
#   matrix:
#     type: matrix          # no end-to-end encryption: use unencrypted rooms
#     homeserver: https://matrix.example.org
#     token_env: MATRIX_ACCESS_TOKEN
#     account: "@habakkuk:example.org"
# A turn that runs past 8 seconds shows one "working" message that is edited
# with each tool step and removed when the reply arrives (Telegram, Slack).
# Set progress: off on a channel to keep only the typing indicator.
#   mail:
#     type: email           # IMAP in (IDLE or polling), SMTP out; each thread is a conversation
#     allow_from: [you@example.com]
#     email:
#       address: habakkuk@example.com
#       imap: imap.example.com:993
#       smtp: smtp.example.com:587
#       password_env: HABAKKUK_MAIL_PASSWORD   # an app password
#   mattermost:
#     type: mattermost
#     mattermost: {url: "https://chat.example.com", token_env: MATTERMOST_BOT_TOKEN}
#   irc:
#     type: irc
#     irc: {server: "irc.libera.chat:6697", nick: habakkuk-bot, password_env: IRC_PASSWORD, channels: ["#habakkuk"]}
#   ntfy:
#     type: ntfy            # replies and notifications to a topic; subscribe reads one too
#     allow_from: [ntfy]    # ntfy has no sender identity: protect the topic
#     ntfy: {topic: habakkuk-out, subscribe: habakkuk-in, token_env: NTFY_TOKEN}
#   zulip:
#     type: zulip           # a generic bot; streams are conversations, topics threads
#     zulip: {url: "https://example.zulipchat.com", email: habakkuk-bot@example.zulipchat.com, api_key_env: ZULIP_API_KEY}
#   rocketchat:
#     type: rocketchat      # a bot user with a personal access token
#     rocketchat: {url: "https://chat.example.com", user_id: aobEdbYhXfu5hkeqG, token_env: ROCKETCHAT_TOKEN}
#   twitch:
#     type: twitch          # public chat: answers only messages naming the bot
#     twitch: {login: habakkuk_bot, token_env: TWITCH_TOKEN, channels: [mystream]}
#
# These receive webhooks at <http.public_url>/channels/<name>, so they need
# the http section below with public_url set (habakkuk doctor prints the URLs).
#   sms:
#     type: sms             # Twilio
#     sms: {account_sid: ACxxxxxxxx, auth_token_env: TWILIO_AUTH_TOKEN, from: "+15551234567"}
#   teams:
#     type: msteams         # Azure Bot; messaging endpoint <public_url>/channels/teams
#     msteams: {app_id: 00000000-0000-0000-0000-000000000000, app_password_env: TEAMS_APP_SECRET}
#   gchat:
#     type: googlechat      # Chat app with an HTTP endpoint URL
#     googlechat: {audience: "123456789012", credentials_env: GOOGLE_CHAT_KEY}   # project number; key JSON or its path
#   imessage:
#     type: bluebubbles     # webhook URL: <public_url>/channels/imessage?secret=<BLUEBUBBLES_HOOK_SECRET>
#     bluebubbles: {url: "http://mac.local:1234", password_env: BLUEBUBBLES_PASSWORD, webhook_secret_env: BLUEBUBBLES_HOOK_SECRET}
#   line:
#     type: line
#     line: {channel_secret_env: LINE_CHANNEL_SECRET, access_token_env: LINE_ACCESS_TOKEN}
#   wacloud:
#     type: whatsappcloud   # WhatsApp Business Cloud API; outside 24h only a template goes out
#     whatsappcloud: {phone_number_id: "123456789012345", access_token_env: WHATSAPP_TOKEN, app_secret_env: META_APP_SECRET, verify_token_env: META_VERIFY_TOKEN}
#   fbpage:
#     type: messenger       # a Facebook Page's inbox, with a Page access token
#     messenger: {access_token_env: FB_PAGE_TOKEN, app_secret_env: META_APP_SECRET, verify_token_env: META_VERIFY_TOKEN}
#   insta:
#     type: instagram       # the Instagram account linked to the Page
#     instagram: {access_token_env: FB_PAGE_TOKEN, app_secret_env: META_APP_SECRET, verify_token_env: META_VERIFY_TOKEN}
#   feishu:
#     type: feishu          # domain: lark for open.larksuite.com
#     feishu: {app_id: cli_a1b2c3d4e5, app_secret_env: FEISHU_APP_SECRET, verification_token_env: FEISHU_VERIFICATION_TOKEN, encrypt_key_env: FEISHU_ENCRYPT_KEY}
#   dingtalk:
#     type: dingtalk        # robot in HTTP mode
#     dingtalk: {client_id: dingabcdef123456, client_secret_env: DINGTALK_CLIENT_SECRET}
#   wecom:
#     type: wecom           # add the server's IP to the app's trusted IPs
#     wecom: {corp_id: ww0123456789abcdef, agent_id: 1000002, secret_env: WECOM_SECRET, token_env: WECOM_TOKEN, aes_key_env: WECOM_AES_KEY}
#   talk:
#     type: nextcloudtalk   # occ talk:bot:install ... <public_url>/channels/talk; groups: all to answer without @Habakkuk
#     nextcloudtalk: {url: "https://cloud.example.com", secret_env: TALK_BOT_SECRET}

# HTTP: a chat page at /, a JSON API with Server-Sent Events, and webhooks.
# The chat is the channel account "web". Without token_env it listens on
# loopback only; a public address needs a token.
# http:
#   listen: 127.0.0.1:8484
#   token_env: HABAKKUK_HTTP_TOKEN      # Authorization: Bearer <token>
#   public_url: https://habakkuk.example.com   # how platforms reach this server (proxy or tunnel)
#   webhooks:
#     alerts:                         # POST /hooks/alerts
#       agent: habakkuk
#       secret_env: ALERTS_SECRET     # X-Habakkuk-Signature: sha256=<HMAC of body>, or a bearer secret
#       prompt: "Alert {{.title}} fired. Look into it:\n{{.Body}}"
#       deliver: telegram:123456789   # optional; default web conversation hook-alerts

# Browser: agents that list the toolset "browser" get a headless Chrome or
# Chromium (found on PATH) with a profile under $HABAKKUK_HOME/browser/<agent>,
# or tabs in a Chrome you started with --remote-debugging-port (mode connect).
# Loopback and private addresses are refused unless listed in allow_private.
# browser:
#   mode: launch                      # launch | connect
#   # cdp_url: http://127.0.0.1:9222  # connect only
#   headless: true
#   allowed_urls: []                  # only these pages, when set
#   blocked_urls: ["*.doubleclick.net"]
#   allow_private: ["localhost:3000"]
#   idle_timeout: 10m
#   timeout: 60s                      # per call

# Computer: agents that list the toolset "computer" see a desktop through
# screenshots and drive its mouse and keyboard. Every action is exec risk.
# linux drives this machine's X11 (xdotool) or Wayland (ydotool) screen,
# macos this Mac's (grant Screen Recording and Accessibility), xvfb gives
# each agent a private virtual desktop (habakkuk computer view shows how to
# watch it). habakkuk doctor checks the commands each backend needs.
# computer:
#   backend: xvfb                     # linux | macos | xvfb
#   screen: 1280x800                  # xvfb only
#   max_actions: 150                  # per turn
#   confirm_apps: ["1Password", "Terminal"]   # always ask a human first

# Schedules use this timezone unless an entry says otherwise. Spending
# limits reset at midnight and on the 1st of the month in it.
# timezone: America/Toronto

# Spending limits in US dollars, priced like habakkuk usage. A model call is
# refused once a limit is used up; the turn ends with a message saying when
# it resets. A warning goes to the chat at 80%. Each agent can have its own
# (agents.<name>.budget, same keys except unknown_price). Changes apply
# without a restart. See them with habakkuk usage or /budget.
# budget:
#   daily: 5                 # the whole install, per day
#   monthly: "100.00"        # per calendar month
#   per_turn: 1.50           # one turn, with its tool loop and sub-agents
#   unknown_price: allow     # allow counts unpriced models as $0; block refuses them

# Voice notes: any OpenAI-compatible speech-to-text endpoint.
# transcription:
#   provider: openai          # a provider with type: openai
#   model: gpt-4o-mini-transcribe
#   prompt: "Names and words to spell right: Habakkuk, Toronto"

# Speech: the speak tool, and voice replies per channel with
# voice_replies: off | mirror | always (and voice_text: false to drop the text).
# Providers: openai, elevenlabs, or edge (free, no key). Voice notes are OGG/Opus;
# mp3 output is converted with ffmpeg when it is installed.
# tts:
#   provider: openai
#   key_env: OPENAI_API_KEY
#   model: gpt-4o-mini-tts
#   voice: alloy

# Image generation (image_generate): openai, gemini or fal.
# images:
#   provider: openai
#   key_env: OPENAI_API_KEY
#   model: gpt-image-2

# Web search for agents whose model has no hosted search (non-Anthropic):
# brave, tavily, exa, searxng (url: your instance) or duckduckgo (no key,
# best effort). Off until set: no search service is contacted by default.
# search:
#   provider: brave
#   key_env: BRAVE_API_KEY

# MCP servers lend their tools to agents that list the toolset "mcp:<name>",
# e.g. toolsets: [core, "mcp:github"]. Values in env and headers name
# variables in the environment or .env; a command server's process gets
# PATH, HOME and these, nothing else.
# mcp_servers:
#   github:
#     command: github-mcp-server
#     args: [stdio]
#     env: {GITHUB_PERSONAL_ACCESS_TOKEN: GITHUB_TOKEN}
#     risk: network         # read | write | network | exec
#     approve: [create_pull_request]   # always ask before these
#   search:
#     url: https://mcp.example.com/mcp
#     headers: {Authorization: SEARCH_MCP_AUTH}
#     allow: [web_search]   # expose only these tools
#     timeout: 60s

# Hooks run a command on an event, with the event as JSON on stdin and
# HABAKKUK_EVENT set. Events: turn_start, turn_end, tool_before, tool_after,
# message_in, message_out, session_new, schedule_run. match is a regular
# expression on the tool, agent, channel or schedule name. A tool_before hook
# that exits non-zero blocks the call; its stdout is the reason.
# hooks:
#   - event: tool_before
#     match: ^exec$
#     command: ~/.habakkuk/hooks/check-exec.sh
#     timeout: 5s
#   - event: turn_end
#     command: cat >> ~/.habakkuk/turns.jsonl

# Memory: semantic search over notes (embeddings from a provider of type
# openai, including local ones like preset ollama, or gemini) and a background
# review that turns idle conversations into notes and USER.md updates, plus a
# nightly pass that merges near-duplicates and archives stale notes.
# memory:
#   embeddings: {provider: openai, model: text-embedding-3-small}
#   review: true
#   review_model: anthropic/claude-sonnet-5-5  # default: each agent's last fallback
#   review_idle: 15m
#   nightly: "03:30"        # or off
#   stale_days: 365         # -1 never archives
#   history_days: 90        # changes kept for habakkuk memory history/undo; -1 keeps all
#
# Skills from outside: habakkuk skills install github:owner/repo[/path][@ref],
# an https URL, a local path, or a name from a registry index. The curator
# merges near-duplicate skills agents wrote and archives unused ones (with
# backups; habakkuk skills restore NAME). Plugins (habakkuk plugins install,
# enable) add MCP servers, skills, hooks and commands without editing this
# file; see docs/tools/plugins.md. habakkuk mcp catalog lists ready-made MCP servers.
# skills:
#   registries:
#     - {name: team, url: "https://skills.example.com/index.json"}
#   curate: weekly          # off | daily | weekly | monthly
#   archive_after_days: 90

workers:
  concurrency: 4
  lease: 45s

retention:
  days: 90
`
