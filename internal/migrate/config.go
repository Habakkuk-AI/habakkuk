package migrate

import (
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/config/yamledit"
)

// config.yaml is edited as a node tree with the same helpers habakkuk setup
// uses, so comments and everything the import does not touch stay as they
// are.

const newConfigHeader = `Habakkuk settings, written by habakkuk migrate. Edit this file or run
habakkuk setup to change them; every option is explained in the docs, under
Configuration. Keys and tokens are kept in .env next to this file, never here.`

// defaultToolsets are an agent's toolsets when config.yaml lists none; an
// agent given MCP toolsets lists them explicitly with these.
var defaultToolsets = []string{"core", "memory", "skills", "schedule", "delegate", "board"}

func newConfigDoc() *yaml.Node {
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	yamledit.Set(root, "version", yamledit.Int(config.CurrentVersion), "")
	return &yaml.Node{Kind: yaml.DocumentNode, HeadComment: newConfigHeader, Content: []*yaml.Node{root}}
}

func mapNode() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"} }

func setProvider(root *yaml.Node, p *Provider) {
	providers := yamledit.EnsureMap(root, "providers", "Where the models come from (habakkuk models lists what each serves).")
	n := mapNode()
	switch {
	case p.Auth != "":
		yamledit.Set(n, "auth", yamledit.Str(p.Auth), "a subscription sign-in (habakkuk auth login "+p.Auth+")")
	case p.Preset != "":
		yamledit.Set(n, "preset", yamledit.Str(p.Preset), "")
	default:
		yamledit.Set(n, "type", yamledit.Str(p.Type), "")
	}
	if p.BaseURL != "" {
		yamledit.Set(n, "base_url", yamledit.Str(p.BaseURL), "")
	}
	if len(p.KeysEnv) > 0 {
		yamledit.Set(n, "keys_env", yamledit.FlowList(p.KeysEnv...), "the variable in .env that holds the key")
	}
	yamledit.Set(providers, p.Name, n, "")
}

// setAgent writes a new agent, or with replace its model and fallbacks;
// toolsets adds the MCP toolsets it lacks.
func setAgent(root *yaml.Node, a *Agent, replace bool, have []string) {
	agents := yamledit.EnsureMap(root, "agents", "Your agents. permission: read-only | ask | workspace | full.")
	n := yamledit.Get(agents, a.Name)
	fresh := n == nil || n.Kind != yaml.MappingNode
	if fresh {
		n = mapNode()
		yamledit.Set(agents, a.Name, n, "")
	}
	if fresh || replace {
		yamledit.Set(n, "model", yamledit.Str(a.Model), "")
		if len(a.Fallbacks) > 0 {
			yamledit.Set(n, "fallbacks", yamledit.FlowList(a.Fallbacks...), "")
		}
	}
	if fresh {
		yamledit.Set(n, "permission", yamledit.Str("workspace"), "it may change files only in its workspace")
		if a.Workspace != "" {
			yamledit.Set(n, "workspace", yamledit.Str(a.Workspace), "")
		}
	}
	if ts := toolsets(have, a.MCP); ts != nil {
		yamledit.Set(n, "toolsets", yamledit.FlowList(ts...), "")
	}
}

// toolsets is have plus the MCP toolsets of servers, or nil when nothing
// needs adding.
func toolsets(have, servers []string) []string {
	if len(servers) == 0 {
		return nil
	}
	out := slices.Clone(have)
	if len(out) == 0 {
		out = slices.Clone(defaultToolsets)
	}
	added := false
	for _, s := range servers {
		if t := "mcp:" + s; !slices.Contains(out, t) {
			out, added = append(out, t), true
		}
	}
	if !added {
		return nil
	}
	return out
}

func setMCP(root *yaml.Node, m *MCPServer) {
	servers := yamledit.EnsureMap(root, "mcp_servers", "MCP servers; an agent uses one through the toolset mcp:<name>.")
	n := mapNode()
	if m.Command != "" {
		yamledit.Set(n, "command", yamledit.Str(m.Command), "")
		if len(m.Args) > 0 {
			yamledit.Set(n, "args", yamledit.FlowList(m.Args...), "")
		}
		if len(m.Env) > 0 {
			yamledit.Set(n, "env", yamledit.FlowMap(sortedKeys(m.Env), m.Env), "child variable: variable in .env")
		}
	} else {
		yamledit.Set(n, "url", yamledit.Str(m.URL), "")
		if len(m.Headers) > 0 {
			yamledit.Set(n, "headers", yamledit.FlowMap(sortedKeys(m.Headers), m.Headers), "header: variable in .env")
		}
		if m.OAuth {
			yamledit.Set(n, "auth", yamledit.Str("oauth"), "sign in with habakkuk mcp login "+m.Name)
		}
	}
	yamledit.Set(servers, m.Name, n, "")
}

func setChannel(root *yaml.Node, c *Channel) {
	chans := yamledit.EnsureMap(root, "channels", "Where you talk to your agents; habakkuk serve connects them.")
	n := mapNode()
	note := ""
	if c.Link {
		note = "a linked device; link it with habakkuk whatsapp link -account " + c.Name
	}
	yamledit.Set(n, "type", yamledit.Str(c.Type), note)
	yamledit.Set(n, "agent", yamledit.Str(c.Agent), "")
	if c.TokenEnv != "" {
		yamledit.Set(n, "token_env", yamledit.Str(c.TokenEnv), "")
	}
	if c.AppTokenEnv != "" {
		yamledit.Set(n, "app_token_env", yamledit.Str(c.AppTokenEnv), "")
		yamledit.Set(n, "bot_tokens_env", yamledit.FlowList(c.BotTokensEnv...), "")
	}
	if c.Homeserver != "" {
		yamledit.Set(n, "homeserver", yamledit.Str(c.Homeserver), "")
	}
	if c.Account != "" {
		yamledit.Set(n, "account", yamledit.Str(c.Account), "")
	}
	if c.APIURL != "" {
		yamledit.Set(n, "api_url", yamledit.Str(c.APIURL), "")
	}
	allowNote := ""
	if len(c.AllowFrom) == 0 {
		allowNote = "approve senders with habakkuk pair CODE"
	}
	yamledit.Set(n, "allow_from", yamledit.FlowList(c.AllowFrom...), allowNote)
	if len(c.Routes) > 0 {
		r := mapNode()
		for _, k := range sortedKeys(c.Routes) {
			yamledit.Set(r, k, yamledit.Str(c.Routes[k]), "")
		}
		yamledit.Set(n, "routes", r, "conversation: agent")
	}
	yamledit.Set(chans, c.Name, n, "")
}

// sameChannel compares an imported channel with a configured one on what
// the import sets.
func sameChannel(c *Channel, have *config.Channel) bool {
	return have.Type == c.Type && have.TokenEnv == c.TokenEnv && have.AppTokenEnv == c.AppTokenEnv &&
		slices.Equal(have.BotTokensEnv, c.BotTokensEnv) && have.Account == c.Account &&
		strings.TrimRight(have.Homeserver, "/") == strings.TrimRight(c.Homeserver, "/")
}

// sameServer compares an imported MCP server with a configured one.
func sameServer(m *MCPServer, have *config.MCPServer) bool {
	return sameServerAs(m, have, have.Command, have.Args)
}

// sameServerAs is sameServer with the configured command and arguments
// given, for comparing them after their references are rewritten.
func sameServerAs(m *MCPServer, have *config.MCPServer, command string, args []string) bool {
	return command == m.Command && slices.Equal(args, m.Args) && have.URL == m.URL &&
		fmt.Sprint(have.Env) == fmt.Sprint(m.Env) && fmt.Sprint(have.Headers) == fmt.Sprint(m.Headers) &&
		(have.Auth == "oauth") == m.OAuth
}

// moveMCP changes only the command and arguments of a configured server,
// keeping everything else written for it.
func moveMCP(root *yaml.Node, m *MCPServer) {
	n := yamledit.Get(yamledit.Get(root, "mcp_servers"), m.Name)
	if n == nil || n.Kind != yaml.MappingNode {
		setMCP(root, m)
		return
	}
	yamledit.Set(n, "command", yamledit.Str(m.Command), "")
	if len(m.Args) > 0 {
		yamledit.Set(n, "args", yamledit.FlowList(m.Args...), "")
	}
}

// setWorkspace changes a configured agent's workspace.
func setWorkspace(root *yaml.Node, name, ws string) {
	if n := yamledit.Get(yamledit.Get(root, "agents"), name); n != nil && n.Kind == yaml.MappingNode {
		yamledit.Set(n, "workspace", yamledit.Str(ws), "")
	}
}
