package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
)

// OpenClaw keeps its state in ~/.openclaw (or $OPENCLAW_STATE_DIR): the
// config openclaw.json (JSON5) with agents, models, channels and MCP
// servers; .env; managed skills in skills/; scheduled jobs in the
// cron_jobs table of state/openclaw.sqlite (older homes: cron/jobs.json).
// Each agent has a workspace folder (workspace/, workspace-<id>/) holding
// SOUL.md, AGENTS.md, USER.md, IDENTITY.md, MEMORY.md, daily notes in
// memory/ and its own skills/.

type ocReader struct {
	f      *Found
	o      Options
	dir    string
	cfg    map[string]any
	env    map[string]string
	used   map[string]bool
	names  map[string]string // source agent id -> Habakkuk agent
	mainID string
}

func readOpenClaw(dir string, o Options) (*Found, error) {
	r := &ocReader{f: &Found{}, o: o, dir: dir, used: map[string]bool{}, names: map[string]string{}}
	path := filepath.Join(dir, "openclaw.json")
	if p := o.getenv("OPENCLAW_CONFIG_PATH"); p != "" && o.Source == "" {
		path = expandHome(p, o.userHome())
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		r.cfg = map[string]any{}
	case err != nil:
		return nil, err
	default:
		if err := decodeJSON5(data, &r.cfg); err != nil {
			return nil, fmt.Errorf("%s does not parse as JSON5: %w", path, err)
		}
		if r.cfg == nil {
			r.cfg = map[string]any{}
		}
	}
	if _, ok := r.cfg["$include"]; ok {
		r.f.skip("the $include files of %s were not read; only the main file was", path)
	}
	if r.env, err = readDotEnv(filepath.Join(dir, ".env")); err != nil {
		return nil, err
	}
	vars, _ := dig(r.cfg, "env", "vars").(map[string]any)
	for k, v := range vars {
		if s, ok := v.(string); ok && r.env[k] == "" {
			r.env[k] = s
		}
	}
	if err := r.agents(); err != nil {
		return nil, err
	}
	r.mcp()
	r.channels()
	r.schedules()
	r.leftovers()
	r.f.otherEnv(r.env, r.used, "OPENCLAW_")
	return r.f, nil
}

type ocAgent struct {
	id  string
	cfg map[string]any
}

func (r *ocReader) agents() error {
	defaults, _ := dig(r.cfg, "agents", "defaults").(map[string]any)
	var list []ocAgent
	if m, ok := dig(r.cfg, "agents", "entries").(map[string]any); ok {
		for _, id := range sortedKeys(m) {
			e, _ := m[id].(map[string]any)
			list = append(list, ocAgent{id: id, cfg: e})
		}
	} else if l, ok := dig(r.cfg, "agents", "list").([]any); ok {
		for _, x := range l {
			if e, _ := x.(map[string]any); e != nil {
				if id, _ := e["id"].(string); id != "" {
					list = append(list, ocAgent{id: id, cfg: e})
				}
			}
		}
	}
	if len(list) == 0 {
		list = []ocAgent{{id: "main"}}
	}
	// The main agent: the system agent, one marked default, "main", or the first.
	r.mainID = list[0].id
	sys, _ := dig(defaults, "systemAgent", "agentId").(string)
	for _, a := range list {
		if d, _ := a.cfg["default"].(bool); d || a.id == "main" {
			r.mainID = a.id
		}
	}
	if sys != "" && slices.ContainsFunc(list, func(a ocAgent) bool { return a.id == sys }) {
		r.mainID = sys
	}
	taken := map[string]bool{}
	for _, a := range list {
		name := r.o.mainAgent()
		if a.id != r.mainID {
			if name = cleanName(a.id, 40); name == "" || taken[name] {
				name = cleanName("agent-"+a.id, 40)
			}
		}
		taken[name] = true
		r.names[a.id] = name
	}
	shared, sharedHashes := r.sharedSkills()
	for _, oa := range list {
		a := &Agent{From: oa.id, Name: r.names[oa.id], Main: oa.id == r.mainID, Files: map[string]string{}}
		r.f.Agents = append(r.f.Agents, a)
		ws := r.workspace(oa, defaults, len(list))
		a.Workspace = tilde(ws, r.o.userHome())
		if r.f.workspaces == nil {
			r.f.workspaces = map[string]string{}
		}
		if abs, err := filepath.Abs(ws); err == nil {
			r.f.workspaces[abs] = a.Name
		}
		model := oa.cfg["model"]
		if model == nil {
			model = defaults["model"]
		}
		primary, fallbacks := modelSpec(model)
		if primary == "" {
			r.f.leftover("a model for agent "+a.Name, "hk setup model -agent "+a.Name)
		} else if ref := r.resolve(primary, defaults); ref != "" {
			a.Model = ref
		} else {
			r.f.leftover(fmt.Sprintf("model %s for agent %s (no equivalent provider)", primary, a.Name), "hk setup model -agent "+a.Name)
		}
		for _, fb := range fallbacks {
			if ref := r.resolve(fb, defaults); ref != "" && ref != a.Model && !slices.Contains(a.Fallbacks, ref) {
				a.Fallbacks = append(a.Fallbacks, ref)
			}
		}
		if err := r.workspaceFiles(a, ws, oa.cfg); err != nil {
			return err
		}
		if a.Main {
			a.Skills = append(a.Skills, shared...)
		}
		own, err := findSkills(filepath.Join(ws, "skills"))
		if err != nil {
			return err
		}
		for _, d := range own {
			name := skillName(filepath.Base(d))
			if name == "" {
				r.f.skip("skill folder %s: its name cannot be made a skill name", d)
				continue
			}
			if h, _ := treeHash(d, nil); h != "" && sharedHashes[name] == h {
				continue // the same as the shared copy
			}
			if slices.ContainsFunc(a.Skills, func(s Skill) bool { return s.Name == name && !s.Shared }) {
				continue
			}
			a.Skills = append(a.Skills, Skill{Name: name, Dir: d})
		}
	}
	return nil
}

// workspace is where an agent's files live in the source.
func (r *ocReader) workspace(a ocAgent, defaults map[string]any, n int) string {
	if ws, _ := a.cfg["workspace"].(string); ws != "" {
		return expandHome(ws, r.o.userHome())
	}
	root, _ := defaults["workspace"].(string)
	root = expandHome(root, r.o.userHome())
	switch {
	case n == 1 && root != "":
		return root
	case n == 1:
		return filepath.Join(r.dir, "workspace")
	case root != "":
		return filepath.Join(root, a.id)
	case a.id == r.mainID && exists(filepath.Join(r.dir, "workspace")) && !exists(filepath.Join(r.dir, "workspace-"+a.id)):
		return filepath.Join(r.dir, "workspace")
	}
	return filepath.Join(r.dir, "workspace-"+a.id)
}

// modelSpec reads "provider/model" or {primary, fallbacks}.
func modelSpec(v any) (string, []string) {
	switch t := v.(type) {
	case string:
		return t, nil
	case map[string]any:
		p, _ := t["primary"].(string)
		return p, asList(t["fallbacks"])
	}
	return "", nil
}

// resolve turns a model reference (or an alias of one) into a Habakkuk
// reference, adding its provider.
func (r *ocReader) resolve(ref string, defaults map[string]any) string {
	if !strings.Contains(ref, "/") {
		models, _ := defaults["models"].(map[string]any)
		for full, v := range models {
			if m, _ := v.(map[string]any); m != nil && m["alias"] == ref {
				ref = full
				break
			}
		}
	}
	p, model, ok := strings.Cut(ref, "/")
	if !ok || model == "" {
		return ""
	}
	p = strings.ToLower(p)
	for _, k := range knownProviders[p].keys {
		r.used[k] = true
	}
	if pc, ok := dig(r.cfg, "models", "providers", p).(map[string]any); ok {
		base, _ := pc["baseUrl"].(string)
		api, _ := pc["api"].(string)
		keyVal, keyVar := secretValue(pc["apiKey"], r.env)
		r.used[keyVar] = true
		if auth, _ := pc["auth"].(string); auth == "none" {
			keyVal, keyVar = "", ""
		}
		if k, ok := knownProviders[p]; ok && k.preset != "" && api != "ollama" {
			name, _ := r.f.knownProvider(p, base, r.env, keyVal)
			return modelRef(name, model)
		}
		if api == "ollama" && base == "" {
			base = "http://localhost:11434"
		}
		return modelRef(r.f.customProvider(p, base, api, keyVar, keyVal, r.env), model)
	}
	if name, ok := r.f.knownProvider(p, "", r.env, ""); ok {
		return modelRef(name, model)
	}
	return ""
}

// workspaceFiles reads the persona files, profile and memory of one agent.
func (r *ocReader) workspaceFiles(a *Agent, ws string, cfg map[string]any) error {
	read := func(name string) (string, error) { return readText(filepath.Join(ws, name)) }
	soul, err := read("SOUL.md")
	if err != nil {
		return err
	}
	if soul != "" {
		a.Files["SOUL.md"] = soul + "\n"
	}
	instr, err := read("AGENTS.md")
	if err != nil {
		return err
	}
	tools, err := read("TOOLS.md")
	if err != nil {
		return err
	}
	if tools != "" {
		instr = strings.TrimSpace(instr + "\n\n## Tools\n\n" + tools)
	}
	if instr != "" {
		a.Files["AGENTS.md"] = instr + "\n"
	}
	idText, err := read("IDENTITY.md")
	if err != nil {
		return err
	}
	id := parseIdentity(idText)
	if m, ok := cfg["identity"].(map[string]any); ok {
		if v, _ := m["name"].(string); v != "" {
			id.Name = v
		}
		if v, _ := m["emoji"].(string); v != "" {
			id.Emoji = v
		}
		if v, _ := m["theme"].(string); v != "" {
			id.Style = v
		}
	}
	if n, _ := cfg["name"].(string); id.Name == "" && n != "" {
		id.Name = n
	}
	if id = cleanIdentity(id); id.Name != "" {
		a.Identity = &id
	}
	user, err := read("USER.md")
	if err != nil {
		return err
	}
	if user != "" {
		if !regexp.MustCompile(`(?m)^## `).MatchString(user) {
			user = "## About you\n\n" + user
		}
		a.Profile = user + "\n"
	}
	mem, err := read("MEMORY.md")
	if err != nil {
		return err
	}
	for _, s := range splitSections(mem) {
		title := s.name
		if title == "" {
			title = "Long-term memory"
		}
		a.Notes = append(a.Notes, chunkNote(title, s.body, []string{"imported"})...)
	}
	entries, err := os.ReadDir(filepath.Join(ws, "memory"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	reDay := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		body, err := read(filepath.Join("memory", e.Name()))
		if err != nil {
			return err
		}
		base := strings.TrimSuffix(e.Name(), ".md")
		title, tags := strings.ReplaceAll(base, "-", " "), []string{"imported"}
		if reDay.MatchString(base) {
			title, tags = "Daily notes "+base, []string{"imported", "daily"}
		}
		a.Notes = append(a.Notes, chunkNote(title, body, tags)...)
	}
	dedupeTitles(a.Notes)
	if t, _ := read("HEARTBEAT.md"); t != "" {
		r.f.leftover("the heartbeat checklist of agent "+a.Name+" (HEARTBEAT.md)",
			`hk schedules add -agent `+a.Name+` -name heartbeat -when "@every 30m" -prompt "..."`)
	}
	return nil
}

var reIDLine = regexp.MustCompile(`^[\s>*_-]*([A-Za-z ]+?)[*_ ]*:[*_ ]*(.*)$`)

// parseIdentity reads "Name: ...", "- **Emoji:** ..." style lines.
func parseIdentity(text string) agent.Identity {
	var id agent.Identity
	for _, l := range strings.Split(text, "\n") {
		m := reIDLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		v := strings.Trim(strings.TrimSpace(m[2]), "*_ ")
		if v == "" || strings.HasPrefix(v, "(") {
			continue // an unfilled template line
		}
		switch strings.ToLower(strings.TrimSpace(m[1])) {
		case "name":
			id.Name = v
		case "emoji":
			id.Emoji = v
		case "vibe", "theme", "style", "personality":
			id.Style = v
		}
	}
	return id
}

// cleanIdentity drops fields IDENTITY.md would refuse.
func cleanIdentity(id agent.Identity) agent.Identity {
	id.Name = strings.TrimSpace(id.Name)
	if len([]rune(id.Name)) > 40 || strings.ContainsAny(id.Name, "\n\r:#") {
		id.Name = ""
	}
	id.Emoji = strings.TrimSpace(id.Emoji)
	if len([]rune(id.Emoji)) > 8 || strings.ContainsFunc(id.Emoji, func(r rune) bool {
		return r == '\n' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9')
	}) {
		id.Emoji = ""
	}
	id.Style = strings.Join(strings.Fields(id.Style), " ")
	if r := []rune(id.Style); len(r) > 300 {
		id.Style = string(r[:300])
	}
	return id
}

// sharedSkills are the managed skills in the state folder.
func (r *ocReader) sharedSkills() ([]Skill, map[string]string) {
	dirs, err := findSkills(filepath.Join(r.dir, "skills"))
	if err != nil {
		r.f.skip("skills in %s could not be listed: %v", filepath.Join(r.dir, "skills"), err)
	}
	var out []Skill
	hashes := map[string]string{}
	for _, d := range dirs {
		name := skillName(filepath.Base(d))
		if name == "" {
			r.f.skip("skill folder %s: its name cannot be made a skill name", d)
			continue
		}
		hashes[name], _ = treeHash(d, nil)
		out = append(out, Skill{Name: name, Dir: d, Shared: true})
	}
	return out, hashes
}

// mcp reads mcp.servers; every imported agent gets every server.
func (r *ocReader) mcp() {
	servers, _ := dig(r.cfg, "mcp", "servers").(map[string]any)
	for _, raw := range sortedKeys(servers) {
		m, _ := servers[raw].(map[string]any)
		if m == nil {
			continue
		}
		if on, ok := m["enabled"].(bool); ok && !on {
			r.f.skip("MCP server %s: turned off in %s", raw, Label(OpenClaw))
			continue
		}
		if t, _ := m["transport"].(string); t == "sse" {
			r.f.skip("MCP server %s uses the older SSE transport; Habakkuk connects with Streamable HTTP, so check it with hk mcp", raw)
		}
		s := r.f.readMCP(raw, m, r.env, r.used)
		if s == nil {
			continue
		}
		name := r.f.mcp(s, "imported")
		for _, a := range r.f.Agents {
			a.MCP = append(a.MCP, name)
		}
	}
}

// chatTypes are the channel types the import carries over.
var chatTypes = []string{"telegram", "discord", "slack", "whatsapp", "signal", "matrix"}

// channelDefaults are each type's token variables in both homes.
var channelDefaults = map[string]struct{ token, app string }{
	"telegram": {token: "TELEGRAM_BOT_TOKEN"},
	"discord":  {token: "DISCORD_BOT_TOKEN"},
	"slack":    {token: "SLACK_BOT_TOKEN", app: "SLACK_APP_TOKEN"},
	"matrix":   {token: "MATRIX_ACCESS_TOKEN"},
}

func (r *ocReader) channels() {
	chans, _ := r.cfg["channels"].(map[string]any)
	for _, typ := range sortedKeys(chans) {
		base, _ := chans[typ].(map[string]any)
		if base == nil || typ == "defaults" {
			continue
		}
		if on, ok := base["enabled"].(bool); ok && !on {
			continue
		}
		if !slices.Contains(chatTypes, typ) {
			r.f.leftover("the "+typ+" channel", "hk setup channel, or see the channels guide")
			continue
		}
		accounts, _ := base["accounts"].(map[string]any)
		ids := sortedKeys(accounts)
		if len(ids) == 0 {
			ids = []string{""}
		}
		for _, id := range ids {
			acct := map[string]any{}
			for k, v := range base {
				if k != "accounts" {
					acct[k] = v
				}
			}
			if a, _ := accounts[id].(map[string]any); a != nil {
				if on, ok := a["enabled"].(bool); ok && !on {
					continue
				}
				for k, v := range a {
					acct[k] = v
				}
			}
			r.channel(typ, id, acct)
		}
	}
	r.bindings()
}

// channel adds one account.
func (r *ocReader) channel(typ, id string, c map[string]any) {
	name, suffix := typ, ""
	if id != "" && id != "default" {
		name, suffix = typ+"-"+cleanName(id, 30), "_"+envSuffix(id)
	}
	// token copies a token into Habakkuk's variable, or keeps the variable
	// it names when the source holds no value for it.
	token := func(def string, v any, fallback string) string {
		val, from := secretValue(v, r.env)
		r.used[from] = true
		if val == "" && suffix == "" && fallback != "" {
			val = r.env[fallback]
			r.used[fallback] = true
		}
		if val != "" {
			r.f.secret(def+suffix, val)
			return def + suffix
		}
		return from
	}
	ch := &Channel{Name: name, Type: typ, Agent: r.names[r.mainID], AllowFrom: allowList(typ, c["allowFrom"])}
	if len(ch.AllowFrom) == 0 {
		ch.AllowFrom = allowList(typ, dig(c, "dm", "allowFrom"))
	}
	d := channelDefaults[typ]
	switch typ {
	case "telegram":
		v := c["botToken"]
		if tf, _ := c["tokenFile"].(string); v == nil && tf != "" {
			if data, err := os.ReadFile(expandHome(tf, r.o.userHome())); err == nil {
				v = strings.TrimSpace(string(data))
			}
		}
		if ch.TokenEnv = token(d.token, v, d.token); ch.TokenEnv == "" {
			r.f.leftover("the Telegram bot token for "+name, "hk setup channel telegram")
			return
		}
	case "discord":
		if ch.TokenEnv = token(d.token, c["token"], d.token); ch.TokenEnv == "" {
			r.f.leftover("the Discord bot token for "+name, "hk setup channel discord")
			return
		}
	case "slack":
		bot := token(d.token, c["botToken"], d.token)
		app := token(d.app, c["appToken"], d.app)
		if bot == "" || app == "" {
			r.f.leftover("Slack account "+name+" (Socket Mode needs a bot and an app-level token)", "hk setup channel slack")
			return
		}
		ch.AppTokenEnv, ch.BotTokensEnv = app, []string{bot}
	case "matrix":
		ch.Homeserver, _ = c["homeserver"].(string)
		ch.Account, _ = c["userId"].(string)
		if ch.TokenEnv = token(d.token, c["accessToken"], d.token); ch.TokenEnv == "" || ch.Homeserver == "" {
			r.f.leftover("Matrix account "+name+" (password sign-in or no homeserver)", "add an access token: see the channels guide")
			return
		}
		r.f.leftover("Matrix end-to-end encryption keys", "verify the session again from another Matrix client")
	case "signal":
		ch.Account, _ = c["account"].(string)
		for _, k := range []string{"httpUrl", "baseUrl", "url"} {
			if u, _ := c[k].(string); u != "" {
				ch.APIURL = u
				break
			}
		}
		if !strings.HasPrefix(ch.Account, "+") {
			r.f.leftover("Signal account "+name+" (no number configured)", "hk setup channel signal")
			return
		}
	case "whatsapp":
		ch.Link = true
		r.f.leftover("the WhatsApp linked-device session", "hk whatsapp link -account "+name+" (or hk setup channel whatsapp) to link the number again")
	}
	r.f.Channels = append(r.f.Channels, ch)
}

// allowList reads allowFrom, dropping channel prefixes.
func allowList(typ string, v any) []string {
	var out []string
	for _, s := range asList(v) {
		for _, p := range []string{typ + ":", "tg:", "user:", "uuid:"} {
			s = strings.TrimPrefix(s, p)
		}
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// bindings route channel accounts and conversations to agents.
func (r *ocReader) bindings() {
	list, _ := r.cfg["bindings"].([]any)
	for _, x := range list {
		b, _ := x.(map[string]any)
		if t, _ := b["type"].(string); t != "" && t != "route" {
			continue
		}
		name := r.names[fmt.Sprint(b["agentId"])]
		m, _ := b["match"].(map[string]any)
		typ, _ := m["channel"].(string)
		acct, _ := m["accountId"].(string)
		if name == "" || typ == "" {
			continue
		}
		cname := typ
		if acct != "" && acct != "default" && acct != "*" {
			cname = typ + "-" + cleanName(acct, 30)
		}
		for _, c := range r.f.Channels {
			if c.Name != cname && (acct != "*" || c.Type != typ) {
				continue
			}
			if peer, _ := dig(m, "peer", "id").(string); peer != "" {
				if c.Routes == nil {
					c.Routes = map[string]string{}
				}
				c.Routes[peer] = name
			} else {
				c.Agent = name
			}
		}
	}
}

// schedules reads the cron_jobs table, or an older cron/jobs.json.
func (r *ocReader) schedules() {
	var jobs []map[string]any
	db := filepath.Join(r.dir, "state", "openclaw.sqlite")
	if exists(db) {
		js, err := readCronDB(db)
		if err != nil {
			r.f.skip("%s could not be read (%v); its schedules were not imported", db, err)
		}
		jobs = append(jobs, js...)
	}
	legacy := filepath.Join(r.dir, "cron", "jobs.json")
	if data, err := os.ReadFile(legacy); err == nil && len(jobs) == 0 {
		var doc struct {
			Jobs []map[string]any `json:"jobs"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			if err2 := json.Unmarshal(data, &doc.Jobs); err2 != nil {
				r.f.skip("%s is not valid JSON (%v); its schedules were not imported", legacy, err)
			}
		}
		jobs = append(jobs, doc.Jobs...)
	}
	disabled := 0
	for _, j := range jobs {
		if on, ok := j["enabled"].(bool); ok && !on {
			disabled++
			continue
		}
		r.schedule(j)
	}
	if disabled > 0 {
		r.f.skip("%s: turned off", plural(disabled, "schedule"))
	}
}

func readCronDB(path string) ([]map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	q := url.Values{}
	q.Add("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'cron_jobs'`).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT job_json, enabled, COALESCE(agent_id, ''), name FROM cron_jobs ORDER BY sort_order, updated_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var js, agentID, name string
		var enabled bool
		if err := rows.Scan(&js, &enabled, &agentID, &name); err != nil {
			return nil, err
		}
		j := map[string]any{}
		if err := json.Unmarshal([]byte(js), &j); err != nil {
			continue
		}
		j["enabled"] = enabled
		if _, ok := j["agentId"]; !ok && agentID != "" {
			j["agentId"] = agentID
		}
		if _, ok := j["name"]; !ok {
			j["name"] = name
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (r *ocReader) schedule(j map[string]any) {
	name, _ := j["name"].(string)
	if name == "" {
		name, _ = j["id"].(string)
	}
	agentName := r.names[r.mainID]
	if id, _ := j["agentId"].(string); id != "" {
		if n := r.names[id]; n != "" {
			agentName = n
		}
	}
	sch, _ := j["schedule"].(map[string]any)
	spec := ""
	switch sch["kind"] {
	case "cron":
		expr, _ := sch["expr"].(string)
		spec = expr
		if tz, _ := sch["tz"].(string); tz != "" && expr != "" {
			spec = "CRON_TZ=" + tz + " " + expr
		}
	case "every":
		if ms, ok := toInt(sch["everyMs"]); ok && ms >= 1000 {
			spec = "@every " + (time.Duration(ms) * time.Millisecond).String()
		}
	case "at":
		var at time.Time
		ok := false
		switch v := sch["at"].(type) {
		case string:
			at, ok = parseTime(v)
		case float64:
			at, ok = time.UnixMilli(int64(v)), true
		}
		if !ok || !at.After(r.o.now()) {
			r.f.skip("schedule %q (agent %s): a one-off that has already run", name, agentName)
			return
		}
		spec = "at " + at.Format(time.RFC3339)
	}
	if spec == "" {
		r.f.skip("schedule %q (agent %s): its timing could not be read", name, agentName)
		return
	}
	sc := &Schedule{Agent: agentName, Name: name, Spec: spec}
	p, _ := j["payload"].(map[string]any)
	switch p["kind"] {
	case "agentTurn":
		sc.Prompt, _ = p["message"].(string)
	case "systemEvent":
		sc.Prompt, _ = p["text"].(string)
	default:
		argv := asList(p["argv"])
		if len(argv) == 3 && (argv[0] == "sh" || argv[0] == "bash") && (argv[1] == "-lc" || argv[1] == "-c") {
			sc.Command = argv[2]
		} else if len(argv) > 0 {
			for i, a := range argv {
				argv[i] = shellQuote(a)
			}
			sc.Command = strings.Join(argv, " ")
		} else if c, _ := p["command"].(string); c != "" {
			sc.Command = c
		}
		sc.OnOutput = "deliver"
	}
	if strings.TrimSpace(sc.Prompt) == "" && sc.Command == "" {
		r.f.skip("schedule %q (agent %s): it has no prompt or command", name, agentName)
		return
	}
	if d, _ := j["delivery"].(map[string]any); d != nil {
		sc.To = r.deliverTo(name, agentName, d)
	}
	if sc.Command != "" && sc.To == nil {
		sc.OnOutput = "silent"
	}
	r.f.Schedules = append(r.f.Schedules, sc)
}

func (r *ocReader) deliverTo(job, agentName string, d map[string]any) *Address {
	if m, _ := d["mode"].(string); m != "" && m != "announce" {
		if m == "webhook" {
			r.f.skip("schedule %q (agent %s): its webhook delivery was dropped", job, agentName)
		}
		return nil
	}
	typ, _ := d["channel"].(string)
	to, _ := d["to"].(string)
	if p, rest, ok := strings.Cut(to, ":"); ok && slices.Contains(chatTypes, p) && (typ == "" || typ == "last" || typ == p) {
		typ, to = p, rest
	}
	acct, _ := d["accountId"].(string)
	cname := typ
	if acct != "" && acct != "default" {
		cname = typ + "-" + cleanName(acct, 30)
	}
	thread := ""
	if t, ok := d["threadId"]; ok && t != nil {
		thread = fmt.Sprint(t)
	}
	for _, c := range r.f.Channels {
		if c.Name == cname && to != "" {
			return &Address{Account: c.Name, Conversation: to, Thread: thread}
		}
	}
	if typ != "" || to != "" {
		r.f.skip("schedule %q (agent %s): its delivery to %s %s was dropped (no matching channel); add one with hk schedules", job, agentName, typ, to)
	}
	return nil
}

func (r *ocReader) leftovers() {
	if ms, _ := filepath.Glob(filepath.Join(r.dir, "agents", "*", "agent", "openclaw-agent.sqlite")); len(ms) > 0 || exists(filepath.Join(r.dir, "credentials", "oauth.json")) {
		r.f.leftover("API keys and sign-ins saved by onboarding (in its own credential store)",
			"put the keys in "+"$HABAKKUK_HOME/.env, or run hk setup model")
	}
	for _, d := range []string{"extensions", "hooks"} {
		if entries, _ := os.ReadDir(filepath.Join(r.dir, d)); len(entries) > 0 {
			r.f.leftover(d, "see hk plugins and the hooks section of config.yaml")
		}
	}
}
