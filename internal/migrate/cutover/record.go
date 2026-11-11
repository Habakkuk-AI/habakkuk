package cutover

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Record is what hk migrate finish changed, kept in
// $HABAKKUK_HOME/migrate/cutover-<from>.json so hk migrate rollback can
// undo exactly that. It is written after every change, so a finish that
// stops halfway still leaves a record of what it did.
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
)

// Change is one thing finish changed.
type Change struct {
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
	// service
	Service *Service      `json:"service,omitempty"`
	Was     *ServiceState `json:"was,omitempty"`
	// process
	PID     int    `json:"pid,omitempty"`
	Cmdline string `json:"cmdline,omitempty"`
	Role    string `json:"role,omitempty"`
	// old-job: its id and name in the old agent, and the store it is in
	// (a jobs file or a database).
	JobID string `json:"job_id,omitempty"`
	Store string `json:"store,omitempty"`
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

// add records a change and saves at once.
func (r *Record) add(c Change) error {
	r.Changes = append(r.Changes, c)
	return r.save()
}

// save writes the record through a temporary file, readable only by you.
func (r *Record) save() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".cutover-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), r.path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// retire moves a record that rollback has undone aside, so the next
// finish starts a new one; the old one stays for reference.
func (r *Record) retire(now time.Time) (string, error) {
	dest := r.path[:len(r.path)-len(".json")] + ".rolled-back-" + now.UTC().Format("20060102T150405Z") + ".json"
	return dest, os.Rename(r.path, dest)
}
