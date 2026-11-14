package cutover

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/app"
	"github.com/habakkuk-ai/habakkuk/internal/migrate"
	"github.com/habakkuk-ai/habakkuk/internal/work/schedule"
)

// ErrSchedulerRunning is returned when finish would resume the imported
// schedules while the old agent's scheduler still runs, or may.
var ErrSchedulerRunning = errors.New("the old agent's scheduler is still running")

// ErrUnchecked is returned when finish could not see what runs (the
// process list could not be read, say), so it cannot tell whether the old
// scheduler still runs.
var ErrUnchecked = errors.New("could not check that the old agent has stopped")

// ErrRecord is returned when the record could not be written. Nothing
// is changed without its entry in the record.
var ErrRecord = errors.New("could not write the record")

// FinishOptions say how to finish a move.
type FinishOptions struct {
	Options
	// Home is the Habakkuk home.
	Home string
	// Found is the source as hk migrate reads it, for the names its
	// schedules were imported under; nil matches on names alone.
	Found *migrate.Found
	// Force resumes the imported schedules even when the old scheduler
	// still runs (or may) after the stop, or what runs could not be seen.
	Force bool
	// Say reports progress, one line at a time; nil is quiet.
	Say func(format string, args ...any)
	// StopWait is how long a process gets to end before it is forced
	// (default 10 seconds).
	StopWait time.Duration
}

func (o *FinishOptions) say(format string, args ...any) {
	if o.Say != nil {
		o.Say(format, args...)
	}
}

// FinishResult says what finish did.
type FinishResult struct {
	Record *Record
	// Left are the things it could not do, each with what to do instead.
	Left []string
	// Resumed are the imported schedules now on, as agent/name.
	Resumed []string
	// Status is the old agent as it is after the stop.
	Status *Status
}

// finisher carries one run of Finish.
type finisher struct {
	ctx context.Context
	sys System
	o   FinishOptions
	rec *Record
	res *FinishResult
}

func (f *finisher) recordErr(err error) error {
	return fmt.Errorf("%w (%s): %v; nothing more was changed", ErrRecord, f.rec.path, err)
}

// step records a change, makes it, and marks it done. A change whose
// entry cannot be written is not made, and the run stops (ErrRecord).
// When the change itself fails, ok is false and the failure goes to Left.
func (f *finisher) step(c Change, what string, do func() error) (ok bool, err error) {
	c.At = f.o.now()
	i, err := f.rec.intend(c)
	if err != nil {
		return false, f.recordErr(err)
	}
	if derr := do(); derr != nil {
		f.res.Left = append(f.res.Left, what+": "+derr.Error())
		if err := f.rec.mark(i, Failed); err != nil {
			return false, f.recordErr(err)
		}
		return false, nil
	}
	if err := f.rec.mark(i, Done); err != nil {
		return true, f.recordErr(err)
	}
	return true, nil
}

// Finish stops the old agent and its services, pauses its schedules, and
// resumes the imported ones. It refuses the last step while the old
// scheduler still runs (or may), or when what runs could not be seen,
// unless Force. Each change is in the record before it is made.
func Finish(ctx context.Context, sys System, o FinishOptions) (*FinishResult, error) {
	sp := specs[o.From]
	if sp == nil {
		return nil, fmt.Errorf("%s does not run in the background; there is nothing to stop", migrate.Label(o.From))
	}
	st, err := Detect(ctx, sys, o.Options)
	if err != nil {
		return nil, err
	}
	rec, err := LoadRecord(o.Home, o.From)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		rec = newRecord(o.Home, o.From, st.Dir, o.now())
	}
	res := &FinishResult{Record: rec, Status: st}
	f := &finisher{ctx: ctx, sys: sys, o: o, rec: rec, res: res}

	// 0. The old agent's own brake, in every home it keeps, when it has
	// one: from now on it starts no new run, even if a process of it is
	// missed or comes back.
	if sp.brakes != nil {
		for _, path := range sp.brakes(st.Dir) {
			if fileExists(path) {
				continue
			}
			ok, err := f.step(Change{Kind: ChangeBrake, Store: path}, "setting "+st.Label+"'s pause switch "+path, func() error {
				return os.WriteFile(path, []byte(sp.brakeText(o.now())), 0o600)
			})
			if err != nil {
				return res, err
			}
			if ok {
				o.say("Set %s's own pause switch (%s): it starts no new work.", st.Label, path)
			}
		}
	}

	// 1. Services: a service manager restarts a process stopped under it.
	for _, sv := range st.Services {
		if !sv.Active && !sv.Enabled {
			continue
		}
		if sv.NeedsRoot() {
			res.Left = append(res.Left, fmt.Sprintf("%s: only an administrator can stop it; run %s", sv.Service, sv.StopCommand(sys.UID())))
			continue
		}
		svc, was := sv.Service, sv.ServiceState
		ok, err := f.step(Change{Kind: ChangeService, Service: &svc, Was: &was},
			fmt.Sprintf("%s: could not stop it; run %s", sv.Service, sv.StopCommand(sys.UID())),
			func() error { return stopService(ctx, sys, svc) })
		if err != nil {
			return res, err
		}
		if ok {
			o.say("Stopped and disabled %s.", sv.Service)
		}
	}

	// 2. The scopes its workers run in, then what still runs: workers,
	// bridges, a gateway started by hand. Only what is tied to this home
	// is stopped.
	if st, err = Detect(ctx, sys, o.Options); err != nil {
		return res, err
	}
	for _, scope := range st.Scopes {
		ok, err := f.step(Change{Kind: ChangeScope, Name: scope, Role: RoleWorker}, "scope "+scope, func() error {
			_, err := sys.Run(ctx, "systemctl", "--user", "stop", scope)
			return err
		})
		if err != nil {
			return res, err
		}
		if ok {
			o.say("Stopped the worker scope %s.", scope)
		}
	}
	if len(st.Scopes) > 0 {
		if st, err = Detect(ctx, sys, o.Options); err != nil {
			return res, err
		}
	}
	procs := slices.Clone(st.Processes)
	slices.SortStableFunc(procs, func(a, b Running) int { return roleOrder(a.Role) - roleOrder(b.Role) })
	for _, p := range procs {
		ok, err := f.step(Change{Kind: ChangeProcess, PID: p.PID, Cmdline: p.Cmdline, Role: p.Role},
			fmt.Sprintf("%s (pid %d): could not stop it", p.Role, p.PID),
			func() error { return stopProcess(sys, p.PID, o.StopWait) })
		if err != nil {
			return res, err
		}
		if ok {
			o.say("Stopped the %s (pid %d).", p.Role, p.PID)
		}
	}
	if st, err = Detect(ctx, sys, o.Options); err != nil {
		return res, err
	}
	res.Status = st
	for _, p := range st.Maybe {
		res.Left = append(res.Left, fmt.Sprintf("%s (pid %d) may be %s's, but %s; stop it yourself if it is: %s",
			p.Role, p.PID, st.Label, p.Why, shortCmd(p.Cmdline)))
	}
	schedulerOn := st.SchedulerRunning() || len(st.MaybeScheduler()) > 0
	unchecked := len(st.Problems) > 0
	for _, p := range st.Problems {
		res.Left = append(res.Left, "could not check: "+p)
	}

	// 3. Pause the old agent's own schedules, in its own store, once it is
	// known to be stopped: a scheduler that runs may write its store back
	// over the change.
	names := importedNames(o.Found)
	jobs := st.Jobs
	if schedulerOn || unchecked {
		jobs = nil
		if len(st.Jobs) > 0 {
			res.Left = append(res.Left, "its schedules stay on until it is known to be stopped")
		}
	}
	paused := 0
	for _, store := range storesOf(jobs) {
		n, err := f.pauseStore(sp, store, jobs, names)
		paused += n
		if err != nil {
			return res, err
		}
	}
	if paused > 0 {
		o.say("Paused %s in %s (paused, not deleted).", count(paused, "of its schedules", "of its schedules"), st.Label)
	}

	// 4. Resume the imported schedules, unless the old scheduler runs or
	// may, or what runs could not be seen.
	if !o.Force {
		switch {
		case unchecked:
			res.Left = append(res.Left, "the imported schedules stay paused until you check by hand that "+st.Label+" has stopped")
			return res, ErrUnchecked
		case schedulerOn:
			res.Left = append(res.Left, "the imported schedules stay paused while "+st.Label+"'s scheduler runs")
			return res, ErrSchedulerRunning
		}
	}
	// Exactly the ones the old agent ran: a schedule that was off there
	// stays paused here too.
	want := map[string]bool{}
	for _, c := range rec.Changes {
		if c.Kind == ChangeOldJob && c.Name != "" && (c.State == Done || c.State == Pending) {
			want[c.Name] = true
		}
	}
	if schedulerOn || unchecked {
		// --force: the ones it still runs, which were not paused there.
		for _, j := range st.Jobs {
			want[names.of(j)] = true
		}
	}
	if err := f.resumeImported(want); err != nil {
		return res, err
	}
	if len(res.Resumed) > 0 {
		o.say("Resumed %s in Habakkuk.", count(len(res.Resumed), "imported schedule", "imported schedules"))
	}
	rec.Finished = o.now()
	if err := rec.save(); err != nil {
		return res, fmt.Errorf("%w (%s): %v", ErrRecord, rec.path, err)
	}
	return res, nil
}

func storesOf(jobs []Job) []string {
	var stores []string
	for _, j := range jobs {
		if !slices.Contains(stores, j.Store) {
			stores = append(stores, j.Store)
		}
	}
	return stores
}

// pauseStore pauses the jobs of one store: each job's fields as they are
// go into the record first, then one write pauses them all.
func (f *finisher) pauseStore(sp *spec, path string, jobs []Job, names nameMap) (int, error) {
	js := sp.store(path)
	var batch []Job
	var ids []string
	for _, j := range jobs {
		if j.Store == path {
			batch = append(batch, j)
			ids = append(ids, j.ID)
		}
	}
	prior, err := js.read(ids)
	if err != nil {
		f.res.Left = append(f.res.Left, fmt.Sprintf("its schedules in %s: %v; turn them off in %s", path, err, f.res.Status.Label))
		return 0, nil
	}
	set := js.paused(f.o.now())
	changes := map[string][2]fields{}
	entries := map[string]int{}
	for _, j := range batch {
		p, ok := prior[j.ID]
		if !ok {
			continue
		}
		i, err := f.rec.intend(Change{Kind: ChangeOldJob, At: f.o.now(), JobID: j.ID, Store: path, Name: names.of(j), Prior: p, Set: set})
		if err != nil {
			return 0, f.recordErr(err)
		}
		entries[j.ID] = i
		changes[j.ID] = [2]fields{p, set}
	}
	done, werr := js.write(changes)
	n := 0
	for id, i := range entries {
		state := Failed
		if slices.Contains(done, id) {
			state = Done
			n++
		}
		f.rec.Changes[i].State = state
	}
	if werr != nil {
		f.res.Left = append(f.res.Left, fmt.Sprintf("its schedules in %s: %v; turn them off in %s", path, werr, f.res.Status.Label))
	}
	if err := f.rec.save(); err != nil {
		return n, f.recordErr(err)
	}
	return n, nil
}

// roleOrder stops the gateway first: it may start workers again.
func roleOrder(role string) int {
	return slices.Index([]string{RoleGateway, RoleScheduler, RoleDashboard, RoleWorker, RoleLeftover, RoleBridge}, role)
}

// stopProcess asks a process to end, waits, then forces it.
func stopProcess(sys System, pid int, wait time.Duration) error {
	if pid == sys.Self() {
		return errors.New("that is this process")
	}
	if wait <= 0 {
		wait = 10 * time.Second
	}
	if err := sys.Stop(pid, false); err != nil && sys.Alive(pid) {
		return err
	}
	const step = 250 * time.Millisecond
	for waited := time.Duration(0); waited < wait; waited += step {
		if !sys.Alive(pid) {
			return nil
		}
		sys.Wait(step)
	}
	if err := sys.Stop(pid, true); err != nil && sys.Alive(pid) {
		return err
	}
	for range 8 {
		if !sys.Alive(pid) {
			return nil
		}
		sys.Wait(step)
	}
	return errors.New("it is still running")
}

// nameMap maps the old agent's schedules to the agent/name they were
// imported as.
type nameMap struct {
	agents map[string]string // source agent or profile -> Habakkuk agent
	main   string
}

func importedNames(f *migrate.Found) nameMap {
	m := nameMap{agents: map[string]string{}}
	if f == nil {
		return m
	}
	for _, a := range f.Agents {
		m.agents[a.From] = a.Name
		if a.Main {
			m.main = a.Name
		}
	}
	return m
}

// of is the agent/name a job was imported as; "*/name" when the agent is
// not known.
func (m nameMap) of(j Job) string {
	agent := m.agents[j.Agent]
	if agent == "" && (j.Agent == "" || j.Agent == "default") {
		agent = m.main
	}
	if agent == "" {
		agent = "*"
	}
	return agent + "/" + j.Name
}

// matches reports whether a Habakkuk schedule is one of want.
func matches(want map[string]bool, sc schedule.Schedule) bool {
	return want[sc.Agent+"/"+sc.Name] || want["*/"+sc.Name]
}

// resumeImported turns on the paused Habakkuk schedules named in want.
func (f *finisher) resumeImported(want map[string]bool) error {
	if len(want) == 0 {
		return nil
	}
	db, err := app.OpenControl(f.ctx, f.o.Home)
	if err != nil {
		f.res.Left = append(f.res.Left, "resuming the imported schedules: "+err.Error())
		return nil
	}
	defer db.Close()
	st := schedule.NewStore(db, nil)
	list, err := st.List(f.ctx, "")
	if err != nil {
		f.res.Left = append(f.res.Left, "resuming the imported schedules: "+err.Error())
		return nil
	}
	for _, sc := range list {
		if sc.Enabled || !matches(want, sc) {
			continue
		}
		name := sc.Agent + "/" + sc.Name
		ok, err := f.step(Change{Kind: ChangeSchedule, ScheduleID: sc.ID, Name: name}, "resuming "+name,
			func() error { return st.SetEnabled(f.ctx, sc.ID, true) })
		if err != nil {
			return err
		}
		if ok {
			f.res.Resumed = append(f.res.Resumed, name)
		}
	}
	return nil
}

// Overlap lists the Habakkuk schedules that are on and match one of the
// old agent's schedules that is on too: each would run twice.
func Overlap(ctx context.Context, home string, found *migrate.Found, jobs []Job) ([]string, error) {
	if len(jobs) == 0 {
		return nil, nil
	}
	names := importedNames(found)
	want := map[string]bool{}
	for _, j := range jobs {
		want[names.of(j)] = true
	}
	db, err := app.OpenControl(ctx, home)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	list, err := schedule.NewStore(db, nil).List(ctx, "")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, sc := range list {
		if sc.Enabled && matches(want, sc) {
			out = append(out, sc.Agent+"/"+sc.Name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// RollbackResult says what rollback did.
type RollbackResult struct {
	Paused   []string // imported schedules paused again
	Resumed  int      // old schedules put back as they were
	Services []string // services put back as they were
	Brakes   []string // the old agent's pause switches, removed
	// Kept are the old schedules changed since finish paused them, left
	// as they are now.
	Kept []string
	// Gone are the processes finish stopped that ran outside a service:
	// they cannot be started again for you, so each names its command.
	Gone []string
	// Left are the things it could not do, each with what to do instead.
	Left []string
	// Retired is where the record went.
	Retired string
}

// Rollback undoes what Finish recorded, newest first, so the imported
// schedules are paused before the old agent starts again. Entries still
// pending (a finish that stopped midway) are undone too. Each entry is
// marked undone as soon as it is, so running rollback again never does a
// step twice; the record is retired once nothing is left.
func Rollback(ctx context.Context, sys System, home, from string, now func() time.Time) (*RollbackResult, error) {
	sp := specs[from]
	if sp == nil {
		return nil, fmt.Errorf("%s does not run in the background; there is nothing to roll back", migrate.Label(from))
	}
	if now == nil {
		now = time.Now
	}
	rec, err := LoadRecord(home, from)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("no finished move from %s to roll back (no %s)", migrate.Label(from), RecordPath(home, from))
	}
	res := &RollbackResult{}
	var hk *schedule.Store
	for i := len(rec.Changes) - 1; i >= 0; i-- {
		c := rec.Changes[i]
		if c.State == Undone {
			continue
		}
		var err error
		switch c.Kind {
		case ChangeSchedule:
			if hk == nil {
				db, oerr := app.OpenControl(ctx, home)
				if oerr != nil {
					return res, fmt.Errorf("pausing the imported schedules: %w; nothing more was rolled back", oerr)
				}
				defer db.Close()
				hk = schedule.NewStore(db, nil)
			}
			if serr := hk.SetEnabled(ctx, c.ScheduleID, false); serr == nil {
				res.Paused = append(res.Paused, c.Name)
			} else if !strings.Contains(serr.Error(), "no schedule") { // deleted since: nothing runs
				err = fmt.Errorf("schedule %s (%s): %v; pause it with hk schedules pause %s", c.Name, c.ScheduleID, serr, c.ScheduleID)
			}
		case ChangeOldJob:
			if c.State == Failed {
				break // never written
			}
			done, werr := sp.store(c.Store).write(map[string][2]fields{c.JobID: {c.Set, c.Prior}})
			switch {
			case werr != nil:
				err = fmt.Errorf("its schedule %s in %s: %v; turn it on again in %s", c.JobID, c.Store, werr, migrate.Label(from))
			case len(done) == 1:
				res.Resumed++
			case c.State == Done:
				res.Kept = append(res.Kept, c.Name)
			}
		case ChangeService:
			if c.Service == nil || c.Was == nil {
				break
			}
			if serr := restoreService(ctx, sys, *c.Service, *c.Was); serr != nil {
				err = fmt.Errorf("%s: %v", c.Service, serr)
			} else {
				res.Services = append(res.Services, c.Service.String())
			}
		case ChangeBrake:
			text, rerr := os.ReadFile(c.Store)
			switch {
			case errors.Is(rerr, os.ErrNotExist):
			case rerr != nil:
				err = fmt.Errorf("%s: %v", c.Store, rerr)
			case !strings.Contains(string(text), pauseReason):
				// Set again since by someone else: theirs to remove.
			default:
				if rerr := os.Remove(c.Store); rerr != nil {
					err = fmt.Errorf("%s: %v; remove it to let %s work again", c.Store, rerr, migrate.Label(from))
				} else {
					res.Brakes = append(res.Brakes, c.Store)
				}
			}
		case ChangeProcess, ChangeScope:
			// Not restorable: it is reported, not kept open.
			if c.State != Failed && !servedBy(rec, c) {
				what := fmt.Sprintf("the %s (pid %d): %s", c.Role, c.PID, shortCmd(c.Cmdline))
				if c.Kind == ChangeScope {
					what = "the worker scope " + c.Name
				}
				res.Gone = append(res.Gone, what)
			}
		}
		if err != nil {
			res.Left = append(res.Left, err.Error())
			continue
		}
		if merr := rec.mark(i, Undone); merr != nil {
			return res, fmt.Errorf("%w (%s): %v; run hk migrate rollback again", ErrRecord, rec.path, merr)
		}
	}
	if len(res.Left) > 0 {
		// The record stays: running rollback again tries only what is left.
		return res, nil
	}
	if res.Retired, err = rec.retire(now()); err != nil {
		return res, err
	}
	return res, nil
}

// servedBy reports whether a stopped process belonged to a service the
// rollback starts again, so it comes back with it.
func servedBy(rec *Record, c Change) bool {
	if c.Kind != ChangeProcess || c.Role != RoleGateway {
		return false
	}
	for _, o := range rec.Changes {
		if o.Kind == ChangeService && o.Was != nil && o.Was.Active && o.Service != nil && o.Service.Role == RoleGateway {
			return true
		}
	}
	return false
}

func shortCmd(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		return s[:117] + "..."
	}
	return s
}
