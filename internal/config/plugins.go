package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"gopkg.in/yaml.v3"
)

// PluginManifestName is the file that makes a folder a plugin.
const PluginManifestName = "habakkuk-plugin.yaml"

// PluginManifest is a plugin's habakkuk-plugin.yaml. Every section is
// optional. See docs/tools/plugins.md.
type PluginManifest struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Version     string `yaml:"version"`
	// MCPServers are servers the plugin brings, in the format of
	// mcp_servers. A server named like the plugin, or the only one, becomes
	// toolset "mcp:<plugin>"; others "mcp:<plugin>-<server>".
	MCPServers map[string]*MCPServer `yaml:"mcp_servers"`
	// Skills is a folder of skill folders (default "skills", when present).
	Skills string `yaml:"skills"`
	// Hooks are run like the hooks in config.yaml, from the plugin folder.
	Hooks []*Hook `yaml:"hooks"`
	// Commands are chat slash commands: /<name>.
	Commands map[string]*PluginCommand `yaml:"commands"`
}

// PluginCommand is a slash command a plugin adds: either a prompt the agent
// runs as a message, or a script whose output is the reply.
type PluginCommand struct {
	Description string `yaml:"description"`
	// Prompt is sent to the agent; {{args}} is replaced by what follows
	// the command (appended when the prompt has no {{args}}).
	Prompt string `yaml:"prompt"`
	// Run is a shell command run in the plugin folder, with the command's
	// arguments as $1... (pass them on with "$@") and in HABAKKUK_ARGS; its
	// output is the reply.
	Run     string        `yaml:"run"`
	Timeout time.Duration `yaml:"timeout"` // for run (default 60s, at most 10m)
}

// Plugin is an enabled plugin merged into the configuration.
type Plugin struct {
	Name      string
	Dir       string
	Manifest  *PluginManifest
	SkillsDir string   // "" when it has none
	Servers   []string // the MCP servers it added
	Problems  []string // parts left out, and why
}

var (
	rePluginName  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	reCommandName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
)

// ValidPluginName checks a plugin name: lowercase letters, digits and
// hyphens, at most 64.
func ValidPluginName(name string) error {
	if !rePluginName.MatchString(name) {
		return fmt.Errorf("invalid plugin name %q: use lowercase letters, digits and hyphens (at most 64), starting with a letter or digit", name)
	}
	return nil
}

// ParsePluginManifest decodes and checks a manifest. Unknown keys are
// errors, as in config.yaml.
func ParsePluginManifest(data []byte) (*PluginManifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var m PluginManifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", PluginManifestName, err)
	}
	var errs []error
	if err := ValidPluginName(m.Name); err != nil {
		errs = append(errs, err)
	}
	for _, key := range slices.Sorted(maps.Keys(m.MCPServers)) {
		s := m.MCPServers[key]
		if s != nil {
			mcpDefaults(s)
		}
		errs = append(errs, validateMCP(PluginServerName(m.Name, key, len(m.MCPServers)), s)...)
	}
	if m.Skills != "" {
		if _, err := relInside(m.Skills); err != nil {
			errs = append(errs, fmt.Errorf("skills: %w", err))
		}
	}
	errs = append(errs, (&Config{Hooks: m.Hooks}).validateExtras()...)
	for _, name := range slices.Sorted(maps.Keys(m.Commands)) {
		c := m.Commands[name]
		bad := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("commands.%s: "+format, append([]any{name}, args...)...))
		}
		if !reCommandName.MatchString(name) {
			bad("name must be lowercase letters, digits, - or _ (at most 32)")
		}
		switch {
		case c == nil:
			bad("set prompt or run")
		case (c.Prompt == "") == (c.Run == ""):
			bad("set exactly one of prompt or run")
		case c.Timeout < 0 || c.Timeout > 10*time.Minute:
			bad("timeout must be between 0 and 10m")
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &m, nil
}

// LoadPluginManifest reads dir/habakkuk-plugin.yaml.
func LoadPluginManifest(dir string) (*PluginManifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, PluginManifestName))
	if err != nil {
		return nil, err
	}
	return ParsePluginManifest(data)
}

// PluginServerName is the MCP server (and toolset "mcp:<name>") a plugin's
// server key becomes.
func PluginServerName(plugin, key string, servers int) string {
	if key == plugin || servers == 1 {
		return plugin
	}
	return plugin + "-" + key
}

func mcpDefaults(m *MCPServer) {
	if m.Risk == "" {
		m.Risk = "network"
	}
	if m.Timeout == 0 {
		m.Timeout = 120 * time.Second
	}
}

func relInside(p string) (string, error) {
	c := path.Clean(filepath.ToSlash(p))
	if path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%q must be a folder inside the plugin", p)
	}
	return c, nil
}

// PluginsDir holds installed plugins.
func PluginsDir(home string) string { return filepath.Join(home, "plugins") }

// enabledPlugins reads the plugin lock file ($HABAKKUK_HOME/plugins/.lock.json,
// written by `habakkuk plugins`) for the plugins that are enabled and trusted.
func enabledPlugins(home string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(PluginsDir(home), ".lock.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lock struct {
		Packages map[string]struct {
			Enabled bool `json:"enabled"`
			Trusted bool `json:"trusted"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("plugins/.lock.json: %w", err)
	}
	var out []string
	for _, name := range slices.Sorted(maps.Keys(lock.Packages)) {
		if p := lock.Packages[name]; p.Enabled && p.Trusted && ValidPluginName(name) == nil {
			out = append(out, name)
		}
	}
	return out, nil
}

// mergePlugins adds what enabled plugins declare to the configuration,
// without touching config.yaml: their MCP servers (and, for agents that
// use the default toolsets, those servers' toolsets), their hooks (run from
// the plugin folder) and their skill folders and commands (in Plugins). A
// plugin that cannot load, or a part that clashes with config.yaml, is
// left out and noted in its Problems; it never stops Habakkuk from starting.
func (c *Config) mergePlugins(home string, defaulted map[string]bool) error {
	names, err := enabledPlugins(home)
	if err != nil {
		return err
	}
	for _, name := range names {
		dir := filepath.Join(PluginsDir(home), name)
		p := &Plugin{Name: name, Dir: dir}
		c.Plugins = append(c.Plugins, p)
		m, err := LoadPluginManifest(dir)
		if err != nil {
			p.Problems = append(p.Problems, err.Error())
			continue
		}
		p.Manifest = m
		expand := func(s string) string { return strings.ReplaceAll(s, "${PLUGIN_DIR}", dir) }
		for _, key := range slices.Sorted(maps.Keys(m.MCPServers)) {
			server := PluginServerName(name, key, len(m.MCPServers))
			if c.MCP[server] != nil {
				p.Problems = append(p.Problems, fmt.Sprintf("MCP server %s is already in config.yaml; the plugin's is left out", server))
				continue
			}
			s := *m.MCPServers[key]
			s.Command = expand(s.Command)
			if strings.HasPrefix(s.Command, "./") || strings.HasPrefix(s.Command, "../") {
				s.Command = filepath.Join(dir, s.Command)
			}
			s.Args = slices.Clone(s.Args)
			for i := range s.Args {
				s.Args[i] = expand(s.Args[i])
			}
			if c.MCP == nil {
				c.MCP = map[string]*MCPServer{}
			}
			c.MCP[server] = &s
			p.Servers = append(p.Servers, server)
			for _, agent := range slices.Sorted(maps.Keys(defaulted)) {
				if a := c.Agents[agent]; a != nil && !slices.Contains(a.Toolsets, "mcp:"+server) {
					a.Toolsets = append(a.Toolsets, "mcp:"+server)
				}
			}
		}
		for _, h := range m.Hooks {
			hook := *h
			hook.Command = platform.CommandInDir(dir, expand(h.Command))
			c.Hooks = append(c.Hooks, &hook)
		}
		skills := m.Skills
		if skills == "" {
			skills = "skills"
		}
		if rel, err := relInside(skills); err == nil {
			if info, err := os.Stat(filepath.Join(dir, rel)); err == nil && info.IsDir() {
				p.SkillsDir = filepath.Join(dir, rel)
			} else if m.Skills != "" {
				p.Problems = append(p.Problems, fmt.Sprintf("skills folder %s is missing", m.Skills))
			}
		}
	}
	return nil
}

// PluginSkillDirs are the skill folders of enabled plugins.
func (c *Config) PluginSkillDirs() []string {
	var out []string
	for _, p := range c.Plugins {
		if p.SkillsDir != "" {
			out = append(out, p.SkillsDir)
		}
	}
	return out
}

// PluginProblems lists what enabled plugins could not contribute.
func (c *Config) PluginProblems() []string {
	var out []string
	for _, p := range c.Plugins {
		for _, msg := range p.Problems {
			out = append(out, "plugin "+p.Name+": "+msg)
		}
	}
	return out
}
