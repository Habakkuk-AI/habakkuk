package meeting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
)

// Kind is the job kind of an attendance.
const Kind = "meeting"

// Model writes text with one model call of the agent's: the notes.
type Model func(ctx context.Context, agent, system, prompt string) (string, error)

// Deliver queues text to a chat's reply address; the ID makes a repeated
// call send once.
type Deliver func(ctx context.Context, id string, to json.RawMessage, text string) error

// Notify starts a turn in the session that asked, for attendances that came
// from no chat (the terminal, an API session). The ID makes it run once.
type Notify func(ctx context.Context, agent, session, id, text string) error

// Manager starts attendances and runs their jobs.
type Manager struct {
	Store    *Store
	Cfg      config.Meeting // with defaults applied
	Queue    *jobs.Queue
	Launch   Launcher
	Model    Model   // nil: no notes, only the transcript
	Deliver  Deliver // nil: no channels
	Notify   Notify  // nil: notes stay in the store when there is no chat
	Location *time.Location
	Log      *slog.Logger

	// For tests: the clock, the polling interval (default 1s) and how long
	// a caption must stay unchanged to be final (default 5s).
	Now    func() time.Time
	Tick   time.Duration
	Settle time.Duration
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (m *Manager) tick() time.Duration {
	if m.Tick > 0 {
		return m.Tick
	}
	return time.Second
}

func (m *Manager) loc() *time.Location {
	if m.Location != nil {
		return m.Location
	}
	return time.Local
}

func (m *Manager) setStatus(rec *Record, st Status, detail string) {
	rec.Status, rec.Detail = st, detail
	if err := m.Store.Save(rec); err != nil {
		m.log().Warn("saving the meeting record failed", "meeting", rec.ID, "err", err)
	}
}

func (m *Manager) setDetail(rec *Record, detail string) { m.setStatus(rec, rec.Status, detail) }

// JoinRequest asks Habakkuk to attend a meeting.
type JoinRequest struct {
	Agent       string
	Session     string
	Origin      json.RawMessage
	URL         string
	Name        string
	Limit       time.Duration // 0: the configured max_duration
	NotesPrompt string
}

// Join records an attendance and queues its job. When the agent is already
// attending that meeting it returns that attendance and true.
func (m *Manager) Join(ctx context.Context, req JoinRequest) (*Record, bool, error) {
	platform, joinURL, err := Detect(req.URL)
	if err != nil {
		return nil, false, err
	}
	name := displayName(req.Name)
	if name == "" {
		name = m.Cfg.Name
	}
	limit := req.Limit
	if limit <= 0 || limit > m.Cfg.MaxDuration {
		limit = m.Cfg.MaxDuration
	}
	list, err := m.Store.List()
	if err != nil {
		return nil, false, err
	}
	for _, r := range list {
		if r.Agent == req.Agent && r.JoinURL == joinURL && !r.Status.Finished() && !m.jobOver(ctx, r) {
			return r, true, nil
		}
	}
	rec := &Record{
		ID: jobs.NewID("mt"), Agent: req.Agent, Session: req.Session, Origin: req.Origin,
		URL: req.URL, JoinURL: joinURL, Platform: platform, Name: name, LimitMS: limit.Milliseconds(),
		NotesPrompt: strings.TrimSpace(req.NotesPrompt), Status: StatusQueued, Detail: "waiting for a worker",
		Created: m.now().UTC(),
	}
	rec.Job = "meeting_" + rec.ID
	if err := m.Store.Create(rec); err != nil {
		return nil, false, err
	}
	if _, err := m.Queue.Enqueue(ctx, jobs.NewJob{
		ID: rec.Job, Kind: Kind, Agent: req.Agent, SerialKey: "meeting:" + rec.ID,
		Payload: payload{Meeting: rec.ID}, MaxAttempts: 3,
	}); err != nil {
		return nil, false, err
	}
	return rec, false, nil
}

// jobOver reports whether the attendance's job stopped for good although
// the record was never finished (it was canceled, or failed every attempt
// in crashes).
func (m *Manager) jobOver(ctx context.Context, r *Record) bool {
	if m.Queue == nil || r.Job == "" {
		return false
	}
	j, err := m.Queue.Get(ctx, r.Job)
	if err != nil {
		return false
	}
	return j.Status == jobs.Failed || j.Status == jobs.Canceled
}

// Effective is the record as people should see it: an attendance whose job
// stopped for good shows as failed.
func (m *Manager) Effective(ctx context.Context, r *Record) *Record {
	if r.Status.Finished() || !m.jobOver(ctx, r) {
		return r
	}
	c := *r
	c.Status, c.Detail = StatusFailed, "the attendance stopped (its job ended without finishing)"
	if j, err := m.Queue.Get(ctx, r.Job); err == nil && j.LastError != "" {
		c.Detail += ": " + j.LastError
	}
	return &c
}

// displayName keeps a name short and printable.
func displayName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = clean(s)
	if r := []rune(s); len(r) > 60 {
		s = string(r[:60])
	}
	return s
}

type payload struct {
	Meeting string `json:"meeting"`
}

// Handle is the jobs.Handler for Kind. It attends the meeting, then writes
// and sends the notes. A process that stops mid-meeting leaves the job to
// be resumed: the next attempt joins again and appends to the same
// transcript. Notes are written and sent once, even across retries.
func (m *Manager) Handle(ctx context.Context, j *jobs.Job, _ string) (string, error) {
	var p payload
	if err := j.Decode(&p); err != nil {
		return "", jobs.Permanent{Err: err}
	}
	rec, err := m.Store.Get(p.Meeting)
	if err != nil {
		return "", jobs.Permanent{Err: err}
	}
	if !rec.Status.Finished() {
		st, reason, err := m.run(ctx, rec)
		if err != nil {
			return "", err
		}
		rec.Status, rec.Detail, rec.EndedAt = st, reason, m.now().UTC()
		if err := m.Store.Save(rec); err != nil {
			return "", err
		}
		m.log().Info("meeting attendance ended", "meeting", rec.ID, "status", st, "reason", reason, "lines", rec.Lines)
	}
	return m.wrapUp(ctx, rec)
}

// run attends, joining again (twice at most) when the browser dies.
func (m *Manager) run(ctx context.Context, rec *Record) (Status, string, error) {
	if m.Store.LeaveRequested(rec.ID) && rec.JoinedAt.IsZero() {
		return StatusEnded, "asked to leave before joining", nil
	}
	for try := 0; ; try++ {
		st, reason, err := m.attend(ctx, rec)
		if err == nil {
			return st, reason, nil
		}
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		if !errors.Is(err, errBrowserGone) || try >= 2 {
			return StatusFailed, err.Error(), nil
		}
		m.log().Warn("the meeting browser stopped; joining again", "meeting", rec.ID, "err", err)
		t := time.NewTimer(2 * m.tick())
		select {
		case <-ctx.Done():
			t.Stop()
			return "", "", ctx.Err()
		case <-t.C:
		}
	}
}

// wrapUp writes the notes (once) and sends them (once).
func (m *Manager) wrapUp(ctx context.Context, rec *Record) (string, error) {
	if !rec.Notes {
		text, err := m.notes(ctx, rec)
		if err != nil {
			return "", err
		}
		if err := m.Store.WriteNotes(rec.ID, text); err != nil {
			return "", err
		}
		rec.Notes = true
		if err := m.Store.Save(rec); err != nil {
			return "", err
		}
	}
	if !rec.Delivered {
		text, err := m.Store.Notes(rec.ID)
		if err != nil {
			return "", err
		}
		id := "meeting_notes_" + rec.ID
		switch {
		case len(rec.Origin) > 0 && m.Deliver != nil:
			err = m.Deliver(ctx, id, rec.Origin, text)
		case rec.Session != "" && m.Notify != nil:
			err = m.Notify(ctx, rec.Agent, rec.Session, id,
				"[Meeting "+rec.ID+" is over. These are the notes; give them to the user as they are.]\n\n"+text)
		}
		if err != nil {
			return "", fmt.Errorf("sending the meeting notes: %w", err)
		}
		rec.Delivered = true
		if err := m.Store.Save(rec); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%s %s (%s): %d caption lines", rec.ID, rec.Status, rec.Detail, rec.Lines), nil
}

// notes writes the notes. When the model cannot be reached they say so,
// and the transcript stays available; it fails only when ctx ends
// (shutting down), so the resumed job writes them.
func (m *Manager) notes(ctx context.Context, rec *Record) (string, error) {
	head := Headline(rec, m.loc())
	if rec.JoinedAt.IsZero() {
		return head + "\n\nHabakkuk could not join: " + rec.Detail + ".", nil
	}
	lines, err := m.Store.Lines(rec.ID)
	if err != nil {
		return head + "\n\nThe transcript could not be read: " + err.Error(), nil
	}
	if len(lines) == 0 {
		why := "nobody spoke while captions were on"
		if !rec.Captions {
			why = "captions could not be turned on"
		}
		return head + "\n\nNo captions were captured (" + why + ").", nil
	}
	if m.Model == nil {
		return head + "\n\nNo model is set up to write notes; read the transcript with meeting_transcript " + rec.ID + ".", nil
	}
	mctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	text, err := m.Model(mctx, rec.Agent, NotesSystem, NotesPrompt(rec, lines, m.loc()))
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil || strings.TrimSpace(text) == "" {
		if err == nil {
			err = errors.New("the model returned nothing")
		}
		m.log().Warn("writing meeting notes failed", "meeting", rec.ID, "err", err)
		return head + "\n\nThe notes could not be written (" + clip(err.Error(), 300) + "). The transcript is kept: meeting_transcript " + rec.ID + ".", nil
	}
	return head + "\n\n" + strings.TrimSpace(text), nil
}

// Headline is the first line of the notes: the platform, when, how long,
// and how it ended.
func Headline(rec *Record, loc *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Meeting notes: %s, meeting %s", rec.Platform.Title(), rec.ID)
	if !rec.JoinedAt.IsZero() {
		end := rec.EndedAt
		if end.IsZero() {
			end = time.Now()
		}
		fmt.Fprintf(&b, ", %s to %s (%s)", rec.JoinedAt.In(loc).Format("Mon Jan 2 15:04"), end.In(loc).Format("15:04"),
			end.Sub(rec.JoinedAt).Round(time.Minute))
	}
	if rec.Detail != "" && rec.Status.Finished() {
		fmt.Fprintf(&b, "; %s", rec.Detail)
	}
	return b.String()
}

// Describe renders a record for meeting_status and habakkuk meetings show.
func Describe(rec *Record, loc *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s  %s\n", rec.ID, rec.Platform.Title(), rec.Status)
	if rec.Detail != "" {
		fmt.Fprintf(&b, "  %s\n", rec.Detail)
	}
	fmt.Fprintf(&b, "  link: %s\n  name: %q, agent %s, limit %s\n", rec.URL, rec.Name, rec.Agent, rec.Limit())
	fmt.Fprintf(&b, "  asked: %s\n", rec.Created.In(loc).Format("Mon 2006-01-02 15:04"))
	if !rec.JoinedAt.IsZero() {
		fmt.Fprintf(&b, "  joined: %s\n", rec.JoinedAt.In(loc).Format("15:04:05"))
	}
	if !rec.EndedAt.IsZero() {
		fmt.Fprintf(&b, "  ended: %s\n", rec.EndedAt.In(loc).Format("15:04:05"))
	}
	fmt.Fprintf(&b, "  captions: %s, %d lines", onOff(rec.Captions), rec.Lines)
	if !rec.LastLine.IsZero() {
		fmt.Fprintf(&b, " (last %s)", rec.LastLine.In(loc).Format("15:04:05"))
	}
	fmt.Fprintf(&b, "\n  announced in chat: %s, notes: %s, sent: %s", yesNo(rec.Announced), yesNo(rec.Notes), yesNo(rec.Delivered))
	return b.String()
}

// Summary is one line per record for lists.
func Summary(rec *Record, loc *time.Location) string {
	when := rec.Created.In(loc).Format("01-02 15:04")
	dur := ""
	if !rec.JoinedAt.IsZero() {
		end := rec.EndedAt
		if end.IsZero() {
			end = time.Now()
		}
		dur = " " + end.Sub(rec.JoinedAt).Round(time.Minute).String()
	}
	return fmt.Sprintf("%s  %-8s %-16s %s%s  %d lines  %s", rec.ID, rec.Status, rec.Platform.Title(), when, dur, rec.Lines, clip(rec.Detail, 80))
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
