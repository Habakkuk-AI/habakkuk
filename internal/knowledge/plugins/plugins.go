// Package plugins installs and runs plugins: folders with a
// habakkuk-plugin.yaml that bundle MCP servers, skills, hooks and slash
// commands. Installing goes through the hub (same sources, lock file and
// scan as skills); a plugin does nothing until it is enabled, which also
// approves it, and config.Load merges every enabled plugin into the running
// configuration. See docs/tools/plugins.md.
package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/hub"
)

// Hub installs plugins into $HABAKKUK_HOME/plugins.
func Hub(home string, f *hub.Fetcher) *hub.Manager {
	return &hub.Manager{
		Kind: "plugins", Dir: config.PluginsDir(home), Marker: config.PluginManifestName, Fetcher: f,
		ValidName: config.ValidPluginName,
		NameOf: func(dir string) string {
			m, err := config.LoadPluginManifest(dir)
			if err != nil {
				return ""
			}
			return m.Name
		},
		Check: func(dir string) error {
			_, err := config.LoadPluginManifest(dir)
			return err
		},
	}
}

// Describe lists what a plugin brings, one line each, for `habakkuk plugins`
// to show before it is enabled.
func Describe(name string, m *config.PluginManifest) []string {
	var out []string
	for _, key := range slices.Sorted(maps.Keys(m.MCPServers)) {
		s := m.MCPServers[key]
		server := config.PluginServerName(name, key, len(m.MCPServers))
		if s.URL != "" {
			out = append(out, fmt.Sprintf("MCP server %s (toolset mcp:%s): %s", server, server, s.URL))
		} else {
			out = append(out, fmt.Sprintf("MCP server %s (toolset mcp:%s): runs %s", server, server, strings.Join(append([]string{s.Command}, s.Args...), " ")))
		}
		for _, v := range slices.Sorted(maps.Values(s.Env)) {
			out = append(out, "  needs "+v)
		}
	}
	for _, h := range m.Hooks {
		match := ""
		if h.Match != "" {
			match = " (" + h.Match + ")"
		}
		out = append(out, fmt.Sprintf("hook on %s%s: runs %s", h.Event, match, h.Command))
	}
	for _, c := range slices.Sorted(maps.Keys(m.Commands)) {
		cmd := m.Commands[c]
		if cmd.Run != "" {
			out = append(out, fmt.Sprintf("command /%s: runs %s", c, cmd.Run))
		} else {
			out = append(out, fmt.Sprintf("command /%s: a prompt", c))
		}
	}
	if m.Skills != "" {
		out = append(out, "skills from "+m.Skills+"/")
	}
	return out
}

// Prompt builds the message a prompt command sends to the agent.
func Prompt(plugin, name string, c *config.PluginCommand, args string) string {
	text := c.Prompt
	if strings.Contains(text, "{{args}}") {
		text = strings.ReplaceAll(text, "{{args}}", args)
	} else if args != "" {
		text += "\n\n" + args
	}
	return fmt.Sprintf("[Command /%s from plugin %s]\n%s", name, plugin, text)
}

const (
	defaultTimeout = 60 * time.Second
	maxOutput      = 16 << 10
)

// Run runs a script command in the plugin folder, with the words after the
// command as $1... and HABAKKUK_ARGS, and returns its output (stdout and
// stderr together). It sees PATH, HOME, LANG, TMPDIR, USER and env, not
// Habakkuk's whole environment. Its process group is killed at the timeout.
func Run(ctx context.Context, p *config.Plugin, name string, c *config.PluginCommand, args string, env []string) (string, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var cmd *exec.Cmd
	if platform.IsWindows {
		// PowerShell has no $0/$1 positional parameters for -Command: the
		// script reads its arguments from $env:HABAKKUK_ARGS.
		cmd = platform.ShellCommandContext(ctx, c.Run)
	} else {
		argv := append([]string{"-c", c.Run, name}, strings.Fields(args)...)
		cmd = exec.CommandContext(ctx, "/bin/sh", argv...)
	}
	cmd.Dir = p.Dir
	// Like an MCP server, a script gets a minimal environment, not
	// Habakkuk's (which holds its API keys).
	var base []string
	for _, k := range platform.MinimalEnvKeys() {
		if v, ok := os.LookupEnv(k); ok {
			base = append(base, k+"="+v)
		}
	}
	cmd.Env = append(append(base, env...), "HABAKKUK_ARGS="+args, "HABAKKUK_PLUGIN_DIR="+p.Dir)
	platform.KillGroupOnCancel(cmd)
	cmd.WaitDelay = time.Second
	var out capped
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("timed out after %s", timeout)
	}
	return strings.TrimSpace(out.String()), err
}

// capped keeps the first maxOutput bytes and never fails a write.
type capped struct{ bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := maxOutput - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
