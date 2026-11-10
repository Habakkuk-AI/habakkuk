package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/app"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/hub"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/plugins"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/skills"
	"github.com/habakkuk-ai/habakkuk/internal/protocols/mcp"
)

const skillsUsage = `usage: hk skills [list] [-agent A]
       hk skills search QUERY
       hk skills install [-y] [-force] [-name N] SOURCE
       hk skills update [-y] [-force] [NAME]
       hk skills remove NAME
       hk skills approve NAME
       hk skills audit
       hk skills restore [-id ID] NAME | restore -list [NAME]
       hk skills curate [-dry-run] [-agent A]
SOURCE: github:owner/repo[/path][@ref], an https URL to a SKILL.md, .zip or .tar.gz,
a local path, or a name from a registry (skills.registries in config.yaml)`

// skillsCmd manages skills: lists them, and installs, updates and removes
// shared ones through the hub. It loads only the config, so it is safe to
// run beside `hk serve`.
func skillsCmd(ctx context.Context, home string, args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	lib := app.SkillLibrary(cfg)
	h := lib.Hub(app.HubFetcher(cfg))
	switch sub {
	case "list", "ls":
		return listSkills(cfg, lib, args)
	case "search":
		return hubSearch(ctx, h, args)
	case "install", "add":
		fs := flag.NewFlagSet("skills install", flag.ExitOnError)
		yes := fs.Bool("y", false, "install even if the security scan flags something (not high-risk findings)")
		force := fs.Bool("force", false, "install despite high-risk findings, and replace a skill of the same name (it is backed up)")
		name := fs.String("name", "", "install under this name")
		fs.Parse(args)
		if fs.NArg() != 1 {
			return errors.New(skillsUsage)
		}
		res, err := h.Install(ctx, fs.Arg(0), hub.Options{Name: *name, Yes: *yes, Force: *force})
		printResults(os.Stdout, res)
		if err != nil {
			return err
		}
		fmt.Println("Installed skills are not approved yet. A skill that ships scripts stays disabled, and others load with\na caution not to run commands because they say so. Read them, then approve with: hk skills approve NAME")
	case "update", "upgrade":
		return hubUpdate(ctx, h, "skills", args)
	case "audit":
		return skillsAudit(cfg, lib, h, os.Stdout)
	case "remove", "rm", "uninstall":
		if len(args) != 1 {
			return errors.New("usage: hk skills remove NAME")
		}
		bk, err := h.Remove(args[0])
		if err != nil {
			return err
		}
		if bk.ID != "" {
			fmt.Printf("removed %s (kept as backup %s; hk skills restore %s brings it back)\n", args[0], bk.ID, args[0])
		} else {
			fmt.Printf("removed %s from the lock file\n", args[0])
		}
	case "approve", "trust":
		if len(args) != 1 {
			return errors.New("usage: hk skills approve NAME")
		}
		if err := h.Set(args[0], func(e *hub.Entry) { e.Trusted = true }); err != nil {
			return err
		}
		fmt.Printf("approved %s\n", args[0])
	case "restore":
		return restoreBackups(h.Backups(), h.Restore, "skills", args)
	case "curate":
		fs := flag.NewFlagSet("skills curate", flag.ExitOnError)
		dry := fs.Bool("dry-run", false, "show what would change")
		agentName := fs.String("agent", "", "only this agent (default every agent)")
		fs.Parse(args)
		agents := cfg.AgentNames()
		if *agentName != "" {
			if _, err := defaultAgent(cfg, *agentName); err != nil {
				return err
			}
			agents = []string{*agentName}
		}
		hist, err := history.Open(ctx, home) // record the curator's changes; pinned skills stay
		if err != nil {
			return err
		}
		defer hist.Close()
		lib.History = hist
		now := time.Now()
		acts, err := lib.Curate(skills.CurateOptions{Agents: agents, ArchiveAfter: cfg.ArchiveAfter(), DryRun: *dry, Now: now})
		for _, a := range acts {
			fmt.Println(a)
		}
		if err != nil {
			return err
		}
		if len(acts) == 0 {
			fmt.Println("nothing to merge or archive")
		}
		if !*dry {
			return lib.MarkCurated(now)
		}
	case "help", "-h", "--help":
		fmt.Println(skillsUsage)
	default:
		return errors.New(skillsUsage)
	}
	return nil
}

// listSkills shows the skills an agent can use and where each comes from.
func listSkills(cfg *config.Config, lib *skills.Library, args []string) error {
	fs := flag.NewFlagSet("skills", flag.ExitOnError)
	agentName := fs.String("agent", "", "agent whose skills to show")
	fs.Parse(args)
	name, err := defaultAgent(cfg, *agentName)
	if err != nil {
		return err
	}
	list, err := lib.List(name)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Printf("no skills for agent %s; add folders with a SKILL.md under %s or %s, or hk skills install SOURCE\n", name, lib.SharedDir(), lib.AgentDir(name))
		return nil
	}
	locked, err := hub.ReadLock(filepath.Join(lib.SharedDir(), hub.LockName))
	if err != nil {
		return err
	}
	for _, s := range list {
		origin := "agent "
		switch {
		case s.Plugin:
			origin = "plugin"
		case s.Shared:
			origin = "shared"
		}
		note := ""
		if e := locked.Packages[s.Name]; s.Shared && !s.Plugin && e != nil {
			note = "  [" + e.Source
			if !e.Trusted {
				note += ", not approved"
			}
			note += "]"
		}
		fmt.Printf("%-24s %s  %s%s\n", s.Name, origin, s.Description, note)
	}
	for name, e := range locked.Packages {
		s := skills.Skill{Name: name, Dir: filepath.Join(lib.SharedDir(), name), Shared: true}
		if !e.Trusted && lib.Disabled(s) {
			fmt.Printf("%-24s shared  disabled: ships scripts and is not approved (hk skills approve %s)\n", name, name)
		}
	}
	return nil
}

// hubUpdate runs `hk skills|plugins update`: each changed package's
// diff, scripts and findings are shown, and it replaces the installed one
// only when the owner says yes (or passed -y).
func hubUpdate(ctx context.Context, h *hub.Manager, kind string, args []string) error {
	fs := flag.NewFlagSet(kind+" update", flag.ExitOnError)
	yes := fs.Bool("y", false, "apply without asking, even if the security scan flags something (not high-risk findings)")
	force := fs.Bool("force", false, "update despite high-risk findings")
	fs.Parse(args)
	in := bufio.NewReader(os.Stdin)
	interactive := isTerminal(os.Stdin)
	opt := hub.UpdateOptions{Yes: *yes, Force: *force, Confirm: func(r hub.Result) bool {
		printUpdate(os.Stdout, r)
		if *yes {
			return true
		}
		if !interactive {
			fmt.Printf("not applied: rerun with -y to apply the update of %s without a terminal\n", r.Name)
			return false
		}
		fmt.Printf("Apply the update of %s? [y/N] ", r.Name)
		line, _ := in.ReadString('\n')
		return strings.EqualFold(strings.TrimSpace(line), "y") || strings.EqualFold(strings.TrimSpace(line), "yes")
	}}
	res, err := h.Update(ctx, fs.Arg(0), opt)
	printResults(os.Stdout, res)
	for _, r := range res {
		if r.Status == "updated" && !r.Entry.Trusted {
			fmt.Printf("%s changed scripts or was flagged: it needs approval again (habakkuk %s %s %s)\n", r.Name, kind, approveVerb(kind), r.Name)
		}
	}
	return err
}

func approveVerb(kind string) string {
	if kind == "plugins" {
		return "enable"
	}
	return "approve"
}

// printUpdate shows what an update changes.
func printUpdate(w io.Writer, r hub.Result) {
	fmt.Fprintf(w, "\n%s: %s -> %s (sha256 %s)\n", r.Name, shortVersion(r.Entry.Version), r.Entry.Source, shortVersion(r.Entry.SHA256))
	for _, c := range r.Changes {
		fmt.Fprintf(w, "  %-8s %s\n", c.Kind, c.Path)
		if c.Patch == "" {
			continue
		}
		lines := strings.Split(strings.TrimSuffix(c.Patch, "\n"), "\n")
		for i, l := range lines {
			if i == 40 {
				fmt.Fprintf(w, "           ... %d more lines\n", len(lines)-40)
				break
			}
			fmt.Fprintf(w, "           %s\n", l)
		}
	}
	if len(r.Scripts) > 0 {
		fmt.Fprintf(w, "  scripts: %s\n", strings.Join(r.Scripts, ", "))
	}
	for _, f := range r.Findings {
		fmt.Fprintf(w, "  ! %s\n", findingText(f))
	}
}

// skillsAudit rescans every installed skill and plugin, and every agent's
// own skills: their scripts, scan findings, and whether installed files
// still match the hash recorded at install. It fails on high-risk
// findings or tampering.
func skillsAudit(cfg *config.Config, lib *skills.Library, h *hub.Manager, w io.Writer) error {
	serious := 0
	report := func(kind string, list []hub.AuditEntry) {
		for _, a := range list {
			state := "added by hand"
			if e := a.Entry; e != nil {
				state = "from " + e.Source
				if e.Version != "" {
					state += " @ " + shortVersion(e.Version)
				}
				switch {
				case kind == "plugin" && e.Trusted && e.Enabled:
					state += ", enabled"
				case kind == "plugin":
					state += ", disabled"
				case e.Trusted:
					state += ", approved"
				case len(a.Scripts) > 0:
					state += ", not approved (disabled: ships scripts)"
				default:
					state += ", not approved"
				}
			}
			fmt.Fprintf(w, "%s %s: %s\n", kind, a.Name, state)
			if a.Err != nil {
				fmt.Fprintf(w, "    ! could not read it: %v\n", a.Err)
				serious++
				continue
			}
			if a.Modified {
				fmt.Fprintf(w, "    ! its files changed since install (sha256 %s, installed %s): reinstall or remove it\n", shortVersion(a.Hash), shortVersion(a.Entry.SHA256))
				serious++
			}
			if len(a.Scripts) > 0 {
				fmt.Fprintf(w, "    scripts: %s\n", strings.Join(a.Scripts, ", "))
			}
			for _, f := range a.Findings {
				fmt.Fprintf(w, "    ! %s\n", findingText(f))
				if f.High() {
					serious++
				}
			}
		}
	}
	list, err := h.Audit()
	if err != nil {
		return err
	}
	report("skill", list)
	ph := plugins.Hub(cfg.Home, app.HubFetcher(cfg))
	plist, err := ph.Audit()
	if err != nil {
		return err
	}
	report("plugin", plist)
	for _, name := range cfg.AgentNames() {
		own := &hub.Manager{Kind: "skills", Dir: lib.AgentDir(name), Marker: skills.FileName}
		alist, err := own.Audit()
		if err != nil {
			return err
		}
		report("agent "+name+" skill", alist)
	}
	if len(list)+len(plist) == 0 {
		fmt.Fprintln(w, "no skills or plugins installed from outside")
	}
	if serious > 0 {
		return fmt.Errorf("%d serious finding(s): high-risk patterns or changed files", serious)
	}
	return nil
}

func findingText(f hub.Finding) string {
	if f.High() {
		return "[high] " + f.String()
	}
	return f.String()
}

func hubSearch(ctx context.Context, h *hub.Manager, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: habakkuk %s search QUERY", h.Kind)
	}
	hits, errs := h.Search(ctx, strings.Join(args, " "))
	for _, err := range errs {
		fmt.Fprintln(os.Stderr, "warning:", err)
	}
	if len(hits) == 0 {
		if len(errs) == 0 && len(h.Fetcher.Registries) == 0 {
			return errors.New("no registries configured: add skills.registries (name, url) to config.yaml")
		}
		fmt.Println("nothing found")
		return nil
	}
	for _, e := range hits {
		tags := ""
		if len(e.Tags) > 0 {
			tags = "  [" + strings.Join(e.Tags, ", ") + "]"
		}
		fmt.Printf("%-24s %s%s\n    %s (registry %s)\n", e.Name, e.Description, tags, e.Source, e.Registry)
	}
	return nil
}

// printResults shows what an install or update did, or would have done:
// the source, the pinned commit and hash, the files and the scripts among
// them, and the scan's findings.
func printResults(w io.Writer, res []hub.Result) {
	for _, r := range res {
		line := fmt.Sprintf("%-24s %s", r.Name, r.Status)
		if r.Entry.Version != "" {
			line += "  " + shortVersion(r.Entry.Version)
		}
		if r.Dir != "" && r.Status != "unchanged" {
			line += "  " + r.Dir
		}
		if r.Backup != "" {
			line += "  (previous version kept as backup " + r.Backup + ")"
		}
		fmt.Fprintln(w, line)
		if r.Status == "installed" || (r.Status == "flagged" && len(r.Changes) == 0) {
			e := r.Entry
			src := e.Source
			if e.Resolved != "" {
				src += " (" + e.Resolved + ")"
			}
			fmt.Fprintf(w, "    source  %s\n", src)
			if e.Ref != "" {
				fmt.Fprintf(w, "    ref     %s\n", e.Ref)
			}
			if e.Version != "" {
				label := "version" // a registry version, or a download's sha256
				if strings.HasPrefix(e.Source, "github:") || strings.HasPrefix(e.Resolved, "github:") {
					label = "commit "
				}
				fmt.Fprintf(w, "    %s %s\n", label, e.Version)
			}
			if e.SHA256 != "" {
				fmt.Fprintf(w, "    sha256  %s (recorded in the lock file)\n", e.SHA256)
			}
			if len(r.Files) > 0 {
				shown := r.Files
				if len(shown) > 30 {
					shown = shown[:30]
				}
				fmt.Fprintf(w, "    files   %s", strings.Join(shown, ", "))
				if len(r.Files) > len(shown) {
					fmt.Fprintf(w, " and %d more", len(r.Files)-len(shown))
				}
				fmt.Fprintln(w)
			}
			if len(r.Scripts) > 0 {
				fmt.Fprintf(w, "    scripts %s\n", strings.Join(r.Scripts, ", "))
			}
		}
		for _, f := range r.Findings {
			fmt.Fprintf(w, "    ! %s\n", findingText(f))
		}
	}
}

func shortVersion(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}

func restoreBackups(b hub.Backups, restore func(name, id string) (hub.Backup, error), kind string, args []string) error {
	fs := flag.NewFlagSet(kind+" restore", flag.ExitOnError)
	id := fs.String("id", "", "the backup to restore (default the newest)")
	list := fs.Bool("list", false, "list backups instead")
	fs.Parse(args)
	if *list {
		names := fs.Args()
		if len(names) == 0 {
			var err error
			if names, err = b.Names(); err != nil {
				return err
			}
		}
		for _, n := range names {
			bks, err := b.List(n)
			if err != nil {
				return err
			}
			for _, bk := range bks {
				fmt.Printf("%-24s %s  %s  %s\n", n, bk.ID, bk.Reason, bk.Origin)
			}
		}
		return nil
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: habakkuk %s restore [-id ID] NAME, or restore -list [NAME]", kind)
	}
	bk, err := restore(fs.Arg(0), *id)
	if err != nil {
		return err
	}
	fmt.Printf("restored %s from %s (%s) to %s\n", bk.Name, bk.ID, bk.Reason, bk.Origin)
	return nil
}

const pluginsUsage = `usage: hk plugins [list]
       hk plugins search QUERY
       hk plugins install [-y] [-force] [-name N] SOURCE
       hk plugins enable NAME | disable NAME
       hk plugins update [-y] [-force] [NAME]
       hk plugins remove NAME
       hk plugins restore [-id ID] NAME | restore -list [NAME]
Sources are as for skills; a plugin does nothing until it is enabled`

// pluginsCmd installs and manages plugins (docs/tools/plugins.md).
func pluginsCmd(ctx context.Context, home string, args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	h := plugins.Hub(home, app.HubFetcher(cfg))
	switch sub {
	case "list", "ls":
		entries, err := h.Entries()
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			fmt.Println("no plugins installed; hk plugins install SOURCE")
			return nil
		}
		loaded := map[string]*config.Plugin{}
		for _, p := range cfg.Plugins {
			loaded[p.Name] = p
		}
		names := make([]string, 0, len(entries))
		for n := range entries {
			names = append(names, n)
		}
		slices.Sort(names)
		for _, n := range names {
			e := entries[n]
			state := "disabled"
			if e.Enabled && e.Trusted {
				state = "enabled"
			}
			desc := ""
			if m, err := config.LoadPluginManifest(filepath.Join(h.Dir, n)); err == nil {
				desc = m.Description
				if m.Version != "" {
					desc = m.Version + "  " + desc
				}
			}
			fmt.Printf("%-20s %-8s %s\n    %s\n", n, state, desc, e.Source)
			if p := loaded[n]; p != nil {
				for _, msg := range p.Problems {
					fmt.Printf("    ! %s\n", msg)
				}
			}
		}
	case "search":
		return hubSearch(ctx, h, args)
	case "install", "add":
		fs := flag.NewFlagSet("plugins install", flag.ExitOnError)
		yes := fs.Bool("y", false, "install even if the security scan flags something (not high-risk findings)")
		force := fs.Bool("force", false, "install despite high-risk findings, and replace a plugin of the same name (it is backed up)")
		name := fs.String("name", "", "install under this name")
		fs.Parse(args)
		if fs.NArg() != 1 {
			return errors.New(pluginsUsage)
		}
		res, err := h.Install(ctx, fs.Arg(0), hub.Options{Name: *name, Yes: *yes, Force: *force})
		printResults(os.Stdout, res)
		if err != nil {
			return err
		}
		for _, r := range res {
			if m, err := config.LoadPluginManifest(r.Dir); err == nil {
				for _, line := range plugins.Describe(r.Name, m) {
					fmt.Println("    " + line)
				}
			}
			fmt.Printf("It is not enabled. Read it, then turn it on with: hk plugins enable %s\n", r.Name)
		}
	case "enable", "disable":
		if len(args) != 1 {
			return fmt.Errorf("usage: hk plugins %s NAME", sub)
		}
		name := args[0]
		on := sub == "enable"
		if on {
			m, err := config.LoadPluginManifest(filepath.Join(h.Dir, name))
			if err != nil {
				return err
			}
			fmt.Printf("Enabling %s. It brings:\n", name)
			for _, line := range plugins.Describe(name, m) {
				fmt.Println("  " + line)
			}
		}
		if err := h.Set(name, func(e *hub.Entry) { e.Enabled, e.Trusted = on, e.Trusted || on }); err != nil {
			return err
		}
		if on {
			// The config must still load with the plugin merged in.
			next, err := config.Load(home)
			if err != nil {
				_ = h.Set(name, func(e *hub.Entry) { e.Enabled = false })
				return fmt.Errorf("left disabled: with it, %w", err)
			}
			for _, msg := range next.PluginProblems() {
				fmt.Println("  ! " + msg)
			}
		}
		fmt.Printf("%sd %s; restart hk serve (and open chats) to apply\n", sub, name)
	case "update", "upgrade":
		return hubUpdate(ctx, h, "plugins", args)
	case "remove", "rm", "uninstall":
		if len(args) != 1 {
			return errors.New("usage: hk plugins remove NAME")
		}
		bk, err := h.Remove(args[0])
		if err != nil {
			return err
		}
		fmt.Printf("removed %s (kept as backup %s)\n", args[0], bk.ID)
	case "restore":
		return restoreBackups(h.Backups(), h.Restore, "plugins", args)
	case "help", "-h", "--help":
		fmt.Println(pluginsUsage)
	default:
		return errors.New(pluginsUsage)
	}
	return nil
}

// mcpCatalogCmd lists the built-in catalog of MCP servers.
func mcpCatalogCmd(args []string) error {
	list, err := mcp.SearchCatalog(strings.Join(args, " "))
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("nothing in the catalog matches")
		return nil
	}
	for _, e := range list {
		fmt.Printf("%-20s %s\n    %s\n", e.Name, e.Description, e.Launch())
		if vars := e.Vars(); len(vars) > 0 {
			fmt.Printf("    needs %s\n", strings.Join(vars, ", "))
		}
		if e.Server.Auth == "oauth" {
			fmt.Println("    signs in with OAuth (hk mcp login NAME)")
		}
	}
	fmt.Println("\nAdd one with: hk mcp add NAME [param=value ...]")
	return nil
}

// mcpAddCmd adds a catalog server to config.yaml.
func mcpAddCmd(home string, args []string) error {
	fs := flag.NewFlagSet("mcp add", flag.ExitOnError)
	as := fs.String("name", "", "name it differently in config.yaml (default the catalog name)")
	fs.Parse(args)
	if fs.NArg() < 1 {
		return errors.New("usage: hk mcp add [-name N] CATALOG-NAME [param=value ...]")
	}
	e, err := mcp.FindCatalog(fs.Arg(0))
	if err != nil {
		return err
	}
	params := map[string]string{}
	for _, kv := range fs.Args()[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("%q: want param=value", kv)
		}
		if rest, ok := strings.CutPrefix(v, "~/"); ok { // the shell leaves ~ after = alone
			if dir, err := os.UserHomeDir(); err == nil {
				v = filepath.Join(dir, rest)
			}
		}
		params[k] = v
	}
	s, err := e.Configure(params)
	if err != nil {
		return err
	}
	name := e.Name
	if *as != "" {
		name = *as
	}
	if err := config.AddMCPServer(home, name, s); err != nil {
		return err
	}
	fmt.Printf("Added %s to mcp_servers in %s/config.yaml (the previous file is config.yaml.bak).\n", name, home)
	if vars := e.Vars(); len(vars) > 0 {
		fmt.Printf("Set %s in %s/.env or the environment.\n", strings.Join(vars, ", "), home)
	}
	for _, prog := range e.Needs {
		if _, err := exec.LookPath(prog); err != nil {
			fmt.Printf("It runs %s, which is not on PATH.\n", prog)
		}
	}
	if e.Notes != "" {
		fmt.Println(e.Notes)
	}
	if s.Auth == "oauth" {
		fmt.Printf("It signs in with OAuth: run hk mcp login %s.\n", name)
	}
	fmt.Printf("Give it to an agent by adding \"mcp:%s\" to its toolsets, then check it with hk mcp.\n", name)
	return nil
}
