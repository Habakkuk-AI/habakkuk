package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/config/yamledit"
	"github.com/habakkuk-ai/habakkuk/internal/core/frontmatter"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/hub"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
)

// What a plan item needs.
const (
	New      = "new"           // created
	Same     = "already there" // nothing to do
	Update   = "update"        // added to what is there (toolsets, profile sections)
	Conflict = "conflict"      // something different is there; left alone without Force
	Replace  = "replace"       // a conflict that Force replaces
	Rejected = "not imported"  // refused: a security finding, a secret in a note, no model
)

// Item is one thing the import creates or checks.
type Item struct {
	Kind   string // agent, provider, mcp, channel, file, profile, note, skill, secret, schedule
	Agent  string // the Habakkuk agent it belongs to, if any
	Name   string
	Status string
	Detail string

	note     *Note
	skill    *Skill
	schedule *Schedule
	file     string // persona files: the text to write
	ident    *agent.Identity
	oldID    string // a schedule a Force replaces, or one changed in place
	// inPlace: only the references of what is there change (a schedule
	// keeps its ID and stays on or paused; a profile keeps its sections).
	inPlace bool
	// refs is how many references in its text were rewritten to name
	// copied files.
	refs int
	// fix is how many references to copied files a conflict holds: what is
	// there differs from the import and stays, but those references are
	// rewritten where they are.
	fix  int
	have *Schedule // schedules: the text of the one that is there
	// hist are the new conversations of a history item, with how many
	// messages they hold; histSame counts the ones already there.
	hist               []*Conversation
	histMsgs, histSame int
}

// Rewritten is how many references in the item's text name copied files
// instead of the source home.
func (it *Item) Rewritten() int {
	if it.Status == Conflict {
		return it.fix
	}
	return it.refs
}

// Fixes reports whether the item is a conflict whose references to copied
// files are rewritten where they are. The rest of it is left alone.
func (it *Item) Fixes() bool { return it.Status == Conflict && it.fix > 0 }

// moved is the detail of an item that is already there and only needs its
// references rewritten.
const moved = "it will name the copied files"

// fixed is added to the detail of a conflict whose references are
// rewritten.
const fixed = "; kept, with its references to the copied files rewritten"

// noteFix counts the references to copied files in the text a conflict
// holds and marks the item for rewriting them.
func (p *Plan) noteFix(it *Item, texts ...string) {
	if it.Status != Conflict {
		return
	}
	for _, t := range texts {
		_, n := p.rw.apply(t)
		it.fix += n
	}
	if it.fix > 0 {
		it.Detail += fixed
	}
}

// Changes reports whether the item writes anything.
func (it *Item) Changes() bool {
	return it.Status == New || it.Status == Update || it.Status == Replace
}

// Plan is what an import will do to a Habakkuk home.
type Plan struct {
	Found *Found
	Home  string
	Items []*Item
	// Config is the new config.yaml (nil when it does not change); OldConfig
	// is the current one (nil when there is none).
	Config, OldConfig []byte
	// ConfigErr is set when the edited config.yaml would not load; Apply
	// then refuses to run.
	ConfigErr error
	// NoConfig explains why config.yaml is left alone, when it is.
	NoConfig string
	// Files is what the import does about the files in the source home
	// that the imported text names; nil when it names none. When entries
	// are copied (now, or by an earlier run), Found holds the text as it is
	// written: with those references rewritten.
	Files *FilesPlan

	opts   Options
	dotenv map[string]string
	rw     *rewriter      // rewrites references to copied entries; nil when there are none
	refs   map[string]int // hitID -> references rewritten in that item
}

// NewPlan compares what was found with the Habakkuk home at home. It
// reads the home but changes nothing.
func NewPlan(f *Found, home string, o Options) (*Plan, error) {
	f, keyed := f.withoutKeyed(o.Secrets != "copy" || IsCodingTool(f.From))
	p := &Plan{Found: f, Home: home, opts: o}
	var cfg *config.Config
	raw, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if cfg, err = config.ParseHome(raw, home); err != nil {
			return nil, fmt.Errorf("%s does not load, so the import cannot edit it safely: %w", filepath.Join(home, "config.yaml"), err)
		}
		p.OldConfig = raw
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	if p.dotenv, err = readDotEnv(filepath.Join(home, ".env")); err != nil {
		return nil, err
	}
	if err := p.planFiles(); err != nil {
		return nil, err
	}
	f = p.Found // with the references to copied files rewritten
	var doc *yaml.Node
	if raw != nil {
		if doc, err = yamledit.Parse(raw); err != nil {
			return nil, err
		}
	} else {
		doc = newConfigDoc()
	}
	root := doc.Content[0]
	edited := p.planConfig(cfg, root)
	for _, m := range keyed {
		why := "values in its command line, URL or headers may be keys, and keys are not copied; run again with --secrets copy to add it with them in .env, or add it yourself with hk mcp add " + m.Name
		if IsCodingTool(f.From) {
			why = "values in its command line, URL or headers may be keys, and keys never move from " + Label(f.From) + "; add it to mcp_servers in config.yaml yourself, with the key in .env, or with hk mcp add " + m.Name
		}
		p.add(&Item{Kind: "mcp", Name: m.Name, Status: Rejected, Detail: why})
	}
	switch {
	case len(cfg.Agents) == 0 && !slices.ContainsFunc(p.Items, func(it *Item) bool { return it.Kind == "agent" && it.Changes() }):
		p.NoConfig = "no agent has a model the import could set up, so config.yaml is not written; run hk setup, then hk migrate again for the channels and MCP servers"
		for _, it := range p.Items {
			if it.Changes() && (it.Kind == "provider" || it.Kind == "mcp" || it.Kind == "channel") {
				it.Status, it.Detail = Rejected, "needs an agent with a model first"
			}
		}
	case !edited:
	default:
		out, err := yamledit.Encode(doc)
		if err != nil {
			return nil, err
		}
		if _, err := config.ParseHome(out, home); err != nil {
			p.ConfigErr = fmt.Errorf("the edited config.yaml would not load: %w", err)
		}
		p.Config = out
	}
	for _, a := range f.Agents {
		if err := p.planAgentFiles(a); err != nil {
			return nil, err
		}
	}
	for _, k := range sortedKeys(f.Secrets) {
		it := &Item{Kind: "secret", Name: k, Status: New}
		switch have, ok := p.dotenv[k]; {
		case ok && have == f.Secrets[k]:
			it.Status = Same
		case ok:
			it.Status, it.Detail = p.conflict(), ".env already sets it to something else"
		}
		p.Items = append(p.Items, it)
	}
	if err := p.planSchedules(); err != nil {
		return nil, err
	}
	if err := p.planHistory(); err != nil {
		return nil, err
	}
	for _, it := range p.Items {
		if it.ident == nil {
			it.refs = p.refs[hitID(it.Kind, it.Agent, it.Name)]
		}
	}
	return p, nil
}

func (p *Plan) conflict() string {
	if p.opts.Force {
		return Replace
	}
	return Conflict
}

func (p *Plan) add(it *Item) *Item {
	p.Items = append(p.Items, it)
	return it
}

// planConfig decides the config.yaml items and edits root; it reports
// whether anything changed.
func (p *Plan) planConfig(cfg *config.Config, root *yaml.Node) bool {
	f := p.Found
	edited := false
	configured := map[string]bool{} // agents config.yaml will have
	for name := range cfg.Agents {
		configured[name] = true
	}
	for _, a := range f.Agents {
		it := p.add(&Item{Kind: "agent", Agent: a.Name, Name: a.Name, Detail: a.Model})
		have := cfg.Agents[a.Name]
		var toolsHave []string
		if have != nil && len(have.Toolsets) > 0 && !slices.Equal(have.Toolsets, defaultToolsets) {
			toolsHave = have.Toolsets
		}
		switch {
		case have == nil && a.Model == "":
			it.Status, it.Detail = Rejected, "no model found; its files are imported, then run hk setup model -agent "+a.Name
			continue
		case have == nil:
			it.Status = New
			if a.Workspace != "" {
				it.Detail += ", workspace " + a.Workspace
			}
		case a.Model == "" || have.Model == a.Model:
			it.Status, it.Detail = Same, have.Model
			if toolsets(toolsHave, a.MCP) != nil {
				it.Status, it.Detail = Update, "adds the MCP toolsets"
			}
		default:
			it.Status, it.Detail = p.conflict(), fmt.Sprintf("config.yaml has model %s; the import has %s", have.Model, a.Model)
			if it.Status == Conflict && toolsets(toolsHave, a.MCP) != nil {
				// The model stays; the MCP toolsets are only added.
				it.Detail += " (kept; the MCP toolsets are added)"
				setAgent(root, a, false, toolsHave)
				edited = true
			}
		}
		configured[a.Name] = true
		// An agent whose workspace was in the source home and is now copied.
		if have != nil && have.Workspace != a.Workspace && a.Workspace != "" && p.rw.str(have.Workspace) == a.Workspace {
			setWorkspace(root, a.Name, a.Workspace)
			edited = true
			if it.Status == Same {
				it.Status, it.Detail = Update, "its workspace becomes "+a.Workspace
			} else {
				it.Detail += "; its workspace becomes " + a.Workspace
			}
		}
		if it.Changes() {
			setAgent(root, a, it.Status == Replace, toolsHave)
			edited = true
		}
	}
	// Providers the configured agents need.
	need := map[string]bool{}
	for _, it := range p.Items {
		if it.Kind == "agent" && it.Changes() {
			a := f.agent(it.Agent)
			for _, m := range append([]string{a.Model}, a.Fallbacks...) {
				prov, _, _ := strings.Cut(m, "/")
				need[prov] = true
			}
		}
	}
	for _, pr := range f.Providers {
		if !need[pr.Name] {
			continue
		}
		it := p.add(&Item{Kind: "provider", Name: pr.Name, Status: New, Detail: providerDetail(pr)})
		if cfg.Providers[pr.Name] != nil {
			it.Status, it.Detail = Same, "kept as configured"
			continue
		}
		setProvider(root, pr)
		edited = true
	}
	for _, m := range f.MCP {
		detail := m.Command
		switch {
		case strings.HasPrefix(m.URL, "${"):
			detail = m.URL // the address is in .env
		case m.URL != "":
			detail = redactURL(m.URL)
		case m.Project != "":
			// A project's own server runs a command the project chose: show
			// all of it.
			detail = strings.Join(append([]string{shellQuote(m.Command)}, quoteAll(m.Args)...), " ") + " (from the project " + m.Project + ")"
		}
		it := p.add(&Item{Kind: "mcp", Name: m.Name, Status: New, Detail: detail})
		if have := cfg.MCP[m.Name]; have != nil {
			if sameServer(m, have) {
				it.Status = Same
				continue
			}
			if p.rw != nil && sameServerAs(m, have, p.rw.str(have.Command), p.rw.strs(have.Args)) {
				it.Status, it.Detail = Update, moved
				moveMCP(root, m)
				edited = true
				continue
			}
			it.Status, it.Detail = p.conflict(), "config.yaml has a different server by that name"
			if p.noteFix(it, append([]string{have.Command}, have.Args...)...); it.Fixes() {
				moveMCP(root, &MCPServer{Name: m.Name, Command: p.rw.str(have.Command), Args: p.rw.strs(have.Args)})
				edited = true
			}
		}
		if it.Changes() {
			setMCP(root, m)
			edited = true
		}
	}
	main := ""
	for _, a := range f.Agents {
		if a.Main && configured[a.Name] {
			main = a.Name
		}
	}
	for _, c := range f.Channels {
		it := p.add(&Item{Kind: "channel", Name: c.Name, Status: New, Detail: c.Type + " for agent " + c.Agent})
		if !configured[c.Agent] {
			if main == "" {
				it.Status, it.Detail = Rejected, "its agent "+c.Agent+" has no model yet"
				continue
			}
			it.Detail += ", answered by " + main + " (agent " + c.Agent + " has no model yet)"
			c.Agent = main
		}
		for conv, ag := range c.Routes {
			if !configured[ag] {
				delete(c.Routes, conv)
			}
		}
		if have := cfg.Channels[c.Name]; have != nil {
			if sameChannel(c, have) {
				it.Status = Same
				continue
			}
			it.Status, it.Detail = p.conflict(), "config.yaml has a different "+have.Type+" channel by that name"
		}
		if it.Changes() {
			setChannel(root, c)
			edited = true
		}
	}
	return edited
}

func providerDetail(p *Provider) string {
	switch {
	case p.Auth != "":
		return "sign in with hk auth login " + p.Auth
	case p.Preset != "" && p.BaseURL != "":
		return p.Preset + " at " + p.BaseURL
	case p.Preset != "":
		return "preset " + p.Preset
	}
	return p.Type + " at " + p.BaseURL
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func (f *Found) agent(name string) *Agent {
	for _, a := range f.Agents {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// planAgentFiles checks one agent's persona files, profile, notes and
// skills.
func (p *Plan) planAgentFiles(a *Agent) error {
	dir := filepath.Join(p.Home, "agents", a.Name)
	for _, name := range sortedKeys(a.Files) {
		text := a.Files[name]
		it := p.add(&Item{Kind: "file", Agent: a.Name, Name: name, Status: New, file: text})
		old, err := os.ReadFile(filepath.Join(dir, name))
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return err
		case strings.TrimSpace(string(old)) == strings.TrimSpace(text):
			it.Status = Same
		case strings.TrimSpace(p.rw.str(string(old))) == strings.TrimSpace(text):
			it.Status, it.Detail = Update, moved
		default:
			it.Status, it.Detail = p.conflict(), "the agent already has a different "+name
			p.noteFix(it, string(old))
		}
	}
	if a.Identity != nil {
		it := p.add(&Item{Kind: "file", Agent: a.Name, Name: agent.IdentityFile, Status: New, ident: a.Identity, Detail: a.Identity.Name})
		if cur, ok := agent.ReadIdentity(dir); ok {
			if cur == *a.Identity {
				it.Status = Same
			} else {
				it.Status, it.Detail = p.conflict(), "the agent already goes by "+cur.Name
			}
		}
	}
	if a.Profile != "" {
		p.planProfile(a, dir)
	}
	for i := range a.Notes {
		n := &a.Notes[i]
		it := p.add(&Item{Kind: "note", Agent: a.Name, Name: n.Title, Status: New, note: n})
		if err := memory.CheckSecrets(n.Title + "\n" + n.Body); err != nil {
			it.Status, it.Detail = Rejected, "it looks like it holds a key or password"
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "memory", memory.Slugify(n.Title)+".md"))
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return err
		default:
			var meta struct{}
			body, _ := frontmatter.Parse(data, &meta)
			switch want := strings.TrimSpace(n.Body); {
			case strings.TrimSpace(body) == want:
				it.Status = Same
			case strings.TrimSpace(p.rw.str(body)) == want:
				it.Status, it.Detail = Update, moved
			default:
				it.Status, it.Detail = p.conflict(), "a different note has that title"
				p.noteFix(it, body)
			}
		}
	}
	for i := range a.Skills {
		s := &a.Skills[i]
		dest := p.skillDir(a.Name, s)
		where := "agent " + a.Name
		switch {
		case s.Shared:
			where = "shared"
		case s.Held:
			where = "agent " + a.Name + ", from the " + s.Source + "; it needs your approval before it loads: hk skills approve -agent " + a.Name + " " + s.Name
		}
		it := p.add(&Item{Kind: "skill", Agent: a.Name, Name: s.Name, Status: New, Detail: where, skill: s})
		findings, err := skillScan(s)
		if err != nil {
			return err
		}
		findings = slices.DeleteFunc(findings, func(fd hub.Finding) bool {
			return slices.ContainsFunc(strings.Split(fd.File, "/"), skipCopy)
		})
		if len(findings) > 0 {
			it.Status = Rejected
			it.Detail = fmt.Sprintf("the security scan flagged %s; read it, then hk skills install %s", strings.Join(hub.Summaries(findings[:min(len(findings), 3)]), "; "), shellQuote(s.Dir))
			if s.Dir == "" {
				it.Detail = fmt.Sprintf("the security scan flagged %s; read the command it was made from, and add it as a skill yourself if you trust it", strings.Join(hub.Summaries(findings[:min(len(findings), 3)]), "; "))
			}
			continue
		}
		want, err := skillHash(s, p.rw)
		if err != nil {
			return err
		}
		have, err := treeHash(dest, nil)
		if err != nil {
			return err
		}
		switch have {
		case "":
		case want:
			it.Status = Same
		default:
			it.Status, it.Detail = p.conflict(), "a different skill by that name is in "+dest
			if p.rw == nil {
				break
			}
			// The same skill, imported before its files were copied?
			if after, err := treeHash(dest, p.rw); err != nil {
				return err
			} else if after == want {
				it.Status, it.Detail = Update, moved
				break
			}
			var texts []string
			if err := skillFiles(dest, func(_ string, data []byte) { texts = append(texts, string(data)) }); err != nil {
				return err
			}
			p.noteFix(it, texts...)
		}
	}
	return nil
}

func (p *Plan) skillDir(agentName string, s *Skill) string {
	if s.Shared {
		return filepath.Join(p.Home, "skills", s.Name)
	}
	return filepath.Join(p.Home, "agents", agentName, "skills", s.Name)
}

// planProfile checks USER.md section by section: sections are added, or
// their lines appended, never removed.
func (p *Plan) planProfile(a *Agent, dir string) {
	it := p.add(&Item{Kind: "profile", Agent: a.Name, Name: "USER.md", Status: New})
	old, _ := readText(filepath.Join(dir, "USER.md"))
	if old == "" {
		return
	}
	have := map[string]string{}
	for _, s := range splitSections(old) {
		have[strings.ToLower(s.name)] = s.body
	}
	it.Status = Same
	grow := 0
	for _, s := range splitSections(a.Profile) {
		b, ok := have[strings.ToLower(s.name)]
		switch {
		case ok && strings.Contains(b, s.body):
		case ok && strings.Contains(p.rw.str(b), s.body):
			// There already, from before its files were copied.
		default:
			it.Status, it.Detail = Update, "adds to the existing profile"
			grow += len(s.body) + len(s.name) + 8
		}
	}
	// Whatever the profile says about folders that are copied now is
	// rewritten where it is, the user's own lines included.
	for _, s := range splitSections(old) {
		if _, n := p.rw.apply(s.body); n > 0 && s.name != "" {
			it.inPlace = true
		}
	}
	if it.inPlace && it.Status == Same {
		it.Status, it.Detail = Update, moved
	}
	if it.Status == Update && len(old)+grow > memory.MaxProfile {
		it.Detail = "too long to add to USER.md; saved as a memory note instead"
	}
}

// planSchedules checks each schedule against the home's schedule table,
// read without changing anything.
func (p *Plan) planSchedules() error {
	if len(p.Found.Schedules) == 0 {
		return nil
	}
	type row struct{ id, spec, prompt, command, workdir string }
	existing := map[string]row{}
	path := filepath.Join(p.Home, "control.db")
	if exists(path) {
		q := url.Values{}
		q.Add("mode", "ro")
		q.Add("_pragma", "busy_timeout(10000)")
		db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
		if err != nil {
			return err
		}
		defer db.Close()
		rows, err := db.QueryContext(context.Background(), `SELECT id, agent, name, spec, prompt, command, workdir FROM schedules`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var r row
				var ag, name string
				if err := rows.Scan(&r.id, &ag, &name, &r.spec, &r.prompt, &r.command, &r.workdir); err != nil {
					return err
				}
				existing[ag+"\x00"+name] = r
			}
			if err := rows.Err(); err != nil {
				return err
			}
		} else if !strings.Contains(err.Error(), "no such table") {
			return fmt.Errorf("read schedules from %s: %w", path, err)
		}
	}
	for _, sc := range p.Found.Schedules {
		it := p.add(&Item{Kind: "schedule", Agent: sc.Agent, Name: sc.Name, Status: New, Detail: sc.Spec, schedule: sc})
		if r, ok := existing[sc.Agent+"\x00"+sc.Name]; ok {
			// A schedule from before its files were copied still names the
			// source home; it is changed where it is, so it keeps its ID and
			// stays on if it was resumed.
			stale := r.prompt != sc.Prompt || r.command != sc.Command || r.workdir != sc.Workdir
			after := p.rw.str(r.prompt) == sc.Prompt && p.rw.str(r.command) == sc.Command
			switch {
			case r.spec == sc.Spec && after && stale && p.rw.str(r.workdir) == sc.Workdir:
				it.Status, it.Detail, it.oldID, it.inPlace = Update, moved, r.id, true
			case r.spec == sc.Spec && r.prompt == sc.Prompt && r.command == sc.Command:
				it.Status = Same
			default:
				it.Status, it.Detail, it.oldID = p.conflict(), "a different schedule "+r.id+" has that name", r.id
				it.have = &Schedule{Prompt: r.prompt, Command: r.command, Workdir: r.workdir}
				p.noteFix(it, r.prompt, r.command, r.workdir)
			}
		}
	}
	return nil
}

// Count returns how many items of a kind have a status.
func (p *Plan) Count(kind, status string) int {
	n := 0
	for _, it := range p.Items {
		if it.Kind == kind && it.Status == status {
			n++
		}
	}
	return n
}

// Conflicts lists the items left alone because something different is
// there.
func (p *Plan) Conflicts() []*Item {
	var out []*Item
	for _, it := range p.Items {
		if it.Status == Conflict {
			out = append(out, it)
		}
	}
	return out
}

// Secrets lists the variable names the import found values for.
func (p *Plan) Secrets() []string { return sortedKeys(p.Found.Secrets) }

// Write prints the plan. copySecrets says what happens to keys: "copy",
// "skip" or "" (not decided yet). Values are never printed.
func (p *Plan) Write(w io.Writer, copySecrets string) {
	f := p.Found
	fmt.Fprintf(w, "Found %s at %s.\n", Label(f.From), f.Dir)
	fmt.Fprintf(w, "Importing into %s. + new, = already there, ~ added to, ! conflict (left alone), * replaced (--force), x not imported.\n", p.Home)
	mark := map[string]string{New: "+", Same: "=", Update: "~", Conflict: "!", Replace: "*", Rejected: "x"}
	line := func(it *Item, text string) {
		if it.Detail != "" {
			text += ": " + it.Detail
		}
		fmt.Fprintf(w, "  %s %s\n", mark[it.Status], text)
	}
	section := func(title, kind string, label func(*Item) string) {
		var items []*Item
		for _, it := range p.Items {
			if it.Kind == kind || kind == "file" && it.Kind == "profile" {
				items = append(items, it)
			}
		}
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(w, "\n%s\n", title)
		for _, it := range items {
			line(it, label(it))
		}
	}
	section("Agents", "agent", func(it *Item) string {
		a := f.agent(it.Agent)
		if a.From != it.Agent {
			return it.Agent + " (was " + a.From + ")"
		}
		return it.Agent
	})
	section("Persona and profile", "file", func(it *Item) string { return it.Agent + ": " + it.Name })
	p.writeNotes(w, mark)
	section("Skills", "skill", func(it *Item) string { return it.Name })
	section("Schedules (imported paused)", "schedule", func(it *Item) string { return it.Agent + ": " + it.Name })
	section("MCP servers", "mcp", func(it *Item) string { return it.Name })
	section("Model providers", "provider", func(it *Item) string { return it.Name })
	section("Channels", "channel", func(it *Item) string { return it.Name })
	section("Conversation history (imported read-only)", "history", func(it *Item) string { return it.Agent })
	p.writeFiles(w)
	if names := p.Secrets(); len(names) > 0 {
		fmt.Fprintf(w, "\nKeys and tokens (names only; values are never shown)\n")
		for _, it := range p.Items {
			if it.Kind == "secret" {
				line(it, it.Name)
			}
		}
		switch copySecrets {
		case "copy":
			fmt.Fprintf(w, "  They will be copied to %s (readable only by you).\n", filepath.Join(p.Home, ".env"))
		case "skip":
			fmt.Fprintf(w, "  They will not be copied (--secrets skip): set them in %s yourself.\n", filepath.Join(p.Home, ".env"))
		}
	}
	if len(f.Missing) > 0 {
		fmt.Fprintf(w, "\nVariables the config names but %s held no value for (set them yourself):\n  %s\n", Label(f.From), strings.Join(f.Missing, ", "))
	}
	if len(f.OtherEnv) > 0 {
		fmt.Fprintf(w, "\nOther variables in the old .env, not copied: %s\n", strings.Join(f.OtherEnv, ", "))
	}
	if len(f.Skipped) > 0 {
		fmt.Fprintf(w, "\nLeft out\n")
		for _, s := range f.Skipped {
			fmt.Fprintf(w, "  - %s\n", s)
		}
	}
	if len(f.Leftovers) > 0 {
		fmt.Fprintf(w, "\nCannot carry over\n")
		for _, l := range f.Leftovers {
			fmt.Fprintf(w, "  - %s: %s\n", l.What, l.Instead)
		}
	}
	switch {
	case p.ConfigErr != nil:
		fmt.Fprintf(w, "\nProblem: %v\n", p.ConfigErr)
	case p.NoConfig != "":
		fmt.Fprintf(w, "\nNote: %s.\n", p.NoConfig)
	case p.Config != nil && p.OldConfig != nil:
		fmt.Fprintf(w, "\nconfig.yaml will be edited in place (the current one is kept as config.yaml.bak).\n")
	case p.Config != nil:
		fmt.Fprintf(w, "\nconfig.yaml will be created.\n")
	}
	if n := len(p.Conflicts()); n > 0 {
		fmt.Fprintf(w, "%d conflicts are left as they are; --force replaces them.\n", n)
	}
}

// writeNotes summarises memory notes per agent, listing only the ones that
// need attention.
func (p *Plan) writeNotes(w io.Writer, mark map[string]string) {
	type tally struct{ n, same int }
	counts := map[string]*tally{}
	var order []string
	var odd []*Item
	for _, it := range p.Items {
		if it.Kind != "note" {
			continue
		}
		t := counts[it.Agent]
		if t == nil {
			t = &tally{}
			counts[it.Agent] = t
			order = append(order, it.Agent)
		}
		switch it.Status {
		case New, Replace:
			t.n++
		case Same:
			t.same++
		}
		if it.Status != New && it.Status != Same {
			odd = append(odd, it)
		}
	}
	if len(order) == 0 {
		return
	}
	fmt.Fprintf(w, "\nMemory notes\n")
	for _, a := range order {
		t := counts[a]
		m := "+"
		if t.n == 0 {
			m = "="
		}
		fmt.Fprintf(w, "  %s %s: %d to import, %d already there\n", m, a, t.n, t.same)
	}
	for _, it := range odd {
		fmt.Fprintf(w, "  %s %s: %q: %s\n", mark[it.Status], it.Agent, it.Name, it.Detail)
	}
}

func quoteAll(list []string) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = shellQuote(s)
	}
	return out
}

// withoutKeyed returns f without its keyed MCP servers when drop is set,
// and those servers: a copy, so the same Found can be planned again with
// another answer. Their variables leave the keys to copy and to set.
func (f *Found) withoutKeyed(drop bool) (*Found, []*MCPServer) {
	c := *f
	c.Leftovers = slices.Clone(f.Leftovers) // the plan adds its own
	defer func() {
		for _, m := range c.MCP {
			if m.OAuth {
				c.leftover("the sign-in to MCP server "+m.Name, "hk mcp login "+m.Name)
			}
		}
	}()
	if !drop || !slices.ContainsFunc(f.MCP, func(m *MCPServer) bool { return len(m.Keyed) > 0 }) {
		return &c, nil
	}
	var keyed []*MCPServer
	gone := map[string]bool{}
	needed := map[string]bool{}
	c.MCP = nil
	for _, m := range f.MCP {
		vars := append(slices.Clone(m.Keyed), sortedValues(m.Env)...)
		for _, h := range sortedValues(m.Headers) {
			vars = append(vars, mcpHeaderVars(h)...)
		}
		if len(m.Keyed) > 0 {
			keyed = append(keyed, m)
			for _, v := range vars {
				gone[v] = true
			}
			continue
		}
		c.MCP = append(c.MCP, m)
		for _, v := range vars {
			needed[v] = true
		}
	}
	c.Agents = make([]*Agent, len(f.Agents))
	for i, a := range f.Agents {
		b := *a
		b.MCP = slices.DeleteFunc(slices.Clone(a.MCP), func(n string) bool {
			return slices.ContainsFunc(keyed, func(m *MCPServer) bool { return m.Name == n })
		})
		c.Agents[i] = &b
	}
	drop2 := func(v string) bool { return gone[v] && !needed[v] }
	if f.Secrets != nil {
		c.Secrets = map[string]string{}
		for k, v := range f.Secrets {
			if !drop2(k) {
				c.Secrets[k] = v
			}
		}
	}
	c.Missing = slices.DeleteFunc(slices.Clone(f.Missing), drop2)
	return &c, keyed
}

func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, m[k])
	}
	return out
}

// mcpHeaderVars lists the variables a header names: a bare name, or the
// references in a template.
func mcpHeaderVars(spec string) []string {
	if !strings.Contains(spec, "${") {
		return []string{spec}
	}
	return varRefs(spec)
}
