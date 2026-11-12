package cutover

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
)

// Record is what hk migrate finish changed, kept in
// $HABAKKUK_HOME/migrate/cutover-<from>.json so hk migrate rollback can
// undo exactly that. Each change is written to it, and flushed to disk,
// before it is made (state pending), and marked done after. When the
// record cannot be written, the change is not made. A finish that stops
// at any point therefore leaves a record that covers everything it may
// have changed, and rollback undoes pending entries too.
type Record struct {
	Version  int       `json:"version"`
	From     string    `json:"from"`
	Source   string    `json:"source"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
	Changes  []Change  `json:"changes"`

	path string
}

// The kinds of change.
const (
	ChangeService  = "service"  // a service stopped and disabled
	ChangeProcess  = "process"  // a process stopped
	ChangeOldJob   = "old-job"  // one of the old agent's schedules paused
	ChangeSchedule = "schedule" // an imported Habakkuk schedule resumed
	ChangeBrake    = "brake"    // the old agent's own stop switch set (Store is its file)
	ChangeScope    = "scope"    // a systemd scope of its workers stopped
)

// Change states.
const (
	Pending = "pending" // about to be made, or made but not yet marked
	Done    = "done"
	Failed  = "failed" // tried, and the attempt reported an error
	Undone  = "undone" // rolled back (or, for a stopped process, reported)
)

// Change is one thing finish changed.
type Change struct {
	Kind  string    `json:"kind"`
	State string    `json:"state"`
	At    time.Time `json:"at"`
	// service
	Service *Service      `json:"service,omitempty"`
	Was     *ServiceState `json:"was,omitempty"`
	// process
	PID     int    `json:"pid,omitempty"`
	Cmdline string `json:"cmdline,omitempty"`
	Role    string `json:"role,omitempty"`
	// old-job: its id and name in the old agent, and the store it is in
	// (a jobs file or a database); Prior are the values of the fields the
	// pause changes as they were, Set what the pause wrote. A field
	// missing from Prior was not there.
	JobID string                     `json:"job_id,omitempty"`
	Store string                     `json:"store,omitempty"`
	Prior map[string]json.RawMessage `json:"prior,omitempty"`
	Set   map[string]json.RawMessage `json:"set,omitempty"`
	// scope: a systemd scope stopped (Name).
	// schedule: the Habakkuk schedule's ID; Name is agent/name.
	ScheduleID string `json:"schedule_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

// RecordPath is where finish keeps its record for a source.
func RecordPath(home, from string) string {
	return filepath.Join(home, "migrate", "cutover-"+from+".json")
}

// LoadRecord reads the record for a source; nil when there is none.
func LoadRecord(home, from string) (*Record, error) {
	path := RecordPath(home, from)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%s does not parse: %w", path, err)
	}
	r.path = path
	return &r, nil
}

func newRecord(home, from, source string, now time.Time) *Record {
	return &Record{Version: 1, From: from, Source: source, Started: now, path: RecordPath(home, from)}
}

// intend records a change before it is made and returns its index. An
// error means the record could not be written, and the change must not
// be made.
func (r *Record) intend(c Change) (int, error) {
	c.State = Pending
	r.Changes = append(r.Changes, c)
	if err := r.save(); err != nil {
		r.Changes = r.Changes[:len(r.Changes)-1]
		return -1, err
	}
	return len(r.Changes) - 1, nil
}

// mark sets a change's state and saves.
func (r *Record) mark(i int, state string) error {
	r.Changes[i].State = state
	return r.save()
}

// saveHook, when set, runs before each save; tests use it to make the
// record fail partway.
var saveHook func(r *Record) error

// save writes the record through a temporary file, readable only by you,
// and flushes it and its folder to disk.
func (r *Record) save() error {
	if saveHook != nil {
		if err := saveHook(r); err != nil {
			return err
		}
	}
	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cutover-*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(data, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), r.path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return platform.SyncDir(dir)
}

// retire moves a record that rollback has undone aside, so the next
// finish starts a new one; the old one stays for reference.
func (r *Record) retire(now time.Time) (string, error) {
	dest := r.path[:len(r.path)-len(".json")] + ".rolled-back-" + now.UTC().Format("20060102T150405Z") + ".json"
	return dest, os.Rename(r.path, dest)
}

// RolledBack lists the records of moves from a source that rollback has
// already undone, oldest first.
func RolledBack(home, from string) []string {
	m, _ := filepath.Glob(filepath.Join(home, "migrate", "cutover-"+from+".rolled-back-*.json"))
	return m
}
