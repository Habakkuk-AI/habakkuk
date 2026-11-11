package migrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/core/frontmatter"
)

// Two coding tools can be imported as well as the agents. Each becomes the
// main Habakkuk agent: its instruction files become AGENTS.md, its MCP
// servers, skills and memory files come along, and its slash commands
// (custom prompts) become skills. Keys and sign-ins are never read: the
// variables the imported config names are listed for the user to set.
//
// Claude Code keeps ~/.claude (or $CLAUDE_CONFIG_DIR) with CLAUDE.md,
// settings.json, skills/<name>/SKILL.md, commands/**/*.md and, per
// project, projects/<slug>/memory/*.md. Its MCP servers are in .claude.json
// next to that folder (user servers under "mcpServers", a project's under
// "projects"). A project folder can add CLAUDE.md, .claude/CLAUDE.md,
// CLAUDE.local.md, .mcp.json and .claude/skills and .claude/commands.
// Claude Desktop's MCP servers (claude_desktop_config.json) come along too.
//
// Codex keeps ~/.codex (or $CODEX_HOME) with AGENTS.md (or
// AGENTS.override.md), config.toml (model, model_provider,
// [model_providers.*], [mcp_servers.*]), skills/<name>/SKILL.md,
// prompts/*.md and memories/. A project folder can add AGENTS.md and
// .codex/skills; ~/.agents/skills holds skills shared between tools.

// codingReader reads one coding tool's folder.
type codingReader struct {
	f     *Found
	o     Options
	dir   string
	from  string
	agent *Agent
	// instructions are the sections of AGENTS.md, in order.
	instructions []string
}

func newCodingReader(dir, from string, o Options) *codingReader {
	a := &Agent{From: from, Name: o.mainAgent(), Main: true, Files: map[string]string{}, Workspace: "~/habakkuk-workspace"}
	return &codingReader{f: &Found{Agents: []*Agent{a}, Dir: dir, userHome: o.userHome()}, o: o, dir: dir, from: from, agent: a}
}

// instruction adds an instruction file's text as a section of AGENTS.md.
func (r *codingReader) instruction(path string) error {
	text, err := readText(path)
	if err != nil || text == "" {
		return err
	}
	r.instructions = append(r.instructions, text)
	return nil
}

func (r *codingReader) finishInstructions() {
	if len(r.instructions) > 0 {
		r.agent.Files["AGENTS.md"] = strings.Join(r.instructions, "\n\n") + "\n"
	}
}

// projectInstructions keeps a project's own instruction file as a memory
// note: it is about that project, so it stays out of the agent's standing
// instructions, and the agent finds it when the project comes up.
func (r *codingReader) projectInstructions(path string) error {
	text, err := readText(path)
	if err != nil || text == "" {
		return err
	}
	title := "Instructions for " + filepath.Base(r.o.Project) + " (" + filepath.Base(path) + ")"
	body := "The instructions in " + tilde(path, r.o.userHome()) + ", for work in the project " +
		tilde(r.o.Project, r.o.userHome()) + ":\n\n" + text
	for _, n := range chunkNote(title, body, []string{"imported", "project", r.from}) {
		n.Untrusted = true // it comes from a repository, which anyone may have written
		r.agent.Notes = append(r.agent.Notes, n)
	}
	return nil
}

// mcpServers adds the servers of one "mcpServers" object. project is the
// project folder that defines them, or "".
func (r *codingReader) mcpServers(servers map[string]any, where, project string) {
	for _, raw := range sortedKeys(servers) {
		m, _ := servers[raw].(map[string]any)
		if m == nil {
			continue
		}
		if cmd, _ := m["command"].(string); strings.Contains(cmd, ".app/Contents/") {
			r.f.skip("MCP server %s (%s): it runs a program inside a desktop app, which only that app can use", raw, where)
			continue
		}
		if on, ok := m["enabled"].(bool); ok && !on {
			r.f.skip("MCP server %s (%s): turned off", raw, where)
			continue
		}
		if on, ok := m["disabled"].(bool); ok && on {
			r.f.skip("MCP server %s (%s): turned off", raw, where)
			continue
		}
		s := r.f.readMCP(raw, m, map[string]string{}, map[string]bool{})
		if s == nil {
			continue
		}
		s.Project = project
		s.Allow = asList(m["enabled_tools"])
		if off := asList(m["disabled_tools"]); len(off) > 0 {
			s.Approve = off
			r.f.leftover(fmt.Sprintf("the tools turned off for MCP server %s (%s)", s.Name, strings.Join(off, ", ")),
				"Habakkuk has no list of tools to hide, so they always ask for approval instead")
		}
		name := r.f.mcp(s, r.agent.Name)
		if !slices.Contains(r.agent.MCP, name) {
			r.agent.MCP = append(r.agent.MCP, name)
		}
	}
}

// skillsIn adds the skills under root.
func (r *codingReader) skillsIn(root string) error { return r.skillsFrom(root, "") }

// skillsFrom adds the skills under root; with project set they are a
// project's own, held until approved (see Skill.Held).
func (r *codingReader) skillsFrom(root, project string) error {
	dirs, err := r.f.findSkills(root)
	if err != nil {
		return err
	}
	for _, d := range dirs {
		name := skillName(filepath.Base(d))
		if name == "" {
			r.f.skip("skill folder %s: its name cannot be made a skill name", d)
			continue
		}
		if slices.ContainsFunc(r.agent.Skills, func(s Skill) bool { return s.Name == name }) {
			r.f.skip("skill %s in %s: another skill has the same name", name, d)
			continue
		}
		r.agent.Skills = append(r.agent.Skills, r.skill(Skill{Name: name, Dir: d}, project))
	}
	return nil
}

// commandMeta is the frontmatter of a slash command file.
type commandMeta struct {
	Description  string `yaml:"description"`
	ArgumentHint string `yaml:"argument-hint"`
}

// commandsIn turns the slash commands (custom prompts) under root into
// skills: commands/review.md is /review, commands/git/commit.md is
// /git:commit and becomes the skill git-commit. A command with a skill of
// the same name is left out, and so is one with no text.
func (r *codingReader) commandsIn(root, label string) error { return r.commandsFrom(root, label, "") }

// skill places a skill: shared, or for a project's, the agent's own and
// held until approved.
func (r *codingReader) skill(s Skill, project string) Skill {
	if project == "" {
		s.Shared = true
		return s
	}
	s.Held, s.Source = true, "project "+tilde(project, r.o.userHome())
	return s
}

// commandsFrom is commandsIn for a project's commands when project is set.
func (r *codingReader) commandsFrom(root, label, project string) error {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == root {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() && p != root && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".md") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	slices.Sort(files)
	for _, p := range files {
		rel, _ := filepath.Rel(root, p)
		rel = strings.TrimSuffix(filepath.ToSlash(rel), filepath.Ext(rel))
		command := strings.ReplaceAll(rel, "/", ":")
		name := skillName(strings.ReplaceAll(rel, "/", "-"))
		if name == "" {
			r.f.skip("%s %s: its name cannot be made a skill name", label, p)
			continue
		}
		if slices.ContainsFunc(r.agent.Skills, func(s Skill) bool { return s.Name == name }) {
			r.f.skip("%s /%s: a skill named %s is imported already", label, command, name)
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var m commandMeta
		body, _ := frontmatter.Parse(data, &m)
		if body == "" {
			r.f.skip("%s /%s: it holds no text", label, command)
			continue
		}
		desc := strings.Join(strings.Fields(m.Description), " ")
		if desc == "" {
			desc = titleOf(body)
		}
		var intro strings.Builder
		fmt.Fprintf(&intro, "This was the %s /%s in %s.", label, command, Label(r.from))
		if strings.Contains(body, "$ARGUMENTS") || strings.Contains(body, "$1") {
			intro.WriteString(" Where it says $ARGUMENTS (or $1, $2, ...), use what the user asked for")
			if m.ArgumentHint != "" {
				fmt.Fprintf(&intro, " (%s)", strings.TrimSpace(m.ArgumentHint))
			}
			intro.WriteString(".")
		}
		out, err := frontmatter.Format(map[string]string{"name": name, "description": desc}, intro.String()+"\n\n"+body)
		if err != nil {
			return err
		}
		r.agent.Skills = append(r.agent.Skills, r.skill(Skill{Name: name, Text: string(out)}, project))
	}
	return nil
}

// noteMeta is the frontmatter of a memory file.
type noteMeta struct {
	Name        string `yaml:"name"`
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
}

// memoryFiles adds each Markdown file under root as a memory note. An
// index file (MEMORY.md) that only links the others is left out; one that
// holds the memory itself is split by its sections.
func (r *codingReader) memoryFiles(root string, tags ...string) error {
	return r.memoryFrom(root, false, tags...)
}

// memoryFrom is memoryFiles; with untrusted set the notes are saved as
// untrusted (Claude Code's auto memory, written from project sessions).
func (r *codingReader) memoryFrom(root string, untrusted bool, tags ...string) error {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == root {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() && p != root && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".md") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	slices.Sort(files)
	tags = append([]string{"imported"}, tags...)
	var index []string
	for _, p := range files {
		if strings.EqualFold(filepath.Base(p), "MEMORY.md") {
			index = append(index, p)
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var m noteMeta
		body, _ := frontmatter.Parse(data, &m)
		title := firstOf(m.Name, m.Title, m.Description)
		if title == "" {
			title = titleOf(body)
		}
		for _, n := range chunkNote(title, body, tags) {
			n.Untrusted = untrusted
			r.agent.Notes = append(r.agent.Notes, n)
		}
	}
	for _, p := range index {
		text, err := readText(p)
		if err != nil {
			return err
		}
		for _, s := range splitSections(text) {
			if s.body == "" || onlyLinks(s.body) {
				continue
			}
			title := s.name
			if title == "" {
				title = titleOf(s.body)
			}
			for _, n := range chunkNote(title, s.body, tags) {
				n.Untrusted = untrusted
				r.agent.Notes = append(r.agent.Notes, n)
			}
		}
	}
	return nil
}

// onlyLinks reports whether every line of a text is a list item pointing
// at another file, as in an index of memory files.
func onlyLinks(text string) bool {
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if !strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "*") || !strings.Contains(l, "](") {
			return false
		}
	}
	return true
}

// readJSONFile reads a JSON file into a map; nil when it is missing.
func readJSONFile(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s does not parse as JSON: %w", path, err)
	}
	return m, nil
}

// readClaudeCode reads a Claude Code folder, and the project folder when
// one is given.
func readClaudeCode(dir string, o Options) (*Found, error) {
	r := newCodingReader(dir, ClaudeCode, o)
	a := r.agent
	if err := r.instruction(filepath.Join(dir, "CLAUDE.md")); err != nil {
		return nil, err
	}
	settings, err := readJSONFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return nil, err
	}
	r.claudeModel(settings)
	// .claude.json sits next to the folder (~/.claude.json), or in it when
	// the folder was moved with CLAUDE_CONFIG_DIR.
	var state map[string]any
	for _, p := range []string{filepath.Join(dir, ".claude.json"), filepath.Join(filepath.Dir(dir), ".claude.json")} {
		if state, err = readJSONFile(p); err != nil {
			return nil, err
		}
		if state != nil {
			break
		}
	}
	servers, _ := state["mcpServers"].(map[string]any)
	r.mcpServers(servers, "Claude Code", "")
	if desktop := claudeDesktopConfig(o); desktop != "" {
		cfg, err := readJSONFile(desktop)
		if err != nil {
			r.f.skip("Claude Desktop's MCP servers: %v", err)
		} else {
			servers, _ := cfg["mcpServers"].(map[string]any)
			r.mcpServers(servers, "Claude Desktop", "")
		}
	}
	if err := r.skillsIn(filepath.Join(dir, "skills")); err != nil {
		return nil, err
	}
	if err := r.commandsIn(filepath.Join(dir, "commands"), "slash command"); err != nil {
		return nil, err
	}
	// Auto memory: one folder per project the user worked in.
	mems, _ := filepath.Glob(filepath.Join(dir, "projects", "*", "memory"))
	slices.Sort(mems)
	for _, m := range mems {
		// Claude Code wrote these itself while working in a project, from
		// whatever that project held: they come in untrusted, like the
		// project's CLAUDE.md.
		if err := r.memoryFrom(m, true, "claude-code"); err != nil {
			return nil, err
		}
	}
	if p := o.Project; p != "" {
		for _, name := range []string{"CLAUDE.md", filepath.Join(".claude", "CLAUDE.md"), "CLAUDE.local.md"} {
			if err := r.projectInstructions(filepath.Join(p, name)); err != nil {
				return nil, err
			}
		}
		pm, _ := dig(state, "projects", p).(map[string]any)
		// The project's .mcp.json: only the servers Claude Code was told to
		// trust for this folder.
		mcp, err := readJSONFile(filepath.Join(p, ".mcp.json"))
		if err != nil {
			return nil, err
		}
		servers, _ := mcp["mcpServers"].(map[string]any)
		approved := claudeApprovedServers(pm)
		for _, name := range sortedKeys(servers) {
			if !approved(name) {
				r.f.skip("MCP server %s in %s: you never approved it for this folder in Claude Code", name, filepath.Join(p, ".mcp.json"))
				delete(servers, name)
			}
		}
		r.mcpServers(servers, p, tilde(p, o.userHome()))
		// Servers added for this folder in Claude Code itself (local scope).
		local, _ := pm["mcpServers"].(map[string]any)
		r.mcpServers(local, p, tilde(p, o.userHome()))
		// The project's skills and commands only from a folder you trusted
		// in Claude Code, and even then held until you approve each one.
		if trusted, _ := pm["hasTrustDialogAccepted"].(bool); trusted {
			if err := r.skillsFrom(filepath.Join(p, ".claude", "skills"), p); err != nil {
				return nil, err
			}
			if err := r.commandsFrom(filepath.Join(p, ".claude", "commands"), "slash command", p); err != nil {
				return nil, err
			}
		} else if exists(filepath.Join(p, ".claude", "skills")) || exists(filepath.Join(p, ".claude", "commands")) {
			r.f.skip("the skills and commands in %s: you never trusted that folder in Claude Code", filepath.Join(p, ".claude"))
		}
	}
	r.finishInstructions()
	dedupeTitles(a.Notes)
	for _, d := range []string{"agents", "hooks", "plugins", "output-styles"} {
		if entries, _ := os.ReadDir(filepath.Join(dir, d)); len(entries) > 0 {
			r.f.leftover("Claude Code's "+d, "no direct equivalent; see hk plugins, the hooks section of config.yaml and agents in config.yaml")
		}
	}
	if _, ok := settings["hooks"]; ok {
		r.f.leftover("the hooks in Claude Code's settings.json", "see the hooks section of config.yaml")
	}
	r.f.leftover("the Claude Code sign-in", "Habakkuk needs an API key: hk setup model")
	return r.f, nil
}

// claudeApprovedServers reads which of a project's .mcp.json servers the
// user told Claude Code to trust for that folder. Only the user's own
// record counts, in .claude.json: the folder's trust prompt accepted
// (hasTrustDialogAccepted), and then enableAllProjectMcpServers or the name
// in enabledMcpjsonServers, and not in disabledMcpjsonServers. Settings
// files inside the project are left aside: whoever wrote the repository
// wrote those.
func claudeApprovedServers(state map[string]any) func(string) bool {
	if trusted, _ := state["hasTrustDialogAccepted"].(bool); !trusted {
		return func(string) bool { return false }
	}
	all, _ := state["enableAllProjectMcpServers"].(bool)
	on, off := map[string]bool{}, map[string]bool{}
	for _, n := range asList(state["enabledMcpjsonServers"]) {
		on[n] = true
	}
	for _, n := range asList(state["disabledMcpjsonServers"]) {
		off[n] = true
	}
	return func(name string) bool { return !off[name] && (all || on[name]) }
}

// claudeModel sets the model settings.json names, when it is a full model
// id; aliases such as "opus" are left for hk setup.
func (r *codingReader) claudeModel(settings map[string]any) {
	model, _ := settings["model"].(string)
	if v, _ := dig(settings, "env", "ANTHROPIC_MODEL").(string); v != "" {
		model = v
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	if !strings.HasPrefix(model, "claude-") {
		r.f.skip("the model %q: it is a Claude Code alias, not a model id; pick a model with hk setup model", model)
		return
	}
	if p, ok := r.f.knownProvider("anthropic", "", nil, ""); ok {
		r.agent.Model = modelRef(p, model)
	}
}

// claudeDesktopConfig is Claude Desktop's config file, or "" when there
// is none.
func claudeDesktopConfig(o Options) string {
	home := o.userHome()
	var p string
	switch runtime.GOOS {
	case "darwin":
		p = filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	case "windows":
		base := o.getenv("APPDATA")
		if base == "" {
			base = filepath.Join(home, "AppData", "Roaming")
		}
		p = filepath.Join(base, "Claude", "claude_desktop_config.json")
	default:
		base := o.getenv("XDG_CONFIG_HOME")
		if base == "" {
			base = filepath.Join(home, ".config")
		}
		p = filepath.Join(base, "Claude", "claude_desktop_config.json")
	}
	if exists(p) {
		return p
	}
	return ""
}

// readCodex reads a Codex folder, and the project folder when one is
// given.
func readCodex(dir string, o Options) (*Found, error) {
	r := newCodingReader(dir, Codex, o)
	a := r.agent
	inst := filepath.Join(dir, "AGENTS.override.md")
	if !exists(inst) {
		inst = filepath.Join(dir, "AGENTS.md")
	}
	if err := r.instruction(inst); err != nil {
		return nil, err
	}
	cfg := map[string]any{}
	path := filepath.Join(dir, "config.toml")
	if data, err := os.ReadFile(path); err == nil {
		if cfg, err = decodeTOML(data); err != nil {
			return nil, fmt.Errorf("%s does not parse as TOML: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	r.codexModel(cfg)
	servers, _ := cfg["mcp_servers"].(map[string]any)
	r.mcpServers(codexServers(servers), "Codex", "")
	if err := r.skillsIn(filepath.Join(dir, "skills")); err != nil {
		return nil, err
	}
	if err := r.skillsIn(filepath.Join(o.userHome(), ".agents", "skills")); err != nil {
		return nil, err
	}
	if err := r.commandsIn(filepath.Join(dir, "prompts"), "custom prompt"); err != nil {
		return nil, err
	}
	// Codex writes these itself from past threads, from whatever those
	// held: they come in untrusted.
	if err := r.memoryFrom(filepath.Join(dir, "memories"), true, "codex"); err != nil {
		return nil, err
	}
	if p := o.Project; p != "" {
		name := "AGENTS.override.md"
		if !exists(filepath.Join(p, name)) {
			name = "AGENTS.md"
		}
		if err := r.projectInstructions(filepath.Join(p, name)); err != nil {
			return nil, err
		}
		// The project's skills only from a folder you marked trusted in
		// Codex, and even then held until you approve each one.
		level, _ := dig(cfg, "projects", p, "trust_level").(string)
		for _, d := range []string{filepath.Join(p, ".codex", "skills"), filepath.Join(p, ".agents", "skills")} {
			switch {
			case strings.EqualFold(level, "trusted"):
				if err := r.skillsFrom(d, p); err != nil {
					return nil, err
				}
			case exists(d):
				r.f.skip("the skills in %s: you never marked that folder trusted in Codex", d)
			}
		}
	}
	r.finishInstructions()
	dedupeTitles(a.Notes)
	if _, ok := cfg["notify"]; ok {
		r.f.leftover("Codex's notify command", "see the hooks section of config.yaml")
	}
	if exists(filepath.Join(dir, "rules")) {
		r.f.leftover("Codex's command rules", "set approvals in config.yaml (see the approvals guide)")
	}
	return r.f, nil
}

// codexModel reads model and model_provider. A Codex that signs in with
// ChatGPT maps to Habakkuk's ChatGPT sign-in; otherwise the provider's key
// variable is listed for the user to set.
func (r *codingReader) codexModel(cfg map[string]any) {
	model, _ := cfg["model"].(string)
	prov, _ := cfg["model_provider"].(string)
	if prov == "" {
		prov = "openai"
	}
	if profile, _ := cfg["profile"].(string); profile != "" {
		if pm, _ := dig(cfg, "profiles", profile).(map[string]any); pm != nil {
			if m, _ := pm["model"].(string); m != "" {
				model = m
			}
			if p, _ := pm["model_provider"].(string); p != "" {
				prov = p
			}
		}
	}
	if model == "" {
		return
	}
	var name string
	if custom, _ := dig(cfg, "model_providers", prov).(map[string]any); custom != nil {
		base, _ := custom["base_url"].(string)
		wire, _ := custom["wire_api"].(string)
		keyVar, _ := custom["env_key"].(string)
		if base == "" {
			r.f.skip("model provider %s: it has no base_url", prov)
			return
		}
		label, _ := custom["name"].(string)
		name = r.f.customProvider(firstOf(label, prov), base, wire, keyVar, "", map[string]string{})
	} else {
		if prov == "openai" && r.chatGPTLogin() {
			prov = "chatgpt"
		}
		var ok bool
		if name, ok = r.f.knownProvider(prov, "", map[string]string{}, ""); !ok {
			r.f.skip("model provider %s: Habakkuk has no preset for it; add it with hk setup model", prov)
			return
		}
	}
	r.agent.Model = modelRef(name, model)
}

// chatGPTLogin reports whether Codex signs in with a ChatGPT account
// rather than an API key. Only the mode is read, never a token.
func (r *codingReader) chatGPTLogin() bool {
	data, err := os.ReadFile(filepath.Join(r.dir, "auth.json"))
	if err != nil {
		return false
	}
	var auth struct {
		Mode   string          `json:"auth_mode"`
		Tokens json.RawMessage `json:"tokens"`
	}
	if json.Unmarshal(data, &auth) != nil {
		return false
	}
	return strings.EqualFold(auth.Mode, "chatgpt") || auth.Mode == "" && len(auth.Tokens) > 0 && string(auth.Tokens) != "null"
}

// codexServers turns Codex's [mcp_servers.*] tables into the shape readMCP
// reads: a bearer token variable and environment-backed headers become
// header templates, and passed-through variables become env references.
func codexServers(in map[string]any) map[string]any {
	out := map[string]any{}
	for name, v := range in {
		m, _ := v.(map[string]any)
		if m == nil {
			continue
		}
		s := map[string]any{}
		for k, x := range m {
			s[k] = x
		}
		headers := map[string]any{}
		if h, _ := m["http_headers"].(map[string]any); h != nil {
			for k, x := range h {
				headers[k] = x
			}
		}
		if h, _ := m["env_http_headers"].(map[string]any); h != nil {
			for k, x := range h {
				headers[k] = "${" + fmt.Sprint(x) + "}"
			}
		}
		if tok, _ := m["bearer_token_env_var"].(string); tok != "" {
			headers["Authorization"] = "Bearer ${" + tok + "}"
		}
		if len(headers) > 0 {
			s["headers"] = headers
		}
		env, _ := m["env"].(map[string]any)
		if vars := asList(m["env_vars"]); len(vars) > 0 {
			if env == nil {
				env = map[string]any{}
			}
			for _, k := range vars {
				if _, ok := env[k]; !ok {
					env[k] = "${" + k + "}"
				}
			}
		}
		if env != nil {
			s["env"] = env
		}
		out[name] = s
	}
	return out
}
