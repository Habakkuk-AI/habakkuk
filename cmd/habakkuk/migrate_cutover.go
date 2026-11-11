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

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/migrate"
	"github.com/habakkuk-ai/habakkuk/internal/migrate/cutover"
)

// hk migrate finish ends a move from another agent with one agent
// running: it stops the old agent (its services and processes), pauses
// its schedules in its own store, and resumes the schedules hk migrate
// imported paused. hk migrate rollback undoes it from the record finish
// keeps. The work is in internal/migrate/cutover.

// newCutoverSystem is the computer the cutover reads and changes; tests
// replace it with a fake.
var newCutoverSystem = func() cutover.System { return cutover.Host{} }

const finishUsage = `usage: hk migrate finish [--from hermes|openclaw] [--source DIR] [--agent NAME]
                        [--dry-run] [--force] [-y]

Ends a move from Hermes Agent or OpenClaw with only Habakkuk running. It
shows what of the old agent still runs, then, once you confirm:

  1. stops and disables its background services (systemd, launchd or a
     Windows scheduled task), so they don't start again at login;
  2. stops its processes that are still running: the gateway, workers and
     channel bridges;
  3. pauses its own schedules in its own store (paused, never deleted);
  4. resumes the schedules hk migrate imported paused.

It refuses step 4 while the old agent's scheduler still runs, unless
--force. Everything it changes is recorded in
$HABAKKUK_HOME/migrate/cutover-<from>.json, and hk migrate rollback
puts it back.

Flags:
`

const rollbackUsage = `usage: hk migrate rollback [--from hermes|openclaw] [-y]

Undoes hk migrate finish from its record: pauses the imported schedules
again, turns the old agent's schedules back on, and enables and starts its
services again.

Flags:
`

// runCutover runs hk migrate finish or rollback.
func runCutover(ctx context.Context, home, verb string, args []string, in io.Reader, out io.Writer, getenv func(string) string, userHome string) error {
	fs := flag.NewFlagSet("migrate "+verb, flag.ContinueOnError)
	fs.SetOutput(out)
	var from, source, agent string
	var dryRun, force, yes bool
	fs.StringVar(&from, "from", "", "the old agent: hermes or openclaw (default: the one found on this computer)")
	if verb == "finish" {
		fs.StringVar(&source, "source", "", "its home folder (default ~/.hermes or $HERMES_HOME; ~/.openclaw or $OPENCLAW_STATE_DIR)")
		fs.StringVar(&agent, "agent", "", `the Habakkuk agent the main agent became at import (default "habakkuk")`)
		fs.BoolVar(&dryRun, "dry-run", false, "show what would be stopped, paused and resumed; change nothing")
		fs.BoolVar(&force, "force", false, "resume the imported schedules even if the old scheduler still runs")
	}
	fs.BoolVar(&yes, "y", false, "do not ask")
	fs.BoolVar(&yes, "yes", false, "same as -y")
	fs.Usage = func() {
		if verb == "finish" {
			fmt.Fprint(out, finishUsage)
		} else {
			fmt.Fprint(out, rollbackUsage)
		}
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q; see hk migrate %s -h", fs.Arg(0), verb)
	}
	sys := newCutoverSystem()
	if userHome == "" {
		userHome = sys.UserHome()
	}
	if from == "" {
		var err error
		if from, err = pickRunning(home, verb, source, getenv, userHome); err != nil {
			return err
		}
	}
	if !cutover.Runs(from) {
		if slices.Contains(migrate.Sources, from) {
			return fmt.Errorf("%s doesn't run in the background, so there is nothing to stop", migrate.Label(from))
		}
		return fmt.Errorf("--from %q: want hermes or openclaw", from)
	}
	m := &migration{home: home, in: bufio.NewReader(in), out: out, getenv: getenv}
	if verb == "rollback" {
		return m.rollback(ctx, sys, from, yes)
	}
	return m.finish(ctx, sys, cutoverArgs{from: from, source: source, agent: agent, userHome: userHome, dryRun: dryRun, force: force, yes: yes})
}

// pickRunning finds the old agent when --from is not given: the one with
// a record (for rollback), else the one whose home is here.
func pickRunning(home, verb, source string, getenv func(string) string, userHome string) (string, error) {
	var found []string
	for _, from := range migrate.Sources {
		if !cutover.Runs(from) {
			continue
		}
		switch {
		case verb == "rollback":
			if r, _ := cutover.LoadRecord(home, from); r != nil {
				found = append(found, from)
			}
		case source != "":
			if migrate.Present(from, source) {
				found = append(found, from)
			}
		default:
			if migrate.Present(from, migrate.DefaultSource(from, migrate.Options{Getenv: getenv, UserHome: userHome})) {
				found = append(found, from)
			}
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		if verb == "rollback" {
			return "", errors.New("there is no finished move to roll back")
		}
		return "", errors.New("found no Hermes Agent or OpenClaw home; pass --from and --source")
	}
	return "", fmt.Errorf("found both %s; pass --from hermes or --from openclaw", andList(labels(found)))
}

type cutoverArgs struct {
	from, source, agent, userHome string
	dryRun, force, yes            bool
}

func (m *migration) finish(ctx context.Context, sys cutover.System, a cutoverArgs) error {
	if a.source == "" {
		a.source = migrate.DefaultSource(a.from, migrate.Options{Getenv: m.getenv, UserHome: a.userHome})
	}
	if !migrate.Present(a.from, a.source) {
		return fmt.Errorf("%s does not look like the home of %s; pass --source", a.source, migrate.Label(a.from))
	}
	o := cutover.Options{From: a.from, Source: a.source, Getenv: m.getenv}
	st, err := cutover.Detect(ctx, sys, o)
	if err != nil {
		return err
	}
	found := readForCutover(a.from, st.Dir, a.agent, m.getenv, a.userHome)
	m.say("%s.", st.Summary())
	m.writeStatus(st)
	m.writeShared(found)
	toStop := 0
	for _, sv := range st.Services {
		if sv.Active || sv.Enabled {
			toStop++
		}
	}
	toStop += len(st.Processes)
	rec, err := cutover.LoadRecord(m.home, a.from)
	if err != nil {
		return err
	}
	if toStop == 0 && len(st.Jobs) == 0 && rec != nil && !rec.Finished.IsZero() {
		m.say("\nThe move is already finished (%s). hk migrate rollback undoes it.", cutover.RecordPath(m.home, a.from))
		return nil
	}
	m.say("\nhk migrate finish will:")
	if toStop > 0 {
		m.say("  - stop %s and keep it from starting again", st.Label)
	}
	if n := len(st.Jobs); n > 0 {
		m.say("  - pause %s in %s (paused, not deleted)", count(n, "of its schedules", "of its schedules"), st.Label)
	}
	m.say("  - resume the schedules hk migrate imported paused")
	m.say("Everything it changes is recorded in %s, and hk migrate rollback puts it back.", cutover.RecordPath(m.home, a.from))
	if a.dryRun {
		m.say("\nDry run: nothing was changed.")
		return nil
	}
	if !a.yes {
		ok, err := m.ask("\nStop "+st.Label+" now?", false)
		if err != nil {
			return err
		}
		if !ok {
			m.say("Nothing was changed.")
			return nil
		}
	}
	m.say("")
	res, err := cutover.Finish(ctx, sys, cutover.FinishOptions{Options: o, Home: m.home, Found: found, Force: a.force,
		Say: func(format string, args ...any) { m.say(format, args...) }})
	if res != nil {
		for _, l := range res.Left {
			m.say("Not done: %s.", l)
		}
	}
	if errors.Is(err, cutover.ErrSchedulerRunning) {
		m.say("\n%s's scheduler is still running:", st.Label)
		m.writeStatus(res.Status)
		m.say("Stop it, then run hk migrate finish again. To resume the imported schedules anyway (they may run twice), run hk migrate finish --force.")
		return errors.New("the imported schedules were left paused")
	}
	if err != nil {
		return err
	}
	if len(res.Resumed) == 0 {
		m.say("No imported schedule was waiting to be resumed; resume any you want with hk schedules resume ID.")
	}
	if len(res.Left) > 0 {
		if res.Status.SchedulerRunning() {
			m.say("\n%s still runs, so a schedule that is on in both runs twice until you stop it.", st.Label)
		}
		m.say("Run hk migrate finish again once the steps above are done. hk migrate rollback undoes what was done.")
		return fmt.Errorf("%d step(s) were not done; each one above says what to do", len(res.Left))
	}
	m.say("\nOnly Habakkuk runs now. If something is wrong, hk migrate rollback puts %s back.", st.Label)
	return nil
}

func (m *migration) rollback(ctx context.Context, sys cutover.System, from string, yes bool) error {
	rec, err := cutover.LoadRecord(m.home, from)
	if err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("there is no finished move from %s to roll back", migrate.Label(from))
	}
	if !yes {
		ok, err := m.ask(fmt.Sprintf("Pause the imported schedules again and start %s again (%d recorded changes)?", migrate.Label(from), len(rec.Changes)), false)
		if err != nil {
			return err
		}
		if !ok {
			m.say("Nothing was changed.")
			return nil
		}
	}
	res, err := cutover.Rollback(ctx, sys, m.home, from, nil)
	if res != nil {
		if n := len(res.Paused); n > 0 {
			m.say("Paused %s in Habakkuk again.", count(n, "imported schedule", "imported schedules"))
		}
		if res.Resumed > 0 {
			m.say("Turned %s back on in %s.", count(res.Resumed, "of its schedules", "of its schedules"), migrate.Label(from))
		}
		if res.Brake != "" {
			m.say("Removed %s's pause switch (%s).", migrate.Label(from), res.Brake)
		}
		for _, s := range res.Services {
			m.say("Enabled and started %s again.", s)
		}
		for _, l := range res.Left {
			m.say("Not done: %s.", l)
		}
		if res.Retired != "" {
			m.say("\n%s runs again. The record is kept as %s.", migrate.Label(from), res.Retired)
		}
		if err == nil && len(res.Left) > 0 {
			return fmt.Errorf("%d step(s) were not done; fix them, then run hk migrate rollback again", len(res.Left))
		}
	}
	return err
}

// readForCutover reads the source the way hk migrate does, without its
// conversations, for the names its schedules and channels were imported
// under. nil when it cannot be read: matching then goes by name alone.
func readForCutover(from, dir, agent string, getenv func(string) string, userHome string) *migrate.Found {
	f, err := migrate.Read(migrate.Options{From: from, Source: dir, Agent: agent, Getenv: getenv,
		UserHome: userHome, History: migrate.HistorySkip})
	if err != nil {
		return nil
	}
	_ = f.Close()
	return f
}

// writeStatus lists what of the old agent runs.
func (m *migration) writeStatus(st *cutover.Status) {
	for _, sv := range st.Services {
		switch {
		case sv.Active:
			m.say("  service %s: running", sv.Service)
		case sv.Enabled:
			m.say("  service %s: stopped, starts at login or boot", sv.Service)
		}
	}
	for _, p := range st.Processes {
		m.say("  %s, pid %d: %s", p.Role, p.PID, shorten(p.Cmdline, 90))
	}
	if n := len(st.Jobs); n > 0 {
		m.say("  %s on in %s", count(n, "schedule", "schedules"), st.Label)
	}
	for _, p := range st.Problems {
		m.say("  could not check: %s", p)
	}
}

// writeShared names the channels both agents would use.
func (m *migration) writeShared(found *migrate.Found) {
	cfg, err := config.Load(m.home)
	if err != nil {
		return
	}
	shared := cutover.SharedChannels(found, cfg, habakkukSecret(m.home, m.getenv))
	if len(shared) == 0 {
		return
	}
	m.say("Channels both would use:")
	for _, s := range shared {
		m.say("  %s", s)
	}
}

// cutoverSection is the part of hk migrate's plan about the old agent
// still running, and the channels both would use.
func (m *migration) cutoverSection(ctx context.Context, found *migrate.Found) {
	if !cutover.Runs(found.From) {
		return
	}
	st, err := cutover.Detect(ctx, newCutoverSystem(), cutover.Options{From: found.From, Source: found.Dir, Getenv: m.getenv})
	if err != nil {
		return
	}
	cfg, _ := config.Load(m.home)
	shared := cutover.SharedChannels(found, cfg, habakkukSecret(m.home, m.getenv))
	if !st.Live() && len(st.Jobs) == 0 && len(shared) == 0 {
		return
	}
	m.say("\nThe old agent")
	m.say("  %s.", st.Summary())
	if st.Live() {
		m.writeStatus(st)
	}
	for _, s := range shared {
		m.say("  Both would use %s", s)
	}
	m.say("  After the import, hk migrate finish stops it, pauses its schedules and resumes the imported ones.")
}

// habakkukSecret reads a variable the way Habakkuk does: the environment,
// then $HABAKKUK_HOME/.env.
func habakkukSecret(home string, getenv func(string) string) func(string) string {
	env, _ := migrate.ReadDotEnv(filepath.Join(home, ".env"))
	return func(k string) string {
		if getenv != nil {
			if v := getenv(k); v != "" {
				return v
			}
		} else if v := os.Getenv(k); v != "" {
			return v
		}
		return env[k]
	}
}

func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// cutoverDoctor warns when an old agent still runs next to Habakkuk: its
// scheduler fires schedules Habakkuk runs too, or it polls a channel
// Habakkuk uses. It returns how many problems it printed.
func cutoverDoctor(ctx context.Context, home string, w io.Writer) int {
	sys := newCutoverSystem()
	cfg, _ := config.Load(home)
	_, serving := serveRunning(home)
	problems := 0
	for _, from := range migrate.Sources {
		if !cutover.Runs(from) {
			continue
		}
		dir := ""
		if rec, _ := cutover.LoadRecord(home, from); rec != nil {
			dir = rec.Source
		}
		if dir == "" {
			dir = migrate.DefaultSource(from, migrate.Options{UserHome: sys.UserHome()})
			if !migrate.Present(from, dir) {
				continue
			}
		}
		st, err := cutover.Detect(ctx, sys, cutover.Options{From: from, Source: dir})
		if err != nil || len(st.Processes) == 0 {
			continue
		}
		fix := "hk migrate finish --from " + from
		// Names alone: the agent names the import used are not known here.
		twice, _ := cutover.Overlap(ctx, home, nil, st.Jobs)
		if st.SchedulerRunning() && len(twice) > 0 {
			when := "they run twice once hk serve starts"
			if serving {
				when = "each one runs twice"
			}
			fmt.Fprintf(w, "migrate: %s; %s is on in both, so %s (%s); fix: %s\n", st.Summary(),
				count(len(twice), "imported schedule", "imported schedules"), when, firstFew(twice, 3), fix)
			problems++
		}
		found := readForCutover(from, dir, "", nil, sys.UserHome())
		if shared := cutover.SharedChannels(found, cfg, habakkukSecret(home, nil)); len(shared) > 0 {
			var names []string
			for _, s := range shared {
				names = append(names, s.Channel)
			}
			what := "channel"
			if len(shared) > 1 {
				what = fmt.Sprintf("%d channels", len(shared))
			}
			fmt.Fprintf(w, "migrate: %s is running and uses the same %s as Habakkuk (%s); fix: %s\n", st.Label,
				what, strings.Join(names, ", "), fix)
			problems++
		}
	}
	return problems
}
