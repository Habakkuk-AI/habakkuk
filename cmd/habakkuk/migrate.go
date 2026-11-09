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
)

// `habakkuk migrate` moves a setup from Hermes Agent or OpenClaw into
// Habakkuk. The work is in internal/migrate; this is the command: flags,
// finding the source, the plan, the confirmation and the closing report.

const migrateUsage = `usage: habakkuk migrate [--from hermes|openclaw] [--source DIR] [--agent NAME]
                        [--dry-run] [--secrets copy|skip] [--force] [-y]

Moves an existing Hermes Agent or OpenClaw setup into Habakkuk: agents and
their persona files, the user profile, memory notes, skills, scheduled tasks
(imported paused), MCP servers, model providers, channels and, when you say
so, their keys and tokens. It prints a plan first and writes nothing until you
confirm. Running it again changes nothing that is already there; things that
differ are left alone unless --force.

Flags:
`

type migrateFlags struct {
	from, source, agent, secrets string
	dryRun, force, yes           bool
	userHome                     string // tests point the source lookup elsewhere
}

func migrateCmd(ctx context.Context, home string, args []string) error {
	return runMigrate(ctx, home, args, os.Stdin, os.Stdout, os.Getenv, "")
}

// runMigrate parses the flags and runs an import. userHome, when set,
// replaces the user's home directory for finding the source.
func runMigrate(ctx context.Context, home string, args []string, in io.Reader, out io.Writer, getenv func(string) string, userHome string) error {
	var fl migrateFlags
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&fl.from, "from", "", "the agent to import from: "+strings.Join(migrate.Sources, " or ")+" (default: the one found on this computer)")
	fs.StringVar(&fl.source, "source", "", "its home folder (default ~/.hermes or $HERMES_HOME; ~/.openclaw or $OPENCLAW_STATE_DIR)")
	fs.StringVar(&fl.agent, "agent", "", `the Habakkuk agent the main agent becomes (default "habakkuk"); other agents keep their names`)
	fs.BoolVar(&fl.dryRun, "dry-run", false, "print the plan and stop; nothing is written")
	fs.StringVar(&fl.secrets, "secrets", "", "copy: put API keys and tokens in $HABAKKUK_HOME/.env; skip: only list their names (default: ask)")
	fs.BoolVar(&fl.force, "force", false, "replace agents, files, skills, notes, schedules and .env values that already exist and differ")
	fs.BoolVar(&fl.yes, "y", false, "do not ask: import without confirming (keys are copied only with --secrets copy)")
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
		return fmt.Errorf("unexpected argument %q; see habakkuk migrate -h", fs.Arg(0))
	}
	if fl.secrets != "" && fl.secrets != "copy" && fl.secrets != "skip" {
		return fmt.Errorf("--secrets %q: want copy or skip", fl.secrets)
	}
	if fl.from != "" && !slices.Contains(migrate.Sources, fl.from) {
		return fmt.Errorf("--from %q: want %s", fl.from, strings.Join(migrate.Sources, " or "))
	}
	if strings.ContainsAny(fl.agent, `/\ :`) || fl.agent == "." || fl.agent == ".." {
		return fmt.Errorf("--agent %q is not a valid agent name", fl.agent)
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
	// inSetup: habakkuk setup runs the import and says what comes next.
	inSetup bool
}

func (m *migration) say(format string, args ...any) { fmt.Fprintf(m.out, format+"\n", args...) }

func (m *migration) run(ctx context.Context) error {
	o := migrate.Options{From: m.fl.from, Source: m.fl.source, Agent: m.fl.agent, Force: m.fl.force,
		Getenv: m.getenv, UserHome: m.fl.userHome}
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
	plan, err := migrate.NewPlan(found, m.home, o)
	if err != nil {
		return err
	}
	plan.Write(m.out, m.fl.secrets)
	if plan.ConfigErr != nil {
		return errors.New("nothing was written; fix the problem above (or report it), then run habakkuk migrate again")
	}
	if m.fl.dryRun {
		m.say("\nDry run: nothing was written.")
		return nil
	}
	changes := 0
	var keys []string
	for _, it := range plan.Items {
		switch {
		case it.Kind == "secret" && it.Changes():
			keys = append(keys, it.Name)
		case it.Changes():
			changes++
		}
	}
	// Keys count as work only when this run may copy them.
	mayCopy := m.fl.secrets == "copy" || m.fl.secrets == "" && !m.fl.yes
	if changes == 0 && (len(keys) == 0 || !mayCopy) {
		m.say("\nNothing to import: everything is already there.")
		if len(keys) > 0 {
			m.say("Still to set in %s: %s (or run again with --secrets copy).", filepath.Join(m.home, ".env"), strings.Join(keys, ", "))
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
	}
	res, err := plan.Apply(ctx, copyKeys)
	if res != nil {
		m.report(plan, res)
	}
	return err
}

// pick finds the source when --from is not given: the one present.
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
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		var where []string
		for _, from := range migrate.Sources {
			where = append(where, fmt.Sprintf("%s (%s)", migrate.Label(from), migrate.DefaultSource(from, o)))
		}
		if o.Source != "" {
			return "", fmt.Errorf("%s does not look like a %s or %s home; pass --from", o.Source, migrate.Label(migrate.Hermes), migrate.Label(migrate.OpenClaw))
		}
		return "", fmt.Errorf("found no setup to import: looked for %s; pass --from and --source", strings.Join(where, " and "))
	}
	return "", fmt.Errorf("found both %s and %s; pass --from %s or --from %s", migrate.Label(found[0]), migrate.Label(found[1]), found[0], found[1])
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
	count := map[string]int{}
	for _, it := range res.Done {
		count[it.Kind]++
	}
	var parts []string
	for _, k := range []struct{ kind, one, many string }{
		{"agent", "agent", "agents"}, {"file", "persona file", "persona files"}, {"profile", "profile", "profiles"},
		{"note", "memory note", "memory notes"}, {"skill", "skill", "skills"}, {"schedule", "schedule", "schedules"},
		{"mcp", "MCP server", "MCP servers"}, {"provider", "provider", "providers"}, {"channel", "channel", "channels"},
	} {
		switch n := count[k.kind]; n {
		case 0:
		case 1:
			parts = append(parts, "1 "+k.one)
		default:
			parts = append(parts, fmt.Sprintf("%d %s", n, k.many))
		}
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
	if len(res.Paused) > 0 {
		m.say("\nSchedules are imported paused. Check them with habakkuk schedules list, then resume each one:")
		for _, k := range sortedNames(res.Paused) {
			m.say("  habakkuk schedules resume %s    # %s", res.Paused[k], k)
		}
	}
	if conflicts := p.Conflicts(); len(conflicts) > 0 {
		m.say("\nLeft as they were (%d conflicts): run again with --force to replace them.", len(conflicts))
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
		m.say("  habakkuk setup       pick a model and where to talk")
	}
	m.say("  habakkuk doctor      check the config, keys and channels")
	m.say("  habakkuk serve       start the agents and connect the channels")
	m.say("Read each agent's SOUL.md and AGENTS.md in %s: text written for the old agent may name tools Habakkuk does not have.", filepath.Join(m.home, "agents"))
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
