package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// AgentSecurity is an agent's safety settings, written inline in
// agents.<name>.
//
// A turn that comes from a messaging channel (anything but the CLI, the
// TUI and the local dashboard) needs approval for the risky capabilities:
// exec, process, write (outside the workspace), computer and browser (on
// loopback or private addresses), whatever the permission mode. Trust
// "full" or channel_allow lifts that for the owner's own messages. Messages
// from people who are not the owner (let in by allow_from "*") never get
// those capabilities.
type AgentSecurity struct {
	// Trust is "channel" (default: channel turns ask) or "full" (the
	// owner's channel turns follow the permission mode, like local ones).
	Trust string `yaml:"trust"`
	// ChannelAllow lists the capabilities the owner's channel turns may use
	// without approval: exec, process, write, computer, browser.
	ChannelAllow []string `yaml:"channel_allow"`
	// AllowTaintedExec lets a turn that read outside content (web pages,
	// search results, email, webhook payloads) run commands and write
	// outside the workspace as the mode says. Off, such calls need a human
	// even in full mode.
	AllowTaintedExec bool `yaml:"allow_tainted_exec"`
	// TrustedWritePaths are folders outside the workspace (absolute, or
	// under ~/) where the file tools may write notes without approval, in
	// a channel turn and in one that read outside content. Commands are
	// not covered, nor are scripts, programs, hidden files, or anything in
	// Habakkuk's own folders (TrustedWriteDirs, ProtectedDirs).
	TrustedWritePaths []string `yaml:"trusted_write_paths"`
	// Allow are approval rules: calls the agent may make without asking,
	// the same as an "Always allow" answer in a chat. They never cover a
	// turn that read outside content, a guest's turn, or a write to the
	// agent's instruction files (tools.Rule).
	Allow []AllowRule `yaml:"allow"`
}

// AllowRule is one entry of agents.<name>.allow: an exact command line,
// run in the workspace or exactly in the folder in names; or a file tool
// writing inside a folder. Command (a pattern) is refused for now:
// wildcard rules may come later, with a reviewed list of programs.
type AllowRule struct {
	Command string `yaml:"command"`
	Exact   string `yaml:"exact"`
	In      string `yaml:"in"`
	Tool    string `yaml:"tool"`
	Path    string `yaml:"path"`
}

// Rule is the entry as a tools.Rule, with ~/ expanded.
func (r AllowRule) Rule() tools.Rule {
	out := tools.Rule{Tool: r.Tool, Exact: strings.TrimSpace(r.Exact),
		In: expandHome(strings.TrimSpace(r.In)), Path: expandHome(strings.TrimSpace(r.Path)), Source: "config"}
	if real, err := filepath.EvalSymlinks(out.In); out.In != "" && err == nil {
		out.In = real // a rule's folder is compared as its real path
	}
	if out.Tool == "" && out.Exact != "" {
		out.Tool = "exec"
	}
	return out
}

// AllowRules are an agent's allow entries as rules.
func (c *Config) AllowRules(agent string) []tools.Rule {
	a := c.Agents[agent]
	if a == nil {
		return nil
	}
	out := make([]tools.Rule, 0, len(a.Allow))
	for _, r := range a.Allow {
		out = append(out, r.Rule())
	}
	return out
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

// SecurityCapabilities are the names channel_allow takes.
var SecurityCapabilities = []string{"exec", "process", "write", "computer", "browser"}

func (a *Agent) validateSecurity(name string) []error {
	var errs []error
	switch a.Trust {
	case "", "channel", "full":
	default:
		errs = append(errs, fmt.Errorf("agent %s: trust %q must be channel or full", name, a.Trust))
	}
	for _, c := range a.ChannelAllow {
		if !slices.Contains(SecurityCapabilities, c) {
			errs = append(errs, fmt.Errorf("agent %s: channel_allow %q must be one of exec, process, write, computer, browser", name, c))
		}
	}
	for i, r := range a.Allow {
		if r.Command != "" {
			errs = append(errs, fmt.Errorf("agent %s: allow entry %d: command patterns are not supported yet; write the whole command line as exact (and in, if it runs outside the workspace)", name, i+1))
			continue
		}
		if r.Tool == "" && r.Exact == "" {
			errs = append(errs, fmt.Errorf("agent %s: allow entry %d needs exact, or tool and path", name, i+1))
			continue
		}
		if err := r.Rule().Validate(); err != nil {
			errs = append(errs, fmt.Errorf("agent %s: allow entry %d: %v", name, i+1, err))
		}
	}
	for _, p := range a.TrustedWritePaths {
		dir, ok := expandTrusted(p)
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("agent %s: trusted_write_paths %q must be an absolute path or start with ~/", name, p))
		case tooBroad(dir):
			errs = append(errs, fmt.Errorf("agent %s: trusted_write_paths %q is too broad: name a folder for notes, not your home folder or a folder that contains it", name, p))
		}
	}
	return errs
}

// expandTrusted turns a trusted_write_paths entry into an absolute, cleaned
// path; ok is false when it is neither absolute nor under ~/.
func expandTrusted(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "~" {
		p = "~/"
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		return filepath.Join(home, rest), true
	}
	if !filepath.IsAbs(p) {
		return "", false
	}
	return filepath.Clean(p), true
}

// tooBroad reports whether dir is a filesystem root, the user's home, or a
// folder that contains the home: trusting it would trust shell startup
// files, keys and everything else a person keeps there.
func tooBroad(dir string) bool {
	if filepath.Dir(dir) == dir {
		return true
	}
	home, err := os.UserHomeDir()
	return err == nil && contains(dir, home)
}

// contains reports whether p is dir or inside it.
func contains(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// TrustedWriteDirs are an agent's trusted_write_paths as absolute folders.
// An entry that contains Habakkuk's home, or is one of its protected
// folders or inside one, is left out and returned in ignored: trusting it
// would let a page that the agent read rewrite the agent.
func (c *Config) TrustedWriteDirs(agent string) (dirs, ignored []string) {
	a := c.Agents[agent]
	if a == nil {
		return nil, nil
	}
	protected := c.ProtectedDirs()
	for _, p := range a.TrustedWritePaths {
		dir, ok := expandTrusted(p)
		bad := !ok || tooBroad(dir) || (c.Home != "" && contains(dir, c.Home))
		for _, pr := range protected {
			bad = bad || contains(pr, dir)
		}
		if bad {
			ignored = append(ignored, p)
			continue
		}
		dirs = append(dirs, dir)
	}
	return dirs, ignored
}

// protectedFolders are the folders of Habakkuk's home that hold what the
// agent is and does: its home and memory, skills, plugins, hooks,
// personas, channel credentials and backups.
var protectedFolders = []string{"agents", "skills", "plugins", "hooks", "personalities", "channels", "backups", "scripts"}

// ProtectedDirs are the folders a trusted_write_paths entry never covers:
// Habakkuk's own folders under its home, and the folders hook commands
// name, since a file there runs on the next event.
func (c *Config) ProtectedDirs() []string {
	var out []string
	if c.Home != "" {
		for _, f := range protectedFolders {
			out = append(out, filepath.Join(c.Home, f))
		}
	}
	for _, h := range c.Hooks {
		if h != nil {
			out = append(out, CommandDirs(h.Command)...)
		}
	}
	return out
}

// CommandDirs lists the folders a shell command names: the folder of each
// path it mentions (a word that is absolute or starts with ~/), or the path
// itself when it is a folder. These are where the script of a hook or a
// scheduled command lives. A path so broad that it is the user's home or
// above is left out.
func CommandDirs(command string) []string {
	var out []string
	for _, word := range strings.Fields(command) {
		path, ok := expandTrusted(strings.Trim(word, `"';`))
		if !ok {
			continue
		}
		dir := filepath.Dir(path)
		if st, err := os.Stat(path); err == nil && st.IsDir() {
			dir = path
		}
		if !tooBroad(dir) && !slices.Contains(out, dir) {
			out = append(out, dir)
		}
	}
	return out
}

// ChannelAllowed is the set of capabilities the owner's channel turns may
// use on this agent without approval.
func (a *Agent) ChannelAllowed() map[string]bool {
	out := map[string]bool{}
	if a.Trust == "full" {
		for _, c := range SecurityCapabilities {
			out[c] = true
		}
		return out
	}
	for _, c := range a.ChannelAllow {
		out[c] = true
	}
	return out
}

// MessagingChannel reports whether an account is a messaging channel:
// every type but the token-gated web chat, which is the owner's own.
func (ch *Channel) MessagingChannel() bool { return ch != nil && ch.Type != "web" }

// OwnerOnly reports whether only the owner can ever write on the account,
// whatever allow_from says: a WhatsApp agent hears only from the person
// who created it, because the platform refuses everyone else.
func (ch *Channel) OwnerOnly() bool { return ch != nil && ch.Type == "whatsappagent" }

// Values of a channel's guest_approvals.
const (
	GuestApprovalsDeny = "deny" // a guest's risky request is refused (default)
	GuestApprovalsHome = "home" // it asks the owner in the agent's home chat
)

// GuestsAskHome reports whether a guest's risky request on the account
// asks the owner in the agent's home chat instead of being refused.
func (ch *Channel) GuestsAskHome() bool {
	return ch.OpenToAll() && ch.GuestApprovals == GuestApprovalsHome
}

// OpenToAll reports whether anyone may talk on the account (allow_from
// "*"). Such senders are guests unless their id is listed or paired. An
// owner-only account is never open, even with "*".
func (ch *Channel) OpenToAll() bool {
	return ch != nil && !ch.OwnerOnly() && slices.Contains(ch.AllowFrom, "*")
}

// ChannelsOf lists the messaging channels whose conversations can reach an
// agent: as the account's default agent, through a route, or (on accounts
// that let senders pick, like the web chat) never.
func (c *Config) ChannelsOf(agent string) []string {
	var out []string
	for name, ch := range c.Channels {
		if !ch.MessagingChannel() {
			continue
		}
		reach := ch.Agent == agent
		for _, a := range ch.Routes {
			reach = reach || a == agent
		}
		if reach {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
