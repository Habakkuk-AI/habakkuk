package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
)

// Hermes Agent keeps one home (~/.hermes, or $HERMES_HOME) with
// config.yaml, .env, SOUL.md, memories/MEMORY.md and memories/USER.md
// (entries separated by "§" lines), skills/<category>/<name>/SKILL.md,
// cron/jobs.json and, under profiles/<name>/, further homes of the same
// shape, one per extra agent. Channel tokens and allow lists are .env
// variables; MCP servers and the model are in config.yaml.

type hermesHome struct {
	name string // "default" or the profile name
	dir  string
	main bool
}

// hermesMarks are the files that make a folder under profiles/ a profile.
var hermesMarks = []string{"config.yaml", ".env", "SOUL.md", "profile.yaml", "auth.json", "state.db"}

func readHermes(dir string, o Options) (*Found, error) {
	f := &Found{Dir: dir, userHome: o.userHome()}
	homes := []hermesHome{{name: "default", dir: dir, main: true}}
	entries, err := os.ReadDir(filepath.Join(dir, "profiles"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(dir, "profiles", e.Name())
		if slices.ContainsFunc(hermesMarks, func(m string) bool { return exists(filepath.Join(p, m)) }) {
			homes = append(homes, hermesHome{name: e.Name(), dir: p})
			f.nested = append(f.nested, "profiles/"+e.Name())
		}
	}
	taken := map[string]bool{}
	var mainEnv map[string]string
	var mainSkills map[string]string
	for _, h := range homes {
		name := o.mainAgent()
		if !h.main {
			name = cleanName(h.name, 40)
			if name == "" || taken[name] {
				name = cleanName("profile-"+h.name, 40)
			}
		}
		taken[name] = true
		r := &hermesReader{f: f, o: o, h: h, name: name, mainEnv: mainEnv, mainSkills: mainSkills}
		if err := r.read(); err != nil {
			return nil, err
		}
		if h.main {
			mainEnv, mainSkills = r.env, r.skillHashes
		}
	}
	hermesLeftovers(f, dir)
	return f, nil
}

type hermesReader struct {
	f          *Found
	o          Options
	h          hermesHome
	name       string
	cfg        map[string]any
	env        map[string]string
	used       map[string]bool // .env names the import accounts for
	agent      *Agent
	mainEnv    map[string]string // the default home's .env (nil while reading it)
	mainSkills map[string]string // name -> tree hash of the default home's skills
	// skillHashes are this home's skills, for profiles to skip copies.
	skillHashes map[string]string
}

func (r *hermesReader) read() error {
	cfgPath := filepath.Join(r.h.dir, "config.yaml")
	data, err := os.ReadFile(cfgPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		r.cfg = map[string]any{}
	case err != nil:
		return err
	default:
		if err := yaml.Unmarshal(data, &r.cfg); err != nil {
			return fmt.Errorf("%s does not parse as YAML: %w", cfgPath, err)
		}
		if r.cfg == nil {
			r.cfg = map[string]any{}
		}
	}
	if r.env, err = readDotEnv(filepath.Join(r.h.dir, ".env")); err != nil {
		return err
	}
	r.used = map[string]bool{}
	a := &Agent{From: r.h.name, Name: r.name, Main: r.h.main, Files: map[string]string{}}
	r.agent = a
	r.f.Agents = append(r.f.Agents, a)

	r.model()
	switch l := r.cfg["command_allowlist"].(type) {
	case []any:
		if len(l) > 0 {
			r.f.allowLeftover(fmt.Sprintf("the commands agent %s ran without asking (command_allowlist)", a.Name), a.Name)
		}
	case string:
		if strings.TrimSpace(l) != "" {
			r.f.allowLeftover(fmt.Sprintf("the commands agent %s ran without asking (command_allowlist)", a.Name), a.Name)
		}
	}
	if cwd, _ := dig(r.cfg, "terminal", "cwd").(string); filepath.IsAbs(cwd) {
		a.Workspace = tilde(cwd, r.o.userHome())
	} else if a.Main {
		a.Workspace = "~/habakkuk-workspace"
	}
	if err := r.persona(); err != nil {
		return err
	}
	if err := r.memories(); err != nil {
		return err
	}
	r.facts()
	if err := r.skills(); err != nil {
		return err
	}
	r.mcp()
	r.channels()
	r.schedules()
	r.f.otherEnv(r.env, r.used, "HERMES_")
	return nil
}

// model resolves the agent's model and fallbacks.
func (r *hermesReader) model() {
	a := r.agent
	var provider, model, base, key string
	switch m := r.cfg["model"].(type) {
	case string:
		model = m
	case map[string]any:
		provider, _ = m["provider"].(string)
		if model, _ = m["default"].(string); model == "" {
			model, _ = m["model"].(string)
		}
		base, _ = m["base_url"].(string)
		key, _ = m["api_key"].(string)
	}
	if model == "" {
		if !a.Main && r.f.Agents[0].Model != "" {
			a.Model = r.f.Agents[0].Model // a profile without its own model
			return
		}
		r.f.leftover(fmt.Sprintf("a model for agent %s (none is set in %s)", a.Name, filepath.Join(r.h.dir, "config.yaml")),
			"hk setup model -agent "+a.Name)
		return
	}
	if ref := r.resolve(provider, model, base, key); ref != "" {
		a.Model = ref
	} else {
		r.f.leftover(fmt.Sprintf("model %s for agent %s (provider %q has no equivalent)", model, a.Name, provider),
			"hk setup model -agent "+a.Name)
	}
	var fbs []any
	if l, ok := r.cfg["fallback_providers"].([]any); ok {
		fbs = l
	} else if m, ok := r.cfg["fallback_model"].(map[string]any); ok {
		fbs = []any{m}
	}
	for _, x := range fbs {
		m, _ := x.(map[string]any)
		p, _ := m["provider"].(string)
		id, _ := m["model"].(string)
		b, _ := m["base_url"].(string)
		if p == "" || id == "" {
			continue
		}
		if ref := r.resolve(p, id, b, ""); ref != "" && ref != a.Model && !slices.Contains(a.Fallbacks, ref) {
			a.Fallbacks = append(a.Fallbacks, ref)
		}
	}
}

// resolve turns a provider and model into "<provider>/<model>", adding the
// provider; "" when the provider has no Habakkuk equivalent.
func (r *hermesReader) resolve(provider, model, base, key string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" || p == "auto" {
		p = r.guess(base)
	}
	for _, k := range knownProviders[p].keys {
		r.used[k] = true
	}
	// A named endpoint from the providers map or the older list.
	if named, ok := dig(r.cfg, "providers", p).(map[string]any); ok {
		return modelRef(r.named(p, named), model)
	}
	if list, ok := r.cfg["custom_providers"].([]any); ok {
		for _, x := range list {
			if m, _ := x.(map[string]any); m != nil && strings.EqualFold(fmt.Sprint(m["name"]), p) {
				return modelRef(r.named(p, m), model)
			}
		}
	}
	switch p {
	case "custom", "ollama", "vllm", "llamacpp", "llama.cpp":
		if base != "" {
			v := ""
			if key == "" {
				v = "OPENAI_API_KEY" // the variable the source falls back to
				r.used[v] = true
			}
			name := r.f.customProvider("custom", base, "", v, key, r.env)
			return modelRef(name, model)
		}
	case "nous", "nous-api":
		r.f.leftover("the Nous Portal provider", "pick another provider with hk setup model -agent "+r.agent.Name)
		return ""
	}
	if name, ok := r.f.knownProvider(p, base, r.env, key); ok {
		return modelRef(name, model)
	}
	return ""
}

// guess picks the provider "auto" would use: from the base URL, then from
// the keys present.
func (r *hermesReader) guess(base string) string {
	if strings.Contains(base, "openrouter.ai") {
		return "openrouter"
	}
	if base != "" {
		if _, ok := localPreset(base); ok {
			return "custom"
		}
	}
	for _, p := range []string{"openrouter", "anthropic", "openai", "gemini", "deepseek", "xai"} {
		for _, k := range knownProviders[p].keys {
			if r.env[k] != "" {
				return p
			}
		}
	}
	return "openrouter"
}

// named adds an endpoint from the providers map.
func (r *hermesReader) named(name string, m map[string]any) string {
	base, _ := m["base_url"].(string)
	if base == "" {
		base, _ = m["api"].(string)
	}
	api, _ := m["api_mode"].(string)
	keyVar, _ := m["key_env"].(string)
	keyVal, from := secretValue(m["api_key"], r.env)
	if from != "" {
		keyVar = from
	}
	r.used[keyVar] = true
	if k, ok := knownProviders[name]; ok && k.preset != "" && base == "" {
		n, _ := r.f.knownProvider(name, "", r.env, keyVal)
		return n
	}
	return r.f.customProvider(name, base, api, keyVar, keyVal, r.env)
}

// persona reads SOUL.md and the manual system prompt.
func (r *hermesReader) persona() error {
	soul, err := readText(filepath.Join(r.h.dir, "SOUL.md"))
	if err != nil {
		return err
	}
	if soul != "" {
		r.agent.Files["SOUL.md"] = soul + "\n"
	}
	if sp, _ := dig(r.cfg, "agent", "system_prompt").(string); strings.TrimSpace(sp) != "" {
		r.agent.Files["AGENTS.md"] = strings.TrimSpace(sp) + "\n"
	}
	return nil
}

// memories splits MEMORY.md into notes and USER.md into the profile.
func (r *hermesReader) memories() error {
	mem, err := readText(filepath.Join(r.h.dir, "memories", "MEMORY.md"))
	if err != nil {
		return err
	}
	for _, e := range splitEntries(mem) {
		r.agent.Notes = append(r.agent.Notes, Note{Title: titleOf(e), Body: e, Tags: []string{"imported"}})
	}
	dedupeTitles(r.agent.Notes)
	user, err := readText(filepath.Join(r.h.dir, "memories", "USER.md"))
	if err != nil {
		return err
	}
	var lines []string
	for _, e := range splitEntries(user) {
		if !strings.Contains(e, "\n") && !strings.HasPrefix(e, "-") {
			e = "- " + e
		}
		lines = append(lines, e)
	}
	if len(lines) > 0 {
		r.agent.Profile = "## About you\n\n" + strings.Join(lines, "\n") + "\n"
	}
	return nil
}

// facts imports the fact store Hermes Agent keeps beside its memory files
// (memory_store.db, written by its memory store plugin): one note per fact,
// tagged with its category and tags. Facts already imported from
// MEMORY.md are not repeated.
func (r *hermesReader) facts() {
	path := filepath.Join(r.h.dir, "memory_store.db")
	if _, err := os.Stat(path); err != nil {
		return
	}
	facts, err := readFacts(path)
	if err != nil {
		r.f.skip("the facts in %s: %v", path, err)
		return
	}
	seen := map[string]bool{}
	for _, n := range r.agent.Notes {
		seen[strings.Join(strings.Fields(n.Body), " ")] = true
	}
	for _, f := range facts {
		key := strings.Join(strings.Fields(f.content), " ")
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		tags := []string{"imported"}
		if f.category != "" && f.category != "general" {
			tags = append(tags, f.category)
		}
		tags = append(tags, strings.Split(f.tags, ",")...)
		r.agent.Notes = append(r.agent.Notes, Note{Title: titleOf(f.content), Body: strings.TrimSpace(f.content), Tags: memory.NormalizeTags(tags)})
	}
	dedupeTitles(r.agent.Notes)
}

type fact struct{ content, category, tags string }

// readFacts reads memory_store.db's facts table, oldest first.
func readFacts(path string) ([]fact, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, closeDB, err := openSourceDB(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = closeDB() }()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'facts'`).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT COALESCE(content, ''), COALESCE(category, ''), COALESCE(tags, '') FROM facts ORDER BY created_at, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fact
	for rows.Next() {
		var f fact
		if err := rows.Scan(&f.content, &f.category, &f.tags); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// splitEntries splits a memory file on lines holding only "§".
func splitEntries(text string) []string {
	var out []string
	var cur []string
	flush := func() {
		if e := strings.TrimSpace(strings.Join(cur, "\n")); e != "" {
			out = append(out, e)
		}
		cur = nil
	}
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) == "§" {
			flush()
			continue
		}
		cur = append(cur, l)
	}
	flush()
	return out
}

// skills lists the home's own skills, leaving out the ones the source
// installed itself (named in skills/.bundled_manifest).
func (r *hermesReader) skills() error {
	root := filepath.Join(r.h.dir, "skills")
	bundled := map[string]bool{}
	if data, err := os.ReadFile(filepath.Join(root, ".bundled_manifest")); err == nil {
		for _, l := range strings.Split(string(data), "\n") {
			if n, _, _ := strings.Cut(strings.TrimSpace(l), ":"); n != "" {
				bundled[n] = true
			}
		}
	}
	dirs, err := r.f.findSkills(root)
	if err != nil {
		return err
	}
	r.skillHashes = map[string]string{}
	nBundled := 0
	for _, d := range dirs {
		base := filepath.Base(d)
		if bundled[base] {
			nBundled++
			continue
		}
		name := skillName(base)
		if name == "" {
			r.f.skip("skill folder %s: its name cannot be made a skill name", d)
			continue
		}
		if slices.ContainsFunc(r.agent.Skills, func(s Skill) bool { return s.Name == name }) {
			r.f.skip("skill %s in %s: another skill has the same name", name, d)
			continue
		}
		h, err := treeHash(d, nil)
		if err != nil {
			return err
		}
		r.skillHashes[name] = h
		if !r.h.main && r.mainSkills[name] == h {
			continue // the same skill the default home has, copied into the profile
		}
		r.agent.Skills = append(r.agent.Skills, Skill{Name: name, Dir: d, Shared: r.h.main})
	}
	if nBundled > 0 {
		r.f.skip("%s that came with %s (agent %s); edits made to them are not carried over", plural(nBundled, "skill"), Label(Hermes), r.agent.Name)
	}
	return nil
}

// mcp reads mcp_servers.
func (r *hermesReader) mcp() {
	servers, _ := r.cfg["mcp_servers"].(map[string]any)
	for _, raw := range sortedKeys(servers) {
		m, _ := servers[raw].(map[string]any)
		if m == nil {
			continue
		}
		if on, ok := m["enabled"].(bool); ok && !on {
			r.f.skip("MCP server %s: turned off in %s", raw, Label(Hermes))
			continue
		}
		s := r.f.readMCP(raw, m, r.env, r.used)
		if s == nil {
			continue
		}
		r.agent.MCP = append(r.agent.MCP, r.f.mcp(s, r.agent.Name))
	}
}

// channels reads the .env variables of each messaging platform.
func (r *hermesReader) channels() {
	a := r.agent
	suffix, nameSuffix := "", ""
	if !a.Main {
		suffix, nameSuffix = "_"+envSuffix(a.Name), "-"+a.Name
	}
	same := func(k string) bool { // a profile reusing the default home's bot
		return !a.Main && r.mainEnv[k] != "" && r.mainEnv[k] == r.env[k]
	}
	allow := func(k string) []string {
		r.used[k] = true
		var out []string
		for _, v := range strings.Split(r.env[k], ",") {
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, v)
			}
		}
		if strings.EqualFold(r.env[strings.TrimSuffix(k, "_ALLOWED_USERS")+"_ALLOW_ALL_USERS"], "true") {
			out = []string{"*"}
		}
		return out
	}
	add := func(c *Channel) {
		c.Name, c.Agent = c.Type+nameSuffix, a.Name
		r.f.Channels = append(r.f.Channels, c)
	}
	for _, k := range []string{"TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN", "SLACK_BOT_TOKEN", "SLACK_APP_TOKEN",
		"MATRIX_ACCESS_TOKEN", "MATRIX_HOMESERVER", "MATRIX_USER_ID", "SIGNAL_ACCOUNT", "SIGNAL_HTTP_URL",
		"WHATSAPP_ENABLED", "TELEGRAM_HOME_CHANNEL", "DISCORD_HOME_CHANNEL", "SLACK_HOME_CHANNEL"} {
		r.used[k] = true
	}
	if t := r.env["TELEGRAM_BOT_TOKEN"]; t != "" && !same("TELEGRAM_BOT_TOKEN") {
		v := "TELEGRAM_BOT_TOKEN" + suffix
		r.f.secret(v, t)
		add(&Channel{Type: "telegram", TokenEnv: v, AllowFrom: allow("TELEGRAM_ALLOWED_USERS")})
	}
	if t := r.env["DISCORD_BOT_TOKEN"]; t != "" && !same("DISCORD_BOT_TOKEN") {
		v := "DISCORD_BOT_TOKEN" + suffix
		r.f.secret(v, t)
		add(&Channel{Type: "discord", TokenEnv: v, AllowFrom: allow("DISCORD_ALLOWED_USERS")})
	}
	if bot, app := r.env["SLACK_BOT_TOKEN"], r.env["SLACK_APP_TOKEN"]; bot != "" && app != "" && !same("SLACK_BOT_TOKEN") {
		bv, av := "SLACK_BOT_TOKEN"+suffix, "SLACK_APP_TOKEN"+suffix
		r.f.secret(bv, bot)
		r.f.secret(av, app)
		add(&Channel{Type: "slack", AppTokenEnv: av, BotTokensEnv: []string{bv}, AllowFrom: allow("SLACK_ALLOWED_USERS")})
	} else if bot != "" && !same("SLACK_BOT_TOKEN") {
		r.f.leftover("Slack for agent "+a.Name+" (no app-level token for Socket Mode)", "hk setup channel slack -agent "+a.Name)
	}
	if hs, tok := r.env["MATRIX_HOMESERVER"], r.env["MATRIX_ACCESS_TOKEN"]; hs != "" && tok != "" && !same("MATRIX_ACCESS_TOKEN") {
		v := "MATRIX_ACCESS_TOKEN" + suffix
		r.f.secret(v, tok)
		add(&Channel{Type: "matrix", Homeserver: hs, TokenEnv: v, Account: r.env["MATRIX_USER_ID"], AllowFrom: allow("MATRIX_ALLOWED_USERS")})
		r.f.leftover("Matrix end-to-end encryption keys", "verify the session again from another Matrix client")
	}
	if acct := r.env["SIGNAL_ACCOUNT"]; acct != "" && !same("SIGNAL_ACCOUNT") {
		add(&Channel{Type: "signal", Account: acct, APIURL: r.env["SIGNAL_HTTP_URL"], AllowFrom: allow("SIGNAL_ALLOWED_USERS")})
	}
	if on := strings.ToLower(r.env["WHATSAPP_ENABLED"]); (on == "true" || on == "1" || on == "yes") && !same("WHATSAPP_ENABLED") {
		add(&Channel{Type: "whatsapp", AllowFrom: allow("WHATSAPP_ALLOWED_USERS"), Link: true})
	}
	for _, p := range []string{"EMAIL", "MATTERMOST", "SMS", "HOMEASSISTANT", "DINGTALK", "FEISHU", "WECOM", "BLUEBUBBLES"} {
		for k := range r.env {
			if strings.HasPrefix(k, p+"_") && !r.used[k] {
				r.f.leftover(fmt.Sprintf("the %s channel of agent %s", strings.ToLower(p), a.Name), "hk setup channel, or see the channels guide")
				break
			}
		}
	}
}

// schedules reads cron/jobs.json.
func (r *hermesReader) schedules() {
	path := filepath.Join(r.h.dir, "cron", "jobs.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		r.f.skip("%s could not be read (%v); its schedules were not imported", path, err)
		return
	}
	var doc struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		var list []map[string]any
		if err2 := json.Unmarshal(data, &list); err2 != nil {
			r.f.skip("%s is not valid JSON (%v); its schedules were not imported", path, err)
			return
		}
		doc.Jobs = list
	}
	disabled := 0
	for _, j := range doc.Jobs {
		name, _ := j["name"].(string)
		if name == "" {
			name, _ = j["id"].(string)
		}
		if on, ok := j["enabled"].(bool); ok && !on {
			disabled++
			continue
		}
		sch, _ := j["schedule"].(map[string]any)
		spec := ""
		switch sch["kind"] {
		case "cron":
			expr, _ := sch["expr"].(string)
			spec = expr
			if tz, _ := j["timezone"].(string); tz != "" && expr != "" {
				spec = "CRON_TZ=" + tz + " " + expr
			}
		case "interval":
			if n, ok := toInt(sch["minutes"]); ok && n > 0 {
				spec = fmt.Sprintf("@every %dm", n)
			}
		case "once":
			at, _ := sch["run_at"].(string)
			if t, ok := parseTime(at); ok && t.After(r.o.now()) {
				spec = "at " + t.Format("2006-01-02T15:04:05Z07:00")
			} else {
				r.f.skip("schedule %q (agent %s): a one-off that has already run", name, r.agent.Name)
				continue
			}
		}
		if spec == "" {
			r.f.skip("schedule %q (agent %s): its timing could not be read", name, r.agent.Name)
			continue
		}
		prompt, _ := j["prompt"].(string)
		skills := asList(j["skills"])
		if len(skills) == 0 {
			if s, _ := j["skill"].(string); s != "" {
				skills = []string{s}
			}
		}
		if len(skills) > 0 {
			prompt = strings.TrimSpace(fmt.Sprintf("Use the %s skill. %s", strings.Join(skills, ", "), prompt))
		}
		sc := &Schedule{Agent: r.agent.Name, Name: name, Spec: spec, Prompt: prompt}
		if script, _ := j["script"].(string); script != "" {
			sc.Command = scriptCommand(script, filepath.Join(r.h.dir, "scripts"))
			if wd, _ := j["workdir"].(string); filepath.IsAbs(wd) {
				sc.Workdir = wd
			}
			noAgent, _ := j["no_agent"].(bool)
			switch {
			case !noAgent:
				sc.OnOutput = "agent"
				if sc.Prompt == "" {
					sc.Prompt = "Report what matters in this output."
				}
			default:
				sc.OnOutput = "deliver"
			}
		}
		if sc.Prompt == "" && sc.Command == "" {
			r.f.skip("schedule %q (agent %s): it has no prompt or script", name, r.agent.Name)
			continue
		}
		sc.To = r.deliverTo(j)
		if sc.Command != "" && sc.OnOutput == "deliver" && sc.To == nil {
			sc.OnOutput = "silent"
		}
		r.f.Schedules = append(r.f.Schedules, sc)
	}
	if disabled > 0 {
		r.f.skip("%s of agent %s: turned off", plural(disabled, "schedule"), r.agent.Name)
	}
}

// deliverTo maps a job's delivery target onto an imported channel.
func (r *hermesReader) deliverTo(j map[string]any) *Address {
	d, _ := j["deliver"].(string)
	var platform, chat, thread string
	switch d {
	case "", "local":
		return nil
	case "origin":
		o, _ := j["origin"].(map[string]any)
		platform, _ = o["platform"].(string)
		chat = fmt.Sprint(o["chat_id"])
		if t, ok := o["thread_id"]; ok && t != nil {
			thread = fmt.Sprint(t)
		}
		if o == nil || o["chat_id"] == nil {
			chat = ""
		}
	default:
		parts := strings.SplitN(d, ":", 3)
		platform = parts[0]
		if len(parts) > 1 {
			chat = parts[1]
		} else {
			chat = r.env[strings.ToUpper(platform)+"_HOME_CHANNEL"]
		}
		if len(parts) > 2 {
			thread = parts[2]
		}
	}
	for _, c := range r.f.Channels {
		if c.Type == platform && c.Agent == r.agent.Name && chat != "" {
			return &Address{Account: c.Name, Conversation: chat, Thread: thread}
		}
	}
	name, _ := j["name"].(string)
	r.f.skip("schedule %q (agent %s): its delivery to %s was dropped (no matching channel); add one with hk schedules", name, r.agent.Name, d)
	return nil
}

// scriptCommand turns a job's script into a shell command.
func scriptCommand(script, dir string) string {
	p := script
	if !filepath.IsAbs(p) && !strings.ContainsAny(p, " ") {
		p = filepath.Join(dir, p)
	}
	q := shellQuote(p)
	switch strings.ToLower(filepath.Ext(p)) {
	case ".py":
		return "python3 " + q
	case ".sh":
		return "bash " + q
	}
	if strings.ContainsAny(script, " ") {
		return script
	}
	return q
}

// hermesLeftovers lists what the home holds that has no equivalent.
func hermesLeftovers(f *Found, dir string) {
	if !exists(filepath.Join(dir, "state.db")) && exists(filepath.Join(dir, "sessions")) {
		f.leftover("conversations kept in the older sessions/ folder", "only state.db is read; they stay in "+dir)
	}
	if exists(filepath.Join(dir, "platforms", "whatsapp", "session")) || slices.ContainsFunc(f.Channels, func(c *Channel) bool { return c.Link }) {
		f.leftover("the WhatsApp linked-device session", "hk whatsapp link (or hk setup channel whatsapp) to link the number again")
	}
	if exists(filepath.Join(dir, "kanban.db")) || exists(filepath.Join(dir, "kanban")) {
		f.leftover("board cards", "recreate the open ones with hk board add")
	}
	if exists(filepath.Join(dir, "auth.json")) {
		f.leftover("provider sign-ins (auth.json)", "hk auth login chatgpt|copilot, or an API key with hk setup model")
	}
	for _, d := range []string{"plugins", "hooks"} {
		if entries, _ := os.ReadDir(filepath.Join(dir, d)); len(entries) > 0 {
			f.leftover(d, "see hk plugins and the hooks section of config.yaml")
		}
	}
}

// redactURL is a URL as the plan shows it: no user, password or query.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(a URL that does not parse)"
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}
