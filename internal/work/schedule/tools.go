package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// Here returns the reply address of the conversation a session belongs to,
// or nil when the session is not a chat (CLI, another schedule).
type Here func(ctx context.Context, sessionID string) json.RawMessage

// RegisterTools adds the "schedule" toolset. Creating a schedule grants no
// new power: a call that needs approval in a scheduled run asks the owner
// in the chat the schedule delivers to, or is denied when there is no such
// chat. An approval rule can answer only for a schedule made by a turn
// outside content did not steer: a schedule created by such a turn
// (tools.Env.Untrusted) is marked tainted, and so is every one of its runs.
// The tools therefore carry network risk (allowed in workspace mode,
// denied in read-only mode).
func RegisterTools(r *tools.Registry, st *Store, here Here) {
	r.Register("schedule", createTool{st, here})
	r.Register("schedule", listTool{st})
	r.Register("schedule", deleteTool{st})
	r.Register("schedule", pauseTool{st})
}

func decode[T any](input json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(input, &v); err != nil {
		return v, fmt.Errorf("invalid input: %w", err)
	}
	return v, nil
}

type createTool struct {
	st   *Store
	here Here
}

func (createTool) Risk() tools.Risk { return tools.RiskNetwork }
func (createTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "schedule_create",
		Description: `Schedule a prompt for yourself to run later, once or repeatedly (reminders, daily briefings, recurring checks). Each run starts a fresh conversation with only the prompt, so write the prompt to stand alone: say exactly what to do and what to report. "when" is a 5-field cron expression in the user's timezone ("0 9 * * 1-5" = weekdays 09:00), "@every 2h", "@daily", or a one-off "at YYYY-MM-DD HH:MM". Results are sent to the current chat unless deliver is false.`,
		Schema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","description":"short label"},
			"when":{"type":"string"},
			"prompt":{"type":"string"},
			"deliver":{"type":"boolean","description":"send each result to this chat (default true)"}
		},"required":["name","when","prompt"]}`),
	}
}

func (t createTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		Name    string `json:"name"`
		When    string `json:"when"`
		Prompt  string `json:"prompt"`
		Deliver *bool  `json:"deliver"`
		// Never copied into the schedule; present only to refuse clearly.
		Command json.RawMessage `json:"command"`
		Run     json.RawMessage `json:"run"`
	}](input)
	if err != nil {
		return "", err
	}
	if in.Command != nil || in.Run != nil {
		return "", errors.New("schedule_create cannot run commands; only the owner can add a script schedule (hk schedules add -run)")
	}
	// Script schedules run shell commands unattended, so this tool must never
	// set Command, Workdir or the other script fields.
	sc := Schedule{Agent: env.Agent, Name: in.Name, Spec: in.When, Prompt: in.Prompt, Tainted: env.Untrusted()}
	if (in.Deliver == nil || *in.Deliver) && t.here != nil {
		sc.DeliverTo = t.here(ctx, env.Session)
	}
	created, err := t.st.Create(ctx, sc)
	if err != nil {
		return "", err
	}
	where := "results stay in Habakkuk's session history (not a chat)"
	if len(created.DeliverTo) > 0 {
		where = "results will be sent to this chat"
	}
	out := fmt.Sprintf("Scheduled %s (%s). First run %s; %s.", created.ID, created.Spec,
		When(created.NextRun, t.st.loc), where)
	if note := ZoneNote(created.Spec, t.st.loc); note != "" {
		out += " " + note
	}
	return out, nil
}

// When shows a run time in loc, naming the zone (America/Toronto rather
// than EST) and adding UTC when loc is another zone, so a time typed in
// the wrong zone is plain to see.
func When(t time.Time, loc *time.Location) string {
	at := t.In(loc)
	zone := loc.String()
	if zone == "Local" || zone == "" {
		zone = at.Format("MST")
	}
	s := at.Format("Mon 2006-01-02 15:04") + " " + zone
	if _, off := at.Zone(); off != 0 {
		s += " (" + t.UTC().Format("15:04") + " UTC)"
	}
	return s
}

// ZoneNote explains, for a one-off "at" time written without a zone, that
// it was read in the schedule timezone; "" for anything else.
func ZoneNote(spec string, loc *time.Location) string {
	at, ok := strings.CutPrefix(strings.TrimSpace(spec), "at ")
	if !ok {
		return ""
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(at)); err == nil {
		return "" // it names its own zone
	}
	return fmt.Sprintf("The time was read in %s, the config timezone. For another zone, give an offset: at 2026-11-09T17:57:00Z is UTC.", loc)
}

// Location is the timezone schedules are read in.
func (s *Store) Location() *time.Location { return s.loc }

type listTool struct{ st *Store }

func (listTool) Risk() tools.Risk { return tools.RiskRead }
func (listTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "schedule_list",
		Description: "List your scheduled tasks with their next run and last result.",
		Schema:      json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func (t listTool) Run(ctx context.Context, env *tools.Env, _ json.RawMessage) (string, error) {
	list, err := t.st.List(ctx, env.Agent)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "No scheduled tasks.", nil
	}
	return Format(list, t.st.loc), nil
}

// Format renders schedules one per line; a script schedule gets a second
// line with its command and output mode.
func Format(list []Schedule, loc *time.Location) string {
	var b strings.Builder
	for _, sc := range list {
		state := "next " + sc.NextRun.In(loc).Format("Mon 2006-01-02 15:04")
		switch {
		case !sc.Enabled && strings.HasPrefix(sc.Spec, "at ") && !sc.LastRun.IsZero():
			state = "ran once"
		case !sc.Enabled:
			state = "paused"
		}
		fmt.Fprintf(&b, "%s  %-24q %-18s %s", sc.ID, sc.Name, sc.Spec, state)
		if sc.LastStatus != "" {
			fmt.Fprintf(&b, "  (last: %s %s)", oneLine(sc.LastStatus, 120), sc.LastRun.In(loc).Format("01-02 15:04"))
		}
		if sc.Command != "" {
			fmt.Fprintf(&b, "\n    script, output %s: $ %s", sc.OnOutput, oneLine(sc.Command, 100))
			if sc.FailStreak > 0 {
				fmt.Fprintf(&b, "  [failed %d in a row]", sc.FailStreak)
			}
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

type deleteTool struct{ st *Store }

func (deleteTool) Risk() tools.Risk { return tools.RiskNetwork }
func (deleteTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "schedule_delete",
		Description: "Delete a scheduled task by id (from schedule_list).",
		Schema:      json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
	}
}

func (t deleteTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		ID string `json:"id"`
	}](input)
	if err != nil {
		return "", err
	}
	if err := t.st.owned(ctx, in.ID, env.Agent); err != nil {
		return "", err
	}
	if err := t.st.Delete(ctx, in.ID); err != nil {
		return "", err
	}
	return "Deleted " + in.ID + ".", nil
}

type pauseTool struct{ st *Store }

func (pauseTool) Risk() tools.Risk { return tools.RiskNetwork }
func (pauseTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "schedule_pause",
		Description: "Pause or resume a scheduled task. Resuming continues from now; missed runs are not replayed.",
		Schema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"paused":{"type":"boolean"}},
			"required":["id","paused"]}`),
	}
}

func (t pauseTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		ID     string `json:"id"`
		Paused bool   `json:"paused"`
	}](input)
	if err != nil {
		return "", err
	}
	if err := t.st.owned(ctx, in.ID, env.Agent); err != nil {
		return "", err
	}
	if err := t.st.SetEnabled(ctx, in.ID, !in.Paused); err != nil {
		return "", err
	}
	if in.Paused {
		return "Paused " + in.ID + ".", nil
	}
	return "Resumed " + in.ID + ".", nil
}

// owned stops one agent from changing another agent's schedules.
func (s *Store) owned(ctx context.Context, id, agentName string) error {
	sc, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if sc.Agent != agentName {
		return fmt.Errorf("schedule %s belongs to another agent", id)
	}
	return nil
}
