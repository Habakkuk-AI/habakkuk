package cutover

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// A jobs file is a JSON file of schedules: {"jobs": [...]} or a bare
// list, each job an object with an "id" and an "enabled" flag. Hermes
// Agent keeps one per home (cron/jobs.json). The cutover edits it only
// while nothing of the old agent runs, keeps a copy of the file before
// its first edit, and changes only the fields that pause or resume a job,
// so every other field (and every number) stays as it was.

// pauseReason marks a job the cutover paused, so rollback resumes only
// those and never one you paused yourself.
const pauseReason = "moved to Habakkuk (hk migrate finish)"

type jobsFile struct {
	path string
	list bool // a bare list rather than {"jobs": [...]}
	doc  map[string]any
	jobs []map[string]any
	mode os.FileMode
}

func readJobsFile(path string) (*jobsFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	f := &jobsFile{path: path, mode: info.Mode().Perm()}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	switch t := v.(type) {
	case map[string]any:
		f.doc = t
		raw, _ := t["jobs"].([]any)
		for _, j := range raw {
			if m, ok := j.(map[string]any); ok {
				f.jobs = append(f.jobs, m)
			}
		}
	case []any:
		f.list = true
		for _, j := range t {
			if m, ok := j.(map[string]any); ok {
				f.jobs = append(f.jobs, m)
			}
		}
	default:
		return nil, fmt.Errorf("%s holds no list of jobs", path)
	}
	return f, nil
}

// write saves the file through a temporary file beside it.
func (f *jobsFile) write() error {
	var v any
	list := make([]any, len(f.jobs))
	for i, j := range f.jobs {
		list[i] = j
	}
	if f.list {
		v = list
	} else {
		f.doc["jobs"] = list
		v = f.doc
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.path), ".jobs-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Chmod(f.mode); err != nil && !errors.Is(err, os.ErrInvalid) {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), f.path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// backup copies the file aside once, before the first change.
func (f *jobsFile) backup() error {
	dest := f.path + ".before-habakkuk"
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	data, err := os.ReadFile(f.path)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0o600)
}

func jobID(j map[string]any) string {
	id, _ := j["id"].(string)
	if id == "" {
		id = fmt.Sprint(j["id"])
		if id == "<nil>" {
			id = ""
		}
	}
	return id
}

func jobName(j map[string]any) string {
	if n, _ := j["name"].(string); n != "" {
		return n
	}
	return jobID(j)
}

// jobRunnable is whether a job would fire: on, not paused, and not
// completed. A job with no "enabled" field is on, and so is a recurring
// one in its error state, which the old agent may still retry.
func jobRunnable(j map[string]any) bool {
	if on, ok := j["enabled"].(bool); ok && !on {
		return false
	}
	state, _ := j["state"].(string)
	switch strings.TrimSpace(state) {
	case "paused", "completed":
		return false
	}
	if p, ok := j["paused_at"]; ok && p != nil && p != "" {
		return false
	}
	return true
}

// runnableJobs lists the jobs in a file that would fire.
func runnableJobs(path, agent string) ([]Job, error) {
	f, err := readJobsFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Job
	for _, j := range f.jobs {
		if id := jobID(j); id != "" && jobRunnable(j) {
			a := agent
			if a == "" {
				a, _ = j["agentId"].(string) // OpenClaw's older file
			}
			out = append(out, Job{ID: id, Name: jobName(j), Agent: a, Store: path})
		}
	}
	return out, nil
}

// pauseJobs pauses the jobs with these ids the way the old agent's own
// pause command does: off and, with marks, marked paused with the time
// and a reason. Without marks only "enabled" changes (a file format that
// has no pause marker).
func pauseJobs(path string, ids []string, now time.Time, marks bool) ([]string, error) {
	f, err := readJobsFile(path)
	if err != nil {
		return nil, err
	}
	var done []string
	for _, j := range f.jobs {
		id := jobID(j)
		if !slices.Contains(ids, id) || !jobRunnable(j) {
			continue
		}
		j["enabled"] = false
		if marks {
			j["state"] = "paused"
			j["paused_at"] = now.Format("2006-01-02T15:04:05.000000-07:00")
			j["paused_reason"] = pauseReason
		}
		done = append(done, id)
	}
	if len(done) == 0 {
		return nil, nil
	}
	if err := f.backup(); err != nil {
		return nil, err
	}
	if err := f.write(); err != nil {
		return nil, err
	}
	return done, nil
}

// resumeJob turns a job the cutover paused back on. Its next run time is
// left as it was, so the old agent's own catch-up rule decides what
// happens to a run it missed. With marks, a job paused since then by
// someone else (another reason) is left alone.
func resumeJob(path, id string, marks bool) error {
	f, err := readJobsFile(path)
	if err != nil {
		return err
	}
	for _, j := range f.jobs {
		if jobID(j) != id {
			continue
		}
		if jobRunnable(j) {
			return nil // already on
		}
		if !marks {
			j["enabled"] = true
			return f.write()
		}
		if reason, _ := j["paused_reason"].(string); reason != pauseReason {
			return errors.New("it was paused again since, not by hk migrate finish")
		}
		j["enabled"] = true
		j["state"] = "scheduled"
		j["paused_at"] = nil
		j["paused_reason"] = nil
		return f.write()
	}
	return errors.New("it is no longer there")
}
