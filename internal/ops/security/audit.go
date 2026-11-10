// Package security audits a Habakkuk installation: file permissions under
// $HABAKKUK_HOME, who can reach agents that act without asking, network
// exposure of the HTTP surface, webhook and MCP settings, skills and hook
// scripts others could change, stale backups and disk space. Each finding
// says how to fix it; permission findings can be fixed automatically.
package security

import (
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
)

// Severity orders findings.
type Severity int

const (
	Info Severity = iota
	Low
	Medium
	High
	Critical
)

func (s Severity) String() string {
	return [...]string{"info", "low", "medium", "high", "critical"}[s]
}

// Finding is one problem and how to fix it.
type Finding struct {
	Severity Severity
	Check    string // permissions, agents, channels, http, webhooks, mcp, skills, hooks, backups, disk, config, tools
	Message  string
	Fix      string       // what to do
	apply    func() error // a safe automatic fix; nil when the fix is manual
}

// Fixable reports whether Apply can fix the finding.
func (f Finding) Fixable() bool { return f.apply != nil }

// Apply makes the automatic fix.
func (f Finding) Apply() error {
	if f.apply == nil {
		return fmt.Errorf("%s: no automatic fix", f.Check)
	}
	return f.apply()
}

// Options is what an audit looks at.
type Options struct {
	Home      string
	Config    *config.Config // nil when the config does not load
	ConfigErr error
	// Secret reads a credential the way Habakkuk does (environment, secret
	// managers, .env). Nil skips the checks that need values.
	Secret func(name string) string
	Now    time.Time
	// Disk reports free and total bytes of the file system holding path;
	// nil uses statfs.
	Disk func(path string) (free, total uint64, err error)
	// BackupDirs are searched for hk backup archives besides
	// $HABAKKUK_HOME and $HABAKKUK_HOME/backups.
	BackupDirs []string
}

// Audit runs every check. Findings are sorted, most severe first.
func Audit(o Options) []Finding {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.Disk == nil {
		o.Disk = statfs
	}
	var out []Finding
	if o.ConfigErr != nil {
		out = append(out, Finding{Severity: High, Check: "config", Message: "config.yaml does not load: " + o.ConfigErr.Error(),
			Fix: "fix config.yaml (hk doctor shows the errors); the checks below that need it were skipped"})
	}
	out = append(out, checkPermissions(o.Home)...)
	if c := o.Config; c != nil {
		out = append(out, checkAgents(c)...)
		out = append(out, checkPolicies(c)...)
		out = append(out, checkChannels(c)...)
		out = append(out, checkHTTP(c, o.Secret)...)
		out = append(out, checkMCP(c)...)
		out = append(out, checkHooks(c, o.Home)...)
		out = append(out, checkTools(c)...)
	}
	out = append(out, checkSkills(o.Home)...)
	out = append(out, checkBackups(o)...)
	out = append(out, checkDisk(o)...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Severity > out[j].Severity })
	return out
}

// --- file permissions ---

// sensitiveDirs hold chats, memory, credentials or keys throughout: agents
// (memory, profiles, identities; workspaces are skipped), blobs (chat
// attachments), browser profiles (cookies), checkpoints, meetings, device
// state, background process logs, ssh keys and channel sessions.
var sensitiveDirs = []string{"agents", "blobs", "browser", "channels", "backups", "checkpoints", "computer",
	"meetings", "procs", "ssh", "state", "auth", "mcp", "nodes"}

// sensitive names hold credentials, keys, memory or conversations.
func sensitive(rel string) bool {
	base := filepath.Base(rel)
	lower := strings.ToLower(base)
	switch {
	case base == ".env", base == "auth.json", base == "config.yaml", base == "config.yaml.bak", base == "serve.json":
		return true
	case strings.HasSuffix(base, ".db"), strings.Contains(base, ".db-"), strings.HasPrefix(base, "habakkuk.log"):
		return true
	case strings.HasSuffix(lower, ".key"), strings.HasSuffix(lower, ".pem"), strings.HasSuffix(lower, ".secret"),
		strings.Contains(lower, "token"), strings.Contains(lower, "credential"), strings.HasPrefix(lower, "id_"):
		return true
	}
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	return first != rel && slices.Contains(sensitiveDirs, first)
}

func checkPermissions(home string) []Finding {
	st, err := os.Stat(home)
	if err != nil {
		return []Finding{{Severity: Info, Check: "permissions", Message: fmt.Sprintf("%s: %v", home, err)}}
	}
	if !platform.HasUnixPermissions {
		return []Finding{{Severity: Info, Check: "permissions",
			Message: "file permissions are not checked on Windows: " + home + " is under your user profile, which only you (and administrators) can read by default"}}
	}
	var out []Finding
	if st.Mode().Perm()&0o077 != 0 {
		out = append(out, Finding{Severity: High, Check: "permissions",
			Message: fmt.Sprintf("%s is open to other users (mode %04o)", home, st.Mode().Perm()),
			Fix:     "chmod 700 " + home, apply: func() error { return os.Chmod(home, 0o700) }})
	}
	var secret, other, dirs, foreign []string
	uid := os.Getuid()
	n := 0
	_ = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == home {
			return nil
		}
		if n++; n > 200_000 {
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(home, path)
		if d.IsDir() && isWorkspace(rel) {
			return filepath.SkipDir // the user's files, not Habakkuk's
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if owner(info) >= 0 && owner(info) != uid {
			foreign = append(foreign, path)
			return nil
		}
		if info.Mode().Perm()&0o077 == 0 {
			return nil
		}
		switch {
		case d.IsDir():
			dirs = append(dirs, path)
		case sensitive(rel):
			secret = append(secret, path)
		case info.Mode().IsRegular():
			other = append(other, path)
		}
		return nil
	})
	if len(secret) > 0 {
		out = append(out, Finding{Severity: High, Check: "permissions",
			Message: fmt.Sprintf("%d credential, chat, memory or database file(s) are readable by other users: %s", len(secret), examples(secret)),
			Fix:     "chmod 600 on each (hk security -fix)", apply: chmodAll(secret, 0o600, 0o077)})
	}
	if len(dirs) > 0 {
		out = append(out, Finding{Severity: Medium, Check: "permissions",
			Message: fmt.Sprintf("%d folder(s) under %s are open to other users: %s", len(dirs), home, examples(dirs)),
			Fix:     "chmod 700 on each (hk security -fix)", apply: chmodAll(dirs, 0o700, 0o077)})
	}
	if len(other) > 0 {
		out = append(out, Finding{Severity: Low, Check: "permissions",
			Message: fmt.Sprintf("%d other file(s) are readable by other users: %s", len(other), examples(other)),
			Fix:     "chmod go-rwx on each (hk security -fix)", apply: chmodAll(other, 0, 0o077)})
	}
	if len(foreign) > 0 {
		out = append(out, Finding{Severity: Medium, Check: "permissions",
			Message: fmt.Sprintf("%d file(s) under %s belong to another user: %s", len(foreign), home, examples(foreign)),
			Fix:     "check where they came from, then chown them to the user that runs Habakkuk"})
	}
	return out
}

// isWorkspace matches agents/<name>/workspace, which holds the user's files.
func isWorkspace(rel string) bool {
	parts := strings.Split(rel, string(filepath.Separator))
	return len(parts) == 3 && parts[0] == "agents" && parts[2] == "workspace"
}

// chmodAll sets mode (or, when mode is 0, clears the bits in clear) on
// each path.
func chmodAll(paths []string, mode, clearBits fs.FileMode) func() error {
	return func() error {
		var errs []string
		for _, p := range paths {
			info, err := os.Lstat(p)
			if err != nil || info.Mode()&fs.ModeSymlink != 0 {
				continue
			}
			m := mode
			if m == 0 {
				m = info.Mode().Perm() &^ clearBits
			}
			if err := os.Chmod(p, m); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if len(errs) > 0 {
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		return nil
	}
}

func examples(paths []string) string {
	if len(paths) <= 3 {
		return strings.Join(paths, ", ")
	}
	return strings.Join(paths[:3], ", ") + fmt.Sprintf(" and %d more", len(paths)-3)
}

// --- who can reach which agent ---

// publicChannels are accounts anyone can talk to: allow_from "*" on a
// platform where strangers can find the bot. The web chat's "*" is gated by
// the API token, and a WhatsApp agent only ever hears from its creator.
func publicChannels(c *config.Config) []string {
	var out []string
	for name, ch := range c.Channels {
		if ch != nil && ch.Type != "web" && ch.OpenToAll() {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// reachedBy maps each agent to the public channels and webhooks that start
// its turns.
func reachedBy(c *config.Config) map[string][]string {
	out := map[string][]string{}
	add := func(agent, via string) {
		if agent != "" && !slices.Contains(out[agent], via) {
			out[agent] = append(out[agent], via)
		}
	}
	for _, name := range publicChannels(c) {
		ch := c.Channels[name]
		add(ch.Agent, "channel "+name)
		for _, a := range ch.Routes {
			add(a, "channel "+name)
		}
	}
	if c.HTTP != nil {
		for name, w := range c.HTTP.Webhooks {
			if w != nil {
				add(w.Agent, "webhook "+name)
			}
		}
	}
	return out
}

func checkAgents(c *config.Config) []Finding {
	var out []Finding
	reach := reachedBy(c)
	for _, name := range c.AgentNames() {
		a := c.Agents[name]
		via := reach[name]
		switch {
		case a.Permission == "full" && len(via) > 0:
			out = append(out, Finding{Severity: Critical, Check: "agents",
				Message: fmt.Sprintf("agent %s runs in full mode (no approvals) and outsiders reach it through %s", name, strings.Join(via, ", ")),
				Fix:     fmt.Sprintf("set agents.%s.permission to workspace or ask, or restrict who can reach it", name)})
		case a.Permission == "full":
			out = append(out, Finding{Severity: Medium, Check: "agents",
				Message: fmt.Sprintf("agent %s runs in full mode: every command and file change runs without approval", name),
				Fix:     fmt.Sprintf("prefer permission: workspace with approvals: reviewer_then_human for agents.%s", name)})
		}
		if a.Approvals == "reviewer" && a.Permission != "full" && a.Permission != "read-only" && len(via) > 0 {
			out = append(out, Finding{Severity: High, Check: "agents",
				Message: fmt.Sprintf("agent %s has no human approver (approvals: reviewer) and outsiders reach it through %s; a model alone decides its commands", name, strings.Join(via, ", ")),
				Fix:     fmt.Sprintf("set agents.%s.approvals to reviewer_then_human or human", name)})
		}
	}
	return out
}

func checkChannels(c *config.Config) []Finding {
	var out []Finding
	for _, name := range publicChannels(c) {
		ch := c.Channels[name]
		out = append(out, Finding{Severity: High, Check: "channels",
			Message: fmt.Sprintf("channel %s (%s) has allow_from \"*\": anyone who finds the bot can talk to agent %s and spend your model budget", name, ch.Type, ch.Agent),
			Fix:     fmt.Sprintf("list sender ids in channels.%s.allow_from, or leave it empty and approve people with hk pair", name)})
	}
	return out
}

// --- HTTP and webhooks ---

func checkHTTP(c *config.Config, secret func(string) string) []Finding {
	h := c.HTTP
	if h == nil {
		return nil
	}
	var out []Finding
	host, _, _ := net.SplitHostPort(h.Listen)
	exposed := !config.IsLoopback(host)
	if exposed {
		switch {
		case strings.HasPrefix(h.PublicURL, "http://"):
			out = append(out, Finding{Severity: High, Check: "http",
				Message: fmt.Sprintf("http listens on %s and public_url %s is plain http: the API token crosses the network unencrypted", h.Listen, h.PublicURL),
				Fix:     "put a TLS proxy or tunnel in front (an https public_url), or listen on 127.0.0.1 and use hk expose tailscale"})
		case h.PublicURL == "":
			out = append(out, Finding{Severity: Medium, Check: "http",
				Message: fmt.Sprintf("http listens on %s, reachable from the network without TLS", h.Listen),
				Fix:     "listen on 127.0.0.1 behind a TLS proxy, or use hk expose tailscale"})
		}
		if c.Metrics.Public {
			out = append(out, Finding{Severity: Low, Check: "http",
				Message: "metrics.public is set and http is reachable from the network: /metrics needs no token",
				Fix:     "unset metrics.public and give your scraper the token"})
		}
	}
	if secret != nil && h.TokenEnv != "" {
		switch tok := secret(h.TokenEnv); {
		case tok == "":
			out = append(out, Finding{Severity: High, Check: "http",
				Message: fmt.Sprintf("http.token_env names %s, which is not set", h.TokenEnv),
				Fix:     fmt.Sprintf("set %s in .env (openssl rand -hex 32)", h.TokenEnv)})
		case len(tok) < 24:
			out = append(out, Finding{Severity: Medium, Check: "http",
				Message: fmt.Sprintf("the API token in %s is short (%d characters)", h.TokenEnv, len(tok)),
				Fix:     fmt.Sprintf("use at least 32 random characters in %s (openssl rand -hex 32)", h.TokenEnv)})
		}
	}
	for _, name := range sortedKeys(h.Webhooks) {
		w := h.Webhooks[name]
		if w == nil || secret == nil {
			continue
		}
		switch s := secret(w.SecretEnv); {
		case s == "":
			out = append(out, Finding{Severity: High, Check: "webhooks",
				Message: fmt.Sprintf("webhook %s has no secret: %s is not set", name, w.SecretEnv),
				Fix:     fmt.Sprintf("set %s in .env and configure the sender to sign with it", w.SecretEnv)})
		case len(s) < 16:
			out = append(out, Finding{Severity: Medium, Check: "webhooks",
				Message: fmt.Sprintf("webhook %s has a short secret (%d characters)", name, len(s)),
				Fix:     fmt.Sprintf("use at least 32 random characters in %s", w.SecretEnv)})
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- MCP servers ---

func checkMCP(c *config.Config) []Finding {
	var out []Finding
	reach := reachedBy(c)
	for _, name := range sortedKeys(c.MCP) {
		m := c.MCP[name]
		if m == nil {
			continue
		}
		var users, risky []string
		for _, an := range c.AgentNames() {
			a := c.Agents[an]
			if slices.Contains(a.Toolsets, "mcp:"+name) {
				users = append(users, an)
				if a.Permission == "full" || len(reach[an]) > 0 {
					risky = append(risky, an)
				}
			}
		}
		if m.Risk == "exec" {
			sev, extra := Medium, ""
			if len(risky) > 0 {
				sev, extra = High, fmt.Sprintf("; %s run in full mode or are reachable by outsiders", strings.Join(risky, ", "))
			}
			used := "no agent lists it yet"
			if len(users) > 0 {
				used = "used by " + strings.Join(users, ", ")
			}
			out = append(out, Finding{Severity: sev, Check: "mcp",
				Message: fmt.Sprintf("MCP server %s can run programs (risk: exec), %s%s", name, used, extra),
				Fix:     fmt.Sprintf("list only the tools you need in mcp_servers.%s.allow, add the dangerous ones to approve, and keep its agents out of full mode", name)})
		}
		if m.URL != "" && strings.HasPrefix(m.URL, "http://") {
			if u := hostOf(m.URL); !config.IsLoopback(u) {
				out = append(out, Finding{Severity: Medium, Check: "mcp",
					Message: fmt.Sprintf("MCP server %s is reached over plain http (%s): its headers and results cross the network unencrypted", name, m.URL),
					Fix:     "use an https:// URL"})
			}
		}
	}
	return out
}

func hostOf(raw string) string {
	rest := raw[strings.Index(raw, "://")+3:]
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if h, _, err := net.SplitHostPort(rest); err == nil {
		return strings.Trim(h, "[]")
	}
	return rest
}

// --- skills and hook scripts ---

func checkSkills(home string) []Finding {
	var out []Finding
	uid := os.Getuid()
	dirs := []string{filepath.Join(home, "skills")}
	agents, _ := os.ReadDir(filepath.Join(home, "agents"))
	for _, a := range agents {
		if a.IsDir() {
			dirs = append(dirs, filepath.Join(home, "agents", a.Name(), "skills"))
		}
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			path := filepath.Join(dir, e.Name())
			if e.Type()&fs.ModeSymlink != 0 {
				target, _ := filepath.EvalSymlinks(path)
				if rel, err := filepath.Rel(home, target); err != nil || strings.HasPrefix(rel, "..") {
					out = append(out, Finding{Severity: Medium, Check: "skills",
						Message: fmt.Sprintf("skill %s is a link to %s, outside $HABAKKUK_HOME: whoever can change that folder writes instructions your agents follow", path, target),
						Fix:     "copy the skill in, or make sure only you can write to " + target})
				}
				continue
			}
			if !e.IsDir() {
				continue
			}
			var writable, foreign, scripts []string
			_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				info, err := d.Info()
				if err != nil {
					return nil
				}
				if o := owner(info); o >= 0 && o != uid {
					foreign = append(foreign, p)
				}
				if platform.HasUnixPermissions && info.Mode().Perm()&0o022 != 0 {
					writable = append(writable, p)
				}
				if info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
					scripts = append(scripts, p)
				}
				return nil
			})
			if len(writable) > 0 {
				out = append(out, Finding{Severity: High, Check: "skills",
					Message: fmt.Sprintf("skill %s can be changed by other users: %s", e.Name(), examples(writable)),
					Fix:     "chmod go-w (hk security -fix)", apply: chmodAll(writable, 0, 0o022)})
			}
			if len(foreign) > 0 {
				out = append(out, Finding{Severity: High, Check: "skills",
					Message: fmt.Sprintf("skill %s holds files owned by another user: %s", e.Name(), examples(foreign)),
					Fix:     "review the skill, then chown it to yourself or remove it"})
			}
			if len(scripts) > 0 {
				out = append(out, Finding{Severity: Info, Check: "skills",
					Message: fmt.Sprintf("skill %s ships executable files (%s): agents may run them", e.Name(), examples(scripts)),
					Fix:     "read them before trusting the skill"})
			}
		}
	}
	return out
}

func checkHooks(c *config.Config, home string) []Finding {
	var out []Finding
	uid := os.Getuid()
	seen := map[string]bool{}
	for _, h := range c.Hooks {
		if h == nil {
			continue
		}
		f := strings.Fields(h.Command)
		if len(f) == 0 {
			continue
		}
		prog := f[0]
		if rest, ok := strings.CutPrefix(prog, "~/"); ok {
			if uh, err := os.UserHomeDir(); err == nil {
				prog = filepath.Join(uh, rest)
			}
		} else if !filepath.IsAbs(prog) {
			if !strings.Contains(prog, "/") {
				continue // a program on PATH
			}
			prog = filepath.Join(home, prog)
		}
		if seen[prog] {
			continue
		}
		seen[prog] = true
		info, err := os.Stat(prog)
		if err != nil {
			continue
		}
		switch {
		case owner(info) >= 0 && owner(info) != uid && owner(info) != 0:
			out = append(out, Finding{Severity: High, Check: "hooks",
				Message: fmt.Sprintf("hook script %s (%s) belongs to another user", prog, h.Event),
				Fix:     "chown it to yourself after reviewing it"})
		case platform.HasUnixPermissions && info.Mode().Perm()&0o022 != 0:
			p := prog
			out = append(out, Finding{Severity: High, Check: "hooks",
				Message: fmt.Sprintf("hook script %s (%s) can be changed by other users, and Habakkuk runs it", prog, h.Event),
				Fix:     "chmod go-w " + prog, apply: chmodAll([]string{p}, 0, 0o022)})
		}
	}
	return out
}

// --- tool settings ---

func checkTools(c *config.Config) []Finding {
	var out []Finding
	if c.Tools.AllowPrivateURLs {
		out = append(out, Finding{Severity: Low, Check: "tools",
			Message: "tools.allow_private_urls is set: fetch_url can reach this machine, your network and cloud metadata services",
			Fix:     "unset it unless agents must read local services"})
	}
	if t := c.Telemetry; t != nil && t.IncludeContent && t.OTLPEndpoint != "" {
		out = append(out, Finding{Severity: Low, Check: "tools",
			Message: "telemetry.include_content sends message text and tool output to " + t.OTLPEndpoint,
			Fix:     "turn it off unless the collector is as private as $HABAKKUK_HOME"})
	}
	return out
}

// --- backups ---

func checkBackups(o Options) []Finding {
	var out []Finding
	dirs := append([]string{o.Home, filepath.Join(o.Home, "backups")}, o.BackupDirs...)
	var archives []string
	var newest time.Time
	for _, d := range dirs {
		matches, _ := filepath.Glob(filepath.Join(d, "habakkuk-backup-*.tar.gz"))
		for _, m := range matches {
			info, err := os.Stat(m)
			if err != nil {
				continue
			}
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
			if platform.HasUnixPermissions && info.Mode().Perm()&0o077 != 0 {
				archives = append(archives, m)
			}
		}
	}
	if len(archives) > 0 {
		out = append(out, Finding{Severity: High, Check: "backups",
			Message: fmt.Sprintf("backup archive(s) readable by other users: %s", examples(archives)),
			Fix:     "chmod 600 on each (hk security -fix)", apply: chmodAll(archives, 0o600, 0)})
	}
	switch age := o.Now.Sub(newest); {
	case newest.IsZero():
		out = append(out, Finding{Severity: Info, Check: "backups",
			Message: "no hk backup archive found in " + strings.Join(dirs, ", "),
			Fix:     "run hk backup -o " + filepath.Join(o.Home, "backups", "habakkuk-backup.tar.gz") + " regularly (a schedule works)"})
	case age > 14*24*time.Hour:
		out = append(out, Finding{Severity: Low, Check: "backups",
			Message: fmt.Sprintf("the newest backup archive is %d days old", int(age.Hours()/24)),
			Fix:     "run hk backup"})
	}
	// Credential copies (WhatsApp) for accounts no longer configured.
	whatsapp := false
	if o.Config != nil {
		for _, ch := range o.Config.Channels {
			if ch != nil && ch.Type == "whatsapp" {
				whatsapp = true
			}
		}
	}
	var stale []string
	entries, _ := os.ReadDir(filepath.Join(o.Home, "backups"))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || strings.HasPrefix(e.Name(), "habakkuk-backup-") {
			continue
		}
		if (o.Config != nil && !whatsapp && strings.HasPrefix(e.Name(), "whatsapp-")) || o.Now.Sub(info.ModTime()) > 90*24*time.Hour {
			stale = append(stale, filepath.Join(o.Home, "backups", e.Name()))
		}
	}
	if len(stale) > 0 {
		out = append(out, Finding{Severity: Low, Check: "backups",
			Message: fmt.Sprintf("%d stale credential backup(s) (older than 90 days, or for a channel no longer configured): %s", len(stale), examples(stale)),
			Fix:     "delete them once you are sure they are not needed"})
	}
	return out
}

// --- disk ---

func checkDisk(o Options) []Finding {
	free, total, err := o.Disk(o.Home)
	if err != nil || total == 0 {
		return nil
	}
	pct := float64(free) / float64(total) * 100
	msg := fmt.Sprintf("%s free on the disk holding %s (%.0f%%)", bytesText(free), o.Home, pct)
	switch {
	case free < 1<<30 || pct < 5:
		return []Finding{{Severity: High, Check: "disk", Message: msg + ": databases may fail to write and backups are skipped",
			Fix: "free space, or lower retention.days so maintenance prunes more"}}
	case free < 5<<30 || pct < 10:
		return []Finding{{Severity: Low, Check: "disk", Message: msg, Fix: "free some space soon"}}
	}
	return nil
}

func bytesText(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d bytes", n)
}

func statfs(path string) (free, total uint64, err error) { return platform.DiskUsage(path) }

// owner returns a file's uid, or -1 when the platform does not say.
func owner(info fs.FileInfo) int { return platform.FileOwner(info) }
