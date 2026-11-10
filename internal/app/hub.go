package app

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/hub"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/plugins"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/skills"
)

// HubFetcher downloads skills and plugins, searching the configured
// registries. GITHUB_TOKEN (environment or .env), when set, lifts GitHub's
// anonymous rate limit.
func HubFetcher(cfg *config.Config) *hub.Fetcher {
	var regs []hub.Registry
	for _, r := range cfg.SkillRegistries() {
		regs = append(regs, hub.Registry{Name: r.Name, URL: r.URL})
	}
	return &hub.Fetcher{Registries: regs, Token: func() string {
		if v := os.Getenv("GITHUB_TOKEN"); v != "" {
			return v
		}
		return loadDotEnv(filepath.Join(cfg.Home, ".env"))["GITHUB_TOKEN"]
	}}
}

// SkillLibrary is the skill library of a home, with enabled plugins' skill
// folders.
func SkillLibrary(cfg *config.Config) *skills.Library {
	return &skills.Library{Home: cfg.Home, Extra: cfg.PluginSkillDirs()}
}

// agentOf is the agent a session belongs to.
func (a *App) agentOf(ctx context.Context, session string) (string, error) {
	s, err := a.Sessions.Get(ctx, session)
	if err != nil {
		return "", err
	}
	return s.Agent, nil
}

// reply is an error whose text is meant for the chat as it stands.
type reply string

func (r reply) Error() string { return string(r) }

// SkillPrompt is the message that runs a skill explicitly (/skill and
// `hk run -skill`).
func (a *App) SkillPrompt(agentName, name, request string) (string, error) {
	return a.Skills.Invoke(agentName, name, request)
}

// addHubCommands adds /skills, /skill, /learn and enabled plugins'
// commands to cmds, never replacing a command already there or one of the
// gateway's own.
func (a *App) addHubCommands(cmds map[string]channel.AppCommand) {
	add := func(name string, c channel.AppCommand) bool {
		if _, taken := cmds[name]; taken || channel.Reserved(name) {
			return false
		}
		cmds[name] = c
		return true
	}
	add("/skills", channel.AppCommand{Help: "/skills: list the skills I can use", Run: a.listSkills})
	add("/skill", channel.AppCommand{Help: "/skill <name> [request]: follow one of my skills for this request",
		Prompt: func(ctx context.Context, session, arg string) (string, error) {
			name, request, _ := strings.Cut(arg, " ")
			if name == "" {
				return "", reply("Usage: /skill <name> [request]. /skills lists them.")
			}
			agentName, err := a.agentOf(ctx, session)
			if err != nil {
				return "", reply("Sorry, I couldn't read this conversation.")
			}
			return a.SkillPrompt(agentName, strings.ToLower(name), request)
		}})
	add("/learn", channel.AppCommand{Help: "/learn <topic, URL or path>: study it and write a new skill",
		Prompt: func(ctx context.Context, session, arg string) (string, error) {
			if arg == "" {
				return "", reply("Usage: /learn <topic, URL or path>")
			}
			agentName, err := a.agentOf(ctx, session)
			if err != nil {
				return "", reply("Sorry, I couldn't read this conversation.")
			}
			if spec := a.Runner.Agents[agentName]; spec == nil || !slices.Contains(spec.Toolsets, skills.Toolset) {
				return "", reply(fmt.Sprintf("Agent %s can't write skills: add the %q toolset to it in config.yaml.", agentName, skills.Toolset))
			}
			return skills.LearnPrompt(arg), nil
		}})
	for _, p := range a.Cfg.Plugins {
		if p.Manifest == nil {
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(p.Manifest.Commands)) {
			c := p.Manifest.Commands[name]
			help := "/" + name + ": " + c.Description
			if c.Description == "" {
				help = "/" + name + ": from plugin " + p.Name
			}
			cmd := channel.AppCommand{Help: help}
			if c.Prompt != "" {
				cmd.Prompt = func(_ context.Context, _, arg string) (string, error) {
					return plugins.Prompt(p.Name, name, c, arg), nil
				}
			} else {
				cmd.Async = true
				cmd.Run = func(ctx context.Context, session, arg string) string {
					agentName, _ := a.agentOf(ctx, session)
					out, err := plugins.Run(ctx, p, name, c, arg, []string{
						"HABAKKUK_HOME=" + a.Cfg.Home, "HABAKKUK_SESSION=" + session, "HABAKKUK_AGENT=" + agentName})
					switch {
					case err != nil && out != "":
						return fmt.Sprintf("/%s failed (%v):\n%s", name, err, out)
					case err != nil:
						return fmt.Sprintf("/%s failed: %v", name, err)
					case out == "":
						return "/" + name + " done."
					}
					return out
				}
			}
			if !add("/"+name, cmd) {
				a.Log.Warn("plugin command left out: the name is taken", "plugin", p.Name, "command", "/"+name)
			}
		}
	}
}

func (a *App) listSkills(ctx context.Context, session, _ string) string {
	agentName, err := a.agentOf(ctx, session)
	if err != nil {
		return "Sorry, I couldn't read this conversation."
	}
	list, err := a.Skills.List(agentName)
	if err != nil {
		return "Couldn't list skills: " + err.Error()
	}
	if len(list) == 0 {
		return "I have no skills yet. Teach me one with /learn <topic, URL or path>, or install one with hk skills install."
	}
	var b strings.Builder
	b.WriteString("Skills:")
	for _, s := range list {
		origin := ""
		switch {
		case s.Plugin:
			origin = " (plugin)"
		case s.Shared:
			if _, untrusted := a.Skills.Untrusted(s); untrusted {
				origin = " (installed, not approved)"
			}
		default:
			origin = " (mine)"
		}
		fmt.Fprintf(&b, "\n- %s%s: %s", s.Name, origin, s.Description)
	}
	b.WriteString("\n\nRun one with /skill <name> [request].")
	return b.String()
}

// curateSkills runs the skill curator when skills.curate says it is due.
func (a *App) curateSkills() {
	every := a.Cfg.CurateEvery()
	now := time.Now()
	if a.Skills == nil || !a.Skills.CurateDue(every, now) {
		return
	}
	var agents []string
	for _, name := range a.Cfg.AgentNames() {
		if slices.Contains(a.Cfg.Agents[name].Toolsets, skills.Toolset) {
			agents = append(agents, name)
		}
	}
	acts, err := a.Skills.Curate(skills.CurateOptions{Agents: agents, ArchiveAfter: a.Cfg.ArchiveAfter(), Now: now})
	for _, act := range acts {
		a.Log.Info("skill curator", "agent", act.Agent, "skill", act.Skill, "action", act.Action, "into", act.Into, "reason", act.Reason)
	}
	if err != nil {
		a.Log.Error("skill curator failed", "err", err)
		return
	}
	if err := a.Skills.MarkCurated(now); err != nil {
		a.Log.Warn("skill curator state", "err", err)
	}
}
