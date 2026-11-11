package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/migrate"
	"github.com/habakkuk-ai/habakkuk/internal/migrate/cutover"
)

// `hk migrate` moves a setup from another agent or a coding tool into
// Habakkuk. The work is in internal/migrate; this is the command: flags,
// finding the source, the plan, the confirmation and the closing report.

const migrateUsage = `usage: hk migrate [--from hermes|openclaw|claude-code|codex] [--source DIR]
                        [--agent NAME] [--project DIR] [--dry-run]
                        [--secrets copy|skip] [--history copy|skip]
                        [--files copy|skip] [--files-dir DIR] [--files-max SIZE]
                        [--force] [-y]

Moves an existing Hermes Agent or OpenClaw setup into Habakkuk: agents and
their persona files, the user profile, memory notes, past conversations
(as read-only sessions), skills, scheduled tasks (imported paused), MCP
servers, model providers, channels and, when you say so, their keys and
tokens. It prints a plan first and writes nothing until you confirm. Running
it again changes nothing that is already there; things that differ are left
alone unless --force.

From Claude Code or Codex it brings the instruction files, MCP servers,
skills, slash commands (as skills) and memory files. Their keys and sign-ins
are never copied. --project DIR adds one project's own instructions, MCP
servers and skills.

Once the import looks right, hk migrate finish stops the old agent, pauses
its schedules and resumes the imported ones, and hk migrate rollback undoes
that (see hk migrate finish -h).

The imported text often names files in the old folder: scripts that schedules
run, folders of notes and reports. The plan lists them. With --files copy your
own files are copied into $HABAKKUK_HOME/files and the imported text is
rewritten to name the copies, so the old folder is no longer needed for them.
The old agent's own parts (its program, logs, sessions, keys) are never
copied, and the old folder is only ever read.

Flags:
`

type migrateFlags struct {
	from, source, agent, secrets string
	project                      string
	files, filesDir, filesMax    string
	history                      string
	dryRun, force, yes           bool
	userHome                     string // tests point the source lookup elsewhere
	// freeSpace replaces the file system's answer in tests.
	freeSpace func(string) (uint64, error)
}

func migrateCmd(ctx context.Context, home string, args []string) error {
	return runMigrate(ctx, home, args, os.Stdin, os.Stdout, os.Getenv, "")
}

// runMigrate parses the flags and runs an import. userHome, when set,
// replaces the user's home directory for finding the source.
func runMigrate(ctx context.Context, home string, args []string, in io.Reader, out io.Writer, getenv func(string) string, userHome string) error {
	if len(args) > 0 && (args[0] == "finish" || args[0] == "rollback") {
		return runCutover(ctx, home, args[0], args[1:], in, out, getenv, userHome)
	}
	var fl migrateFlags
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&fl.from, "from", "", "the agent to import from: "+strings.Join(migrate.Sources, " or ")+" (default: the one found on this computer)")
	fs.StringVar(&fl.source, "source", "", "its home folder (default ~/.hermes or $HERMES_HOME; ~/.openclaw or $OPENCLAW_STATE_DIR; ~/.claude or $CLAUDE_CONFIG_DIR; ~/.codex or $CODEX_HOME)")
	fs.StringVar(&fl.project, "project", "", "claude-code and codex: also bring this project folder's instructions (as a memory note), approved MCP servers and skills")
	fs.StringVar(&fl.agent, "agent", "", `the Habakkuk agent the main agent becomes (default "habakkuk"); other agents keep their names`)
	fs.BoolVar(&fl.dryRun, "dry-run", false, "print the plan and stop; nothing is written")
	fs.StringVar(&fl.secrets, "secrets", "", "copy: put API keys and tokens in $HABAKKUK_HOME/.env; skip: only list their names (default: ask)")
	fs.StringVar(&fl.files, "files", "", "copy: copy your files that the imported text names out of the old folder and rewrite the text to name the copies; skip: leave them (default: ask)")
	fs.StringVar(&fl.filesDir, "files-dir", "", "where --files copy puts them (default $HABAKKUK_HOME/files)")
	fs.StringVar(&fl.filesMax, "files-max", "", "the largest file --files copy takes, such as 500M or 4G; 0 is no limit (default 1G)")
	fs.StringVar(&fl.history, "history", "", "copy: import past conversations as read-only sessions; skip: leave them (default copy)")
	fs.BoolVar(&fl.force, "force", false, "replace agents, files, skills, notes, schedules and .env values that already exist and differ")
	fs.BoolVar(&fl.yes, "y", false, "do not ask: import without confirming (keys are copied only with --secrets copy, files only with --files copy)")
	fs.BoolVar(&fl.yes, "yes", false, "same as -y")
	fs.Usage = func() {
		fmt.Fprint(out, migrateUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q; see hk migrate -h", fs.Arg(0))
	}
	if fl.secrets != "" && fl.secrets != "copy" && fl.secrets != "skip" {
		return fmt.Errorf("--secrets %q: want copy or skip", fl.secrets)
	}
	if fl.files != "" && fl.files != migrate.FilesCopy && fl.files != migrate.FilesSkip {
		return fmt.Errorf("--files %q: want copy or skip", fl.files)
	}
	if fl.history != "" && fl.history != migrate.HistoryCopy && fl.history != migrate.HistorySkip {
		return fmt.Errorf("--history %q: want copy or skip", fl.history)
	}
	if fl.filesMax != "" {
		if _, err := migrate.ParseSize(fl.filesMax); err != nil {
			return fmt.Errorf("--files-max: %w", err)
		}
	}
	if fl.from != "" && !slices.Contains(migrate.Sources, fl.from) {
		return fmt.Errorf("--from %q: want %s", fl.from, strings.Join(migrate.Sources, " or "))
	}
	if strings.ContainsAny(fl.agent, `/\ :`) || fl.agent == "." || fl.agent == ".." {
		return fmt.Errorf("--agent %q is not a valid agent name", fl.agent)
	}
	if fl.project != "" {
		abs, err := filepath.Abs(fl.project)
		if err != nil {
			return err
		}
		if info, err := os.Stat(abs); err != nil || !info.IsDir() {
			return fmt.Errorf("--project %s: not a folder", fl.project)
		}
		fl.project = abs
	}
	fl.userHome = userHome
	m := &migration{fl: fl, home: home, in: bufio.NewReader(in), out: out, getenv: getenv}
	return m.run(ctx)
}

type migration struct {
	fl     migrateFlags
	home   string
	in     *bufio.Reader
	out    io.Writer
	getenv func(string) string
	// inSetup: hk setup runs the import and says what comes next.
	inSetup bool
}

func (m *migration) say(format string, args ...any) { fmt.Fprintf(m.out, format+"\n", args...) }

func (m *migration) run(ctx context.Context) error {
	o := migrate.Options{From: m.fl.from, Source: m.fl.source, Agent: m.fl.agent, Force: m.fl.force,
		Getenv: m.getenv, UserHome: m.fl.userHome, Files: m.fl.files, FilesDir: m.fl.filesDir, FreeSpace: m.fl.freeSpace,
		History: m.fl.history, Project: m.fl.project}
	if m.fl.filesMax != "" {
		o.FilesMax, _ = migrate.ParseSize(m.fl.filesMax) // checked with the flags
	}
	// Without --files a person is asked once, so the plan shows the copy;
	// with -y the files are left where they are.
	o.FilesAsk = m.fl.files == "" && !m.fl.yes
	// MCP servers whose command line, URL or headers hold keys come along
	// only with the keys. A person who has not answered yet sees them in
	// the plan, and the plan is made again without them on a no.
	o.Secrets = m.fl.secrets
	if o.Secrets == "" && !m.fl.yes {
		o.Secrets = "copy"
	}
	if o.From == "" {
		from, err := m.pick(o)
		if err != nil {
			return err
		}
		o.From = from
	}
	found, err := migrate.Read(o)
	if err != nil {
		return err
	}
	defer func() { _ = found.Close() }()
	if o.Project != "" {
		if !migrate.IsCodingTool(o.From) {
			return errors.New("--project is for --from claude-code and --from codex")
		}
		m.say("Adding the project in %s.", o.Project)
	}
	plan, err := migrate.NewPlan(found, m.home, o)
	if err != nil {
		return err
	}
	plan.Write(m.out, m.fl.secrets)
	m.cutoverSection(ctx, found)
	if plan.ConfigErr != nil {
		return errors.New("nothing was written; fix the problem above (or report it), then run hk migrate again")
	}
	if m.fl.dryRun {
		m.say("\nDry run: nothing was written.")
		return nil
	}
	if fp := plan.Files; fp.Copies() && fp.Err != nil {
		if !fp.Ask {
			return errors.New("nothing was written; fix the problem above, or run again with --files skip")
		}
		// Nobody chose the copy: go on without it.
		m.say("\nYour files cannot be copied now, so they stay where they are; hk migrate --files copy brings them over later.")
		o.FilesAsk = false
		if plan, err = migrate.NewPlan(found, m.home, o); err != nil {
			return err
		}
	}
	changes := 0
	var keys []string
	for _, it := range plan.Items {
		switch {
		case it.Kind == "secret" && it.Changes():
			keys = append(keys, it.Name)
		case it.Changes(), it.Fixes():
			changes++
		}
	}
	// Keys count as work only when this run may copy them.
	mayCopy := m.fl.secrets == "copy" || m.fl.secrets == "" && !m.fl.yes
	if changes == 0 && !plan.Files.Pending() && (len(keys) == 0 || !mayCopy) {
		m.say("\nNothing to import: everything is already there.")
		if len(keys) > 0 {
			m.say("Still to set in %s: %s (or run again with --secrets copy).", filepath.Join(m.home, ".env"), strings.Join(keys, ", "))
		}
		if w := plan.Files.Waiting(); len(w) > 0 {
			m.say("Your files that the imported text names are still in %s: run again with --files copy to bring them over.", plan.Found.Dir)
		}
		return nil
	}
	if !m.fl.yes {
		ok, err := m.ask("\nImport now?", true)
		if err != nil {
			return err
		}
		if !ok {
			m.say("Nothing was written.")
			return nil
		}
	}
	copyKeys := m.fl.secrets == "copy"
	if m.fl.secrets == "" && !m.fl.yes && len(keys) > 0 {
		if copyKeys, err = m.ask(fmt.Sprintf("Copy the %d keys and tokens into %s (readable only by you)?", len(keys), filepath.Join(m.home, ".env")), true); err != nil {
			return err
		}
		if !copyKeys {
			o.Secrets = "skip"
			if plan, err = migrate.NewPlan(found, m.home, o); err != nil {
				return err
			}
			var left []string
			for _, it := range plan.Items {
				if it.Kind == "mcp" && it.Status == migrate.Rejected && strings.Contains(it.Detail, "--secrets copy") {
					left = append(left, it.Name)
				}
			}
			if len(left) > 0 {
				m.say("Without the keys, these MCP servers are left out: %s.", strings.Join(left, ", "))
			}
		}
	}
	if fp := plan.Files; o.FilesAsk && fp.Pending() {
		var names []string
		for _, e := range fp.Waiting() {
			names = append(names, e.Name)
		}
		q := fmt.Sprintf("Bring the files that changed in %s over to %s (%s)?", plan.Found.Dir, fp.Dir, migrate.HumanSize(fp.Need))
		if len(names) > 0 {
			q = fmt.Sprintf("Copy your files (%s: %s) into %s and point the imported text at the copies?",
				strings.Join(names, ", "), migrate.HumanSize(fp.Need), fp.Dir)
		}
		ok, err := m.ask(q, true)
		if err != nil {
			return err
		}
		if !ok {
			o.Files, o.FilesAsk = migrate.FilesSkip, false
			if plan, err = migrate.NewPlan(found, m.home, o); err != nil {
				return err
			}
		}
	}
	if fp := plan.Files; fp.Copies() && fp.Need > 0 {
		m.say("\nCopying %s into %s ...", migrate.HumanSize(fp.Need), fp.Dir)
	}
	res, err := plan.Apply(ctx, copyKeys)
	if res != nil {
		m.report(plan, res)
	}
	return err
}

// pick finds the source when --from is not given: the one present. An
// agent found beside coding tools is the one picked; the others are named.
func (m *migration) pick(o migrate.Options) (string, error) {
	var found []string
	if o.Source != "" {
		for _, from := range migrate.Sources {
			if migrate.Present(from, o.Source) {
				found = append(found, from)
			}
		}
	} else {
		found = migrate.Detect(o)
	}
	if len(found) == 0 {
		var where []string
		for _, from := range migrate.Sources {
			where = append(where, fmt.Sprintf("%s (%s)", migrate.Label(from), migrate.DefaultSource(from, o)))
		}
		if o.Source != "" {
			return "", fmt.Errorf("%s does not look like the home of %s; pass --from", o.Source, orList(labels(migrate.Sources)))
		}
		return "", fmt.Errorf("found no setup to import: looked for %s; pass --from and --source", orList(where))
	}
	from, others, err := choose(found)
	if err != nil {
		return "", err
	}
	if len(others) > 0 {
		var hints []string
		for _, o := range others {
			hints = append(hints, "--from "+o)
		}
		m.say("Found %s too; run hk migrate again with %s to import it.", orList(labels(others)), orList(hints))
	}
	return from, nil
}

// choose picks one of the sources found: the only agent, or the only
// coding tool when there is no agent. Two of a kind need --from.
func choose(found []string) (string, []string, error) {
	var agents, tools []string
	for _, f := range found {
		if migrate.IsCodingTool(f) {
			tools = append(tools, f)
		} else {
			agents = append(agents, f)
		}
	}
	tier := agents
	if len(tier) == 0 {
		tier = tools
	}
	if len(tier) > 1 {
		var flags []string
		for _, f := range tier {
			flags = append(flags, "--from "+f)
		}
		if len(tier) == 2 {
			return "", nil, fmt.Errorf("found both %s and %s; pass %s", migrate.Label(tier[0]), migrate.Label(tier[1]), orList(flags))
		}
		return "", nil, fmt.Errorf("found %s; pass %s", andList(labels(tier)), orList(flags))
	}
	var others []string
	for _, f := range found {
		if f != tier[0] {
			others = append(others, f)
		}
	}
	return tier[0], others, nil
}

func labels(froms []string) []string {
	var out []string
	for _, f := range froms {
		out = append(out, migrate.Label(f))
	}
	return out
}

func orList(s []string) string  { return joinList(s, "or") }
func andList(s []string) string { return joinList(s, "and") }

func joinList(s []string, word string) string {
	switch len(s) {
	case 0:
		return ""
	case 1:
		return s[0]
	}
	return strings.Join(s[:len(s)-1], ", ") + " " + word + " " + s[len(s)-1]
}

// ask is a yes/no question; end of input is no.
func (m *migration) ask(prompt string, def bool) (bool, error) {
	d := "y/N"
	if def {
		d = "Y/n"
	}
	fmt.Fprintf(m.out, "%s (%s) ", prompt, d)
	line, err := m.in.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(m.out)
			return false, nil
		}
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// report says what was done and what is next.
func (m *migration) report(p *migrate.Plan, res *migrate.Result) {
	kinds := map[string]int{}
	for _, it := range res.Done {
		kinds[it.Kind]++
	}
	var parts []string
	for _, k := range []struct{ kind, one, many string }{
		{"agent", "agent", "agents"}, {"file", "persona file", "persona files"}, {"profile", "profile", "profiles"},
		{"note", "memory note", "memory notes"}, {"skill", "skill", "skills"}, {"schedule", "schedule", "schedules"},
		{"mcp", "MCP server", "MCP servers"}, {"provider", "provider", "providers"}, {"channel", "channel", "channels"},
	} {
		switch n := kinds[k.kind]; n {
		case 0:
		case 1:
			parts = append(parts, "1 "+k.one)
		default:
			parts = append(parts, fmt.Sprintf("%d %s", n, k.many))
		}
	}
	if res.Conversations > 0 {
		parts = append(parts, fmt.Sprintf("%s (%s) as read-only sessions", count(res.Conversations, "past conversation", "past conversations"),
			count(res.Messages, "message", "messages")))
	}
	m.say("")
	if len(parts) == 0 {
		m.say("Imported nothing new.")
	} else {
		m.say("Imported %s.", strings.Join(parts, ", "))
	}
	if res.WroteCfg && p.OldConfig != nil {
		m.say("config.yaml was edited; the previous one is config.yaml.bak.")
	}
	if len(res.Copied) > 0 {
		m.say("Copied %s into %s.", strings.Join(res.Copied, ", "), filepath.Join(m.home, ".env"))
	}
	if len(res.ToSet) > 0 {
		m.say("Set these in %s yourself: %s.", filepath.Join(m.home, ".env"), strings.Join(res.ToSet, ", "))
	}
	for _, it := range p.Items {
		if err := res.Failed[it]; err != nil {
			m.say("Could not import %s %s: %v", it.Kind, it.Name, err)
		}
	}
	m.reportFiles(p, res)
	if len(res.Paused) > 0 {
		if cutover.Runs(p.Found.From) {
			m.say("\nSchedules are imported paused, so nothing runs twice. Check them with hk schedules list.")
			m.say("When you're ready to switch, hk migrate finish --from %s stops %s, pauses its schedules and resumes these:",
				p.Found.From, migrate.Label(p.Found.From))
		} else {
			m.say("\nSchedules are imported paused. Check them with hk schedules list, then resume each one:")
		}
		for _, k := range sortedNames(res.Paused) {
			m.say("  hk schedules resume %s    # %s", res.Paused[k], k)
		}
	}
	if conflicts := p.Conflicts(); len(conflicts) > 0 {
		m.say("\nLeft as they were (%d conflicts): run again with --force to replace them.", len(conflicts))
		if n := len(res.Fixed); n > 0 {
			m.say("In %d of them only the references to the copied files were rewritten.", n)
		}
	}
	if len(p.Found.Leftovers) > 0 {
		m.say("\nStill to do by hand:")
		for _, l := range p.Found.Leftovers {
			m.say("  - %s: %s", l.What, l.Instead)
		}
	}
	if m.inSetup {
		m.say("\nSetup goes on from what was imported; Enter keeps each imported setting.")
		return
	}
	m.say("\nNext:")
	if !res.WroteCfg && p.OldConfig == nil {
		m.say("  hk setup       pick a model and where to talk")
	}
	m.say("  hk doctor      check the config, keys and channels")
	if cutover.Runs(p.Found.From) {
		m.say("  hk migrate finish    stop %s, pause its schedules and resume the imported ones", migrate.Label(p.Found.From))
	}
	m.say("  hk serve       start the agents and connect the channels")
	switch fp := p.Files; {
	case fp == nil:
	case len(fp.Waiting()) > 0 && !fp.Copies():
		m.say("  hk migrate --files copy    bring your files over from %s, so the new agent stops using that folder", p.Found.Dir)
	default:
		m.say("  hk migrate --dry-run       see what still points into %s before you remove it", p.Found.Dir)
	}
	m.say("Read each agent's SOUL.md and AGENTS.md in %s: text written for the old agent may name tools Habakkuk does not have.", filepath.Join(m.home, "agents"))
}

// reportFiles says what was copied out of the source home, how many
// references now name the copies, and what still points into it.
func (m *migration) reportFiles(p *migrate.Plan, res *migrate.Result) {
	fp := p.Files
	if fp == nil {
		return
	}
	src := p.Found.Dir
	m.say("")
	inside := ""
	if fr := res.Files; fr != nil {
		t := fr.Totals()
		var names []string
		for _, e := range fr.Entries {
			names = append(names, e.Name)
		}
		switch n := t.Copy + t.MakeLinks; {
		case n > 0:
			m.say("Copied %s (%s) out of %s into %s: %s.", count(n, "file", "files"), migrate.HumanSize(t.CopyBytes), src, fr.Dir, strings.Join(names, ", "))
		case len(names) > 0:
			m.say("Your files are in %s: %s. Nothing new had to be copied.", fr.Dir, strings.Join(names, ", "))
		}
		if n := len(t.Unreadable); n > 0 {
			m.say("Could not copy %s (the old folder would not let them be read, or they are links this system cannot make): %s.",
				count(n, "path", "paths"), firstFew(t.Unreadable, 5))
		}
		for _, e := range fr.Entries {
			if st := fr.Stats[e.Name]; st != nil && len(st.TooBig) > 0 {
				m.say("Left out of %s, over the --files-max limit: %s.", e.Name, firstFew(st.TooBig, 5))
			}
		}
		if t.References > 0 {
			inside = fmt.Sprintf("%d inside %s", t.References, count(t.Rewritten, "copied file", "copied files"))
		}
	}
	refs := map[string]int{}
	total := 0
	for _, it := range append(slices.Clone(res.Done), res.Fixed...) {
		refs[it.Kind] += it.Rewritten()
		total += it.Rewritten()
	}
	var parts []string
	for _, k := range []struct{ kind, where string }{
		{"file", "persona files"}, {"profile", "the profile"}, {"note", "memory notes"}, {"skill", "skills"},
		{"schedule", "schedules"}, {"mcp", "MCP servers"}, {"agent", "workspace settings"},
	} {
		if n := refs[k.kind]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d in %s", n, k.where))
		}
	}
	if inside != "" {
		parts = append(parts, inside)
		total += res.Files.Totals().References
	}
	if total > 0 {
		m.say("Rewrote %s to name the copies: %s.", count(total, "reference", "references"), strings.Join(parts, ", "))
	}
	still := p.StillPoints()
	switch {
	case len(still) > 0:
		m.say("Still points into %s (that folder is needed while these remain):", src)
		for _, s := range still {
			m.say("  - %s: named %s (%s): %s", s.Name, migrate.Times(s.Refs), firstFew(s.Where, 4), s.Why)
		}
		if len(fp.Waiting()) > 0 && !fp.Copies() {
			m.say("hk migrate --files copy copies your own files into %s and rewrites the references.", fp.Dir)
		}
	case len(res.Failed) == 0 && len(p.Conflicts()) == 0:
		m.say("The imported text no longer names anything in %s.", src)
	}
}

// count is "1 file" or "3 files".
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// firstFew joins the first n of a list and says how many more there are.
func firstFew(list []string, n int) string {
	if len(list) <= n {
		return strings.Join(list, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(list[:n], ", "), len(list)-n)
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
