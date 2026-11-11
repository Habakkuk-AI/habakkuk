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
// schedules while the old agent's scheduler still runs.
var ErrSchedulerRunning = errors.New("the old agent's scheduler is still running")

// FinishOptions say how to finish a move.
type FinishOptions struct {
	Options
	// Home is the Habakkuk home.
	Home string
	// Found is the source as hk migrate reads it, for the names its
	// schedules were imported under; nil matches on names alone.
	Found *migrate.Found
	// Force resumes the imported schedules even when the old scheduler
	// still runs after the stop.
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

// Finish stops the old agent and its services, pauses its schedules, and
// resumes the imported ones. It refuses the last step while the old
// scheduler still runs, unless Force. Each change goes into the record
// before the next one starts.
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

	// 0. The old agent's own brake, when it has one: from now on it
	// starts no new run, even if a process of it is missed or comes back.
	if b := sp.brake; b != nil {
		path := b.file(st.Dir)
		if !fileExists(path) {
			if err := os.WriteFile(path, []byte(b.text(o.now())), 0o600); err != nil {
				res.Left = append(res.Left, fmt.Sprintf("setting %s's own pause switch (%s): %v", st.Label, path, err))
			} else {
				if err := rec.add(Change{Kind: ChangeBrake, At: o.now(), Store: path}); err != nil {
					return res, err
				}
				o.say("Set %s's own pause switch (%s): it starts no new work.", st.Label, path)
			}
		}
	}

	// 1. Services first: a service manager restarts a process stopped
	// under it.
	for _, sv := range st.Services {
		if !sv.Active && !sv.Enabled {
			continue
		}
		if sv.NeedsRoot() {
			res.Left = append(res.Left, fmt.Sprintf("%s: only an administrator can stop it; run %s", sv.Service, sv.StopCommand(sys.UID())))
			continue
		}
		if err := stopService(ctx, sys, sv.Service); err != nil {
			res.Left = append(res.Left, fmt.Sprintf("%s: could not stop it (%v); run %s", sv.Service, err, sv.StopCommand(sys.UID())))
			continue
		}
		was := sv.ServiceState
		svc := sv.Service
		if err := rec.add(Change{Kind: ChangeService, At: o.now(), Service: &svc, Was: &was}); err != nil {
			return res, err
		}
		o.say("Stopped and disabled %s.", sv.Service)
	}

	// 2. What still runs: workers, bridges, a gateway started by hand.
	if st, err = Detect(ctx, sys, o.Options); err != nil {
		return res, err
	}
	procs := slices.Clone(st.Processes)
	slices.SortStableFunc(procs, func(a, b Running) int { return roleOrder(a.Role) - roleOrder(b.Role) })
	for _, p := range procs {
		if err := stopProcess(sys, p.PID, o.StopWait); err != nil {
			res.Left = append(res.Left, fmt.Sprintf("%s (pid %d): could not stop it: %v", p.Role, p.PID, err))
			continue
		}
		if err := rec.add(Change{Kind: ChangeProcess, At: o.now(), PID: p.PID, Cmdline: p.Cmdline, Role: p.Role}); err != nil {
			return res, err
		}
		o.say("Stopped the %s (pid %d).", p.Role, p.PID)
	}
	if st, err = Detect(ctx, sys, o.Options); err != nil {
		return res, err
	}
	res.Status = st
	schedulerOn := st.SchedulerRunning()

	// 3. Pause the old agent's own schedules, in its own store, once
	// nothing of it runs: a scheduler that runs may write its store back
	// over the change.
	names := importedNames(o.Found)
	jobs := st.Jobs
	if schedulerOn {
		jobs = nil
		res.Left = append(res.Left, "its schedules stay on until its scheduler is stopped")
	}
	byStore := map[string][]Job{}
	var stores []string
	for _, j := range jobs {
		if _, ok := byStore[j.Store]; !ok {
			stores = append(stores, j.Store)
		}
		byStore[j.Store] = append(byStore[j.Store], j)
	}
	paused := 0
	for _, store := range stores {
		batch := byStore[store]
		var ids []string
		for _, j := range batch {
			ids = append(ids, j.ID)
		}
		done, err := sp.pause(store, ids)
		for _, j := range batch {
			if !slices.Contains(done, j.ID) {
				continue
			}
			if err := rec.add(Change{Kind: ChangeOldJob, At: o.now(), JobID: j.ID, Store: store, Name: names.of(j)}); err != nil {
				return res, err
			}
			paused++
		}
		if err != nil {
			res.Left = append(res.Left, fmt.Sprintf("its schedules in %s: could not pause them all (%v); turn them off in %s", store, err, st.Label))
		}
	}
	if paused > 0 {
		o.say("Paused %s in %s (paused, not deleted).", count(paused, "of its schedules", "of its schedules"), st.Label)
	}

	// 4. Resume the imported schedules, unless the old scheduler runs.
	if schedulerOn && !o.Force {
		res.Left = append(res.Left, "the imported schedules stay paused while "+st.Label+"'s scheduler runs")
		rec.Finished = time.Time{}
		return res, errors.Join(ErrSchedulerRunning, rec.save())
	}
	// Exactly the ones the old agent ran: a schedule that was off there
	// stays paused here too.
	want := map[string]bool{}
	for _, c := range rec.Changes {
		if c.Kind == ChangeOldJob && c.Name != "" {
			want[c.Name] = true
		}
	}
	if schedulerOn {
		// --force: the ones it still runs, which were not paused there.
		for _, j := range st.Jobs {
			want[names.of(j)] = true
		}
	}
	resumed, err := resumeImported(ctx, o.Home, want, func(id, name string) error {
		return rec.add(Change{Kind: ChangeSchedule, At: o.now(), ScheduleID: id, Name: name})
	})
	res.Resumed = resumed
	if err != nil {
		res.Left = append(res.Left, "resuming the imported schedules: "+err.Error())
	}
	if len(resumed) > 0 {
		o.say("Resumed %s in Habakkuk.", count(len(resumed), "imported schedule", "imported schedules"))
	}
	rec.Finished = o.now()
	if err := rec.save(); err != nil {
		return res, err
	}
	return res, nil
}

// roleOrder stops the gateway first: it may start workers again.
func roleOrder(role string) int {
	return slices.Index([]string{RoleGateway, RoleScheduler, RoleWorker, RoleBridge}, role)
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

// names maps the old agent's schedules to the agent/name they were
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
func resumeImported(ctx context.Context, home string, want map[string]bool, done func(id, name string) error) ([]string, error) {
	if len(want) == 0 {
		return nil, nil
	}
	db, err := app.OpenControl(ctx, home)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	st := schedule.NewStore(db, nil)
	list, err := st.List(ctx, "")
	if err != nil {
		return nil, err
	}
	var resumed []string
	var errs []error
	for _, sc := range list {
		if sc.Enabled || !matches(want, sc) {
			continue
		}
		if err := st.SetEnabled(ctx, sc.ID, true); err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", sc.Agent, sc.Name, err))
			continue
		}
		name := sc.Agent + "/" + sc.Name
		resumed = append(resumed, name)
		if err := done(sc.ID, name); err != nil {
			return resumed, err
		}
	}
	return resumed, errors.Join(errs...)
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
	Resumed  int      // old schedules turned back on
	Services []string // services enabled and started again
	Brake    string   // the old agent's pause switch, removed
	// Left are the things it could not do, each with what to do instead.
	Left []string
	// Retired is where the record went.
	Retired string
}

// Rollback undoes what Finish recorded, newest first: the imported
// schedules are paused before the old agent starts again.
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
		switch c.Kind {
		case ChangeSchedule:
			if hk == nil {
				db, err := app.OpenControl(ctx, home)
				if err != nil {
					return res, fmt.Errorf("pausing the imported schedules: %w; nothing was rolled back", err)
				}
				defer db.Close()
				hk = schedule.NewStore(db, nil)
			}
			if err := hk.SetEnabled(ctx, c.ScheduleID, false); err != nil {
				res.Left = append(res.Left, fmt.Sprintf("schedule %s (%s): %v; pause it with hk schedules pause %s", c.Name, c.ScheduleID, err, c.ScheduleID))
				continue
			}
			res.Paused = append(res.Paused, c.Name)
		case ChangeOldJob:
			if err := sp.resume(c.Store, c.JobID); err != nil {
				res.Left = append(res.Left, fmt.Sprintf("its schedule %s in %s: %v; turn it on again in %s", c.JobID, c.Store, err, migrate.Label(from)))
				continue
			}
			res.Resumed++
		case ChangeService:
			if c.Service == nil || c.Was == nil {
				continue
			}
			if err := restoreService(ctx, sys, *c.Service, *c.Was); err != nil {
				res.Left = append(res.Left, fmt.Sprintf("%s: %v", c.Service, err))
				continue
			}
			res.Services = append(res.Services, c.Service.String())
		case ChangeBrake:
			text, err := os.ReadFile(c.Store)
			switch {
			case errors.Is(err, os.ErrNotExist):
			case err != nil:
				res.Left = append(res.Left, fmt.Sprintf("%s: %v", c.Store, err))
				continue
			case !strings.Contains(string(text), pauseReason):
				// Set again since by someone else: theirs to remove.
				continue
			default:
				if err := os.Remove(c.Store); err != nil {
					res.Left = append(res.Left, fmt.Sprintf("%s: %v; remove it to let %s work again", c.Store, err, migrate.Label(from)))
					continue
				}
			}
			res.Brake = c.Store
		case ChangeProcess:
			if !servedBy(rec, c) {
				res.Left = append(res.Left, fmt.Sprintf("the %s that ran outside a service (pid %d) is not started again; start it the way you did before: %s",
					c.Role, c.PID, strings.TrimSpace(c.Cmdline)))
			}
		}
	}
	if len(res.Left) > 0 {
		// Keep the record: running rollback again tries what is left,
		// and what is already undone does nothing the second time.
		return res, rec.save()
	}
	if res.Retired, err = rec.retire(now()); err != nil {
		return res, err
	}
	return res, nil
}

// servedBy reports whether a stopped process belonged to a service the
// rollback starts again, so it comes back with it.
func servedBy(rec *Record, c Change) bool {
	if c.Role != RoleGateway {
		return false
	}
	for _, o := range rec.Changes {
		if o.Kind == ChangeService && o.Was != nil && o.Was.Active {
			return true
		}
	}
	return false
}
