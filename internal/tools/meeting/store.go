// Package meeting attends video calls: Habakkuk joins a Google Meet, Zoom or
// Microsoft Teams call as a named guest in a browser of its own, turns on
// the platform's live captions, reads them from the page into a transcript,
// and when the call ends writes notes (decisions and action items) for the
// chat that asked. An attendance is a durable job of kind "meeting"; its
// state lives in files under $HABAKKUK_HOME/meetings/<id>, not in control.db.
package meeting

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// Status is where an attendance stands.
type Status string

const (
	StatusQueued  Status = "queued"  // the job has not started
	StatusJoining Status = "joining" // opening the page, entering the name
	StatusWaiting Status = "waiting" // in the lobby, waiting to be let in
	StatusInCall  Status = "in_call"
	StatusEnded   Status = "ended"  // left after being in the call (or asked to leave first)
	StatusFailed  Status = "failed" // never got in, or broke
)

// Finished reports whether the attendance is over.
func (s Status) Finished() bool { return s == StatusEnded || s == StatusFailed }

// Record is one attendance, kept in meeting.json.
type Record struct {
	ID          string          `json:"id"`
	Agent       string          `json:"agent"`
	Session     string          `json:"session,omitempty"` // the session that asked
	Origin      json.RawMessage `json:"origin,omitempty"`  // the chat to send the notes to
	URL         string          `json:"url"`               // as given
	JoinURL     string          `json:"join_url"`          // the page the browser opens
	Platform    Platform        `json:"platform"`
	Name        string          `json:"name"`     // display name in the call
	LimitMS     int64           `json:"limit_ms"` // longest stay in the call
	NotesPrompt string          `json:"notes_prompt,omitempty"`
	Job         string          `json:"job,omitempty"`

	Status    Status    `json:"status"`
	Detail    string    `json:"detail,omitempty"` // what is happening now, or why it ended
	Created   time.Time `json:"created"`
	JoinedAt  time.Time `json:"joined_at,omitzero"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
	Lines     int       `json:"lines"`
	LastLine  time.Time `json:"last_line,omitzero"`
	Captions  bool      `json:"captions"`  // live captions are on
	Announced bool      `json:"announced"` // presence posted in the meeting chat
	Notes     bool      `json:"notes"`     // notes written
	Delivered bool      `json:"delivered"` // notes sent to the chat
}

// Limit is the longest stay in the call.
func (r *Record) Limit() time.Duration { return time.Duration(r.LimitMS) * time.Millisecond }

// Line is one finished caption: who spoke, what was said, and when it
// first appeared.
type Line struct {
	Time    time.Time `json:"time"`
	Speaker string    `json:"speaker,omitempty"`
	Text    string    `json:"text"`
}

// Store keeps attendances as directories: meeting.json (the record),
// transcript.jsonl (one Line per line, appended), notes.md, a "leave" file
// asking the attendance to end, and browser/ (the profile, removed when the
// attendance ends).
type Store struct {
	Dir string
	mu  sync.Mutex
}

// NewStore returns the store under dir ($HABAKKUK_HOME/meetings).
func NewStore(dir string) *Store { return &Store{Dir: dir} }

var idPattern = regexp.MustCompile(`^mt_[0-9a-f]{8,32}$`)

// ErrNotFound means there is no attendance with that ID.
var ErrNotFound = errors.New("no such meeting")

func (s *Store) dir(id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("%w: %q (IDs look like mt_0123abcd)", ErrNotFound, id)
	}
	return filepath.Join(s.Dir, id), nil
}

// BrowserDir is where the attendance's browser keeps its profile.
func (s *Store) BrowserDir(id string) string { return filepath.Join(s.Dir, id, "browser") }

// Create writes a new record.
func (s *Store) Create(r *Record) error {
	d, err := s.dir(r.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	return s.Save(r)
}

// Save replaces the record.
func (s *Store) Save(r *Record) error {
	d, err := s.dir(r.ID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return store.WriteFileAtomic(filepath.Join(d, "meeting.json"), append(data, '\n'), 0o600)
}

// Get reads a record.
func (s *Store) Get(id string) (*Record, error) {
	d, err := s.dir(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(d, "meeting.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("meeting %s: %w", id, err)
	}
	return &r, nil
}

// List returns every record, newest first.
func (s *Store) List() ([]*Record, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Record
	for _, e := range entries {
		if !e.IsDir() || !idPattern.MatchString(e.Name()) {
			continue
		}
		r, err := s.Get(e.Name())
		if err != nil {
			continue // half-created or damaged; skipped
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b *Record) int { return b.Created.Compare(a.Created) })
	return out, nil
}

// AppendLines adds finished captions to the transcript.
func (s *Store) AppendLines(id string, lines []Line) error {
	if len(lines) == 0 {
		return nil
	}
	d, err := s.dir(id)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, l := range lines {
		data, err := json.Marshal(l)
		if err != nil {
			return err
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	f, err := os.OpenFile(filepath.Join(d, "transcript.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Lines reads the transcript. A line cut short by a crash is skipped.
func (s *Store) Lines(id string) ([]Line, error) {
	d, err := s.dir(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(d, "transcript.jsonl"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Line
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var l Line
		if json.Unmarshal(sc.Bytes(), &l) == nil && l.Text != "" {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

// RequestLeave asks the attendance to leave the call. It works from another
// process (habakkuk meetings leave): the attendance checks for the file every
// few seconds.
func (s *Store) RequestLeave(id string) error {
	d, err := s.dir(id)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(d, "meeting.json")); err != nil {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return os.WriteFile(filepath.Join(d, "leave"), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// LeaveRequested reports whether someone asked the attendance to leave.
func (s *Store) LeaveRequested(id string) bool {
	d, err := s.dir(id)
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(d, "leave"))
	return err == nil
}

// WriteNotes saves the meeting notes.
func (s *Store) WriteNotes(id, text string) error {
	d, err := s.dir(id)
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(filepath.Join(d, "notes.md"), []byte(strings.TrimSpace(text)+"\n"), 0o600)
}

// Notes reads the meeting notes; "" when there are none yet.
func (s *Store) Notes(id string) (string, error) {
	d, err := s.dir(id)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(d, "notes.md"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(data)), err
}

// Prune deletes finished attendances that ended before cutoff.
func (s *Store) Prune(cutoff time.Time) (int, error) {
	list, err := s.List()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range list {
		end := r.EndedAt
		if end.IsZero() {
			end = r.Created
		}
		if !r.Status.Finished() || !end.Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.Dir, r.ID)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// FormatTranscript renders lines as text, one turn per line; consecutive
// lines of one speaker less than 30 seconds apart are joined.
func FormatTranscript(lines []Line, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	var b strings.Builder
	var last Line
	for i, l := range lines {
		if i > 0 && l.Speaker == last.Speaker && l.Time.Sub(last.Time) < 30*time.Second {
			b.WriteString(" " + l.Text)
			last.Time = l.Time
			continue
		}
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[%s] ", l.Time.In(loc).Format("15:04:05"))
		if l.Speaker != "" {
			b.WriteString(l.Speaker + ": ")
		}
		b.WriteString(l.Text)
		last = l
	}
	return b.String()
}
