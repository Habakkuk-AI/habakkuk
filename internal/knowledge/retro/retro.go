// Package retro runs agents' retrospectives. A retro looks back over a
// period of one agent's work in Habakkuk's own records (turns, jobs,
// approvals, schedules, the board, deliveries, memory changes and signs
// the user was unhappy), has a model find what went well, what went badly
// and why, and propose concrete changes, then applies the safe ones or
// leaves them for the owner to approve. The next retro is shown the last
// one's actions and judges whether they helped.
//
// A retro is a durable job whose id derives from the agent and period, so
// it is queued once however often the scheduler looks. The job gathers
// the evidence (no model), asks the model once and stores the plan with
// its actions in memory.db, then works through the actions, recording
// progress after each. A job resumed after a crash reuses the stored plan
// and continues at the first action not yet looked at; applying an action
// twice changes nothing the second time. Skill, memory and SOUL.md changes
// go through the memory history, so each can be undone.
package retro

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/consolidate"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/skills"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/work/schedule"
)

// Kind is the job kind of a retro.
const Kind = "retro"

// Apply modes.
const (
	ApplyPropose = "propose" // nothing changes until the owner approves
	ApplySafe    = "safe"    // skill and memory changes are applied at once
)

// Action statuses.
const (
	Proposed = "proposed" // waiting for the owner
	Applied  = "applied"  // made, and recorded in the memory history
	Approved = "approved" // the owner said yes to a change only they can make
	Rejected = "rejected"
	Failed   = "failed"
)

const (
	callTimeout = 5 * time.Minute
	planTokens  = 8192
	lookback    = 24 * time.Hour // a scheduled slot older than this is not run late
)

// Sender is the model call. *llm.Router implements it.
type Sender interface {
	Send(ctx context.Context, chain []llm.Target, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error)
}

// Runner queues, runs and applies retros.
type Runner struct {
	Home    string
	Mem     *memory.Store // memory.db holds the retros; notes are written through it
	Skills  *skills.Library
	Queue   *jobs.Queue
	Models  Sender
	Sources Sources
	// Agents are the agents a retro can look at.
	Agents map[string]Agent
	// Apply is ApplyPropose or ApplySafe.
	Apply string
	// Spec is the cron expression scheduled retros follow, in Location;
	// "" turns them off. Only names the agents that get them; empty means
	// every agent with turns in the period.
	Spec     string
	Only     []string
	Location *time.Location
	// Schedules lists an agent's schedules for the model and pauses one
	// when the owner approves.
	Schedules *schedule.Store
	// Deliver queues a summary to a `habakkuk send` target; id makes it
	// idempotent.
	Deliver func(ctx context.Context, id, target, text string) error
	Wake    func()
	Log     *slog.Logger
	Now     func() time.Time

	// crashAfter, in tests, stops a job before looking at action n (0-based)
	// as if the process died.
	crashAfter func(n int) bool
}

var errCrash = errors.New("simulated crash")

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (r *Runner) db() *sql.DB { return r.Mem.DB() }

// JobID is the id of an agent's retro for a period: the same period always
// gets the same job.
func JobID(agent string, from, to time.Time) string {
	const f = "20060102T1504"
	return "retro_" + agent + "_" + from.UTC().Format(f) + "_" + to.UTC().Format(f)
}

// Recent is the period of an on-demand retro: the last days up to the end
// of the current minute, so asking twice in a minute gets the same retro.
func Recent(days int, now time.Time) (from, to time.Time) {
	to = now.Truncate(time.Minute).Add(time.Minute)
	return to.AddDate(0, 0, -days), to
}

type payload struct {
	From  int64 `json:"from"` // unix ms
	To    int64 `json:"to"`
	Quiet bool  `json:"quiet,omitempty"` // asked for on demand: the asker gets the summary
}

// Enqueue queues a retro of an agent's work from from to to. A quiet retro
// is not delivered anywhere: whoever asked for it reads the result.
func (r *Runner) Enqueue(ctx context.Context, agent string, from, to time.Time, quiet bool) (string, error) {
	if _, ok := r.Agents[agent]; !ok {
		return "", fmt.Errorf("unknown agent %q", agent)
	}
	if !from.Before(to) {
		return "", errors.New("the period is empty")
	}
	id := JobID(agent, from, to)
	if _, err := r.Queue.Enqueue(ctx, jobs.NewJob{
		ID: id, Kind: Kind, Agent: agent, SerialKey: consolidate.SerialKey(agent), Priority: -1,
		Payload: payload{From: from.UnixMilli(), To: to.UnixMilli(), Quiet: quiet},
	}); err != nil {
		return "", err
	}
	if r.Wake != nil {
		r.Wake()
	}
	return id, nil
}

// Tick queues the scheduled retro of every agent due one: the latest slot
// of Spec within the last day, covering the time since the slot before.
// Job ids make repeated ticks harmless.
func (r *Runner) Tick(ctx context.Context) error {
	if r.Spec == "" {
		return nil
	}
	st := schedule.NewStore(nil, r.Location)
	now := r.now()
	var slot time.Time
	for t, i := now.Add(-lookback), 0; i < 1000; i++ {
		next, err := st.Next(r.Spec, t)
		if err != nil {
			return err
		}
		if next.IsZero() || next.After(now) {
			break
		}
		slot, t = next, next
	}
	if slot.IsZero() {
		return nil
	}
	after, err := st.Next(r.Spec, slot)
	if err != nil {
		return err
	}
	period := after.Sub(slot)
	period = min(max(period, time.Hour), 31*24*time.Hour)
	from := slot.Add(-period)
	agents := r.Only
	if len(agents) == 0 {
		if agents, err = r.active(ctx, from, slot); err != nil {
			return err
		}
	}
	for _, name := range agents {
		if _, ok := r.Agents[name]; !ok {
			continue
		}
		if _, err := r.Enqueue(ctx, name, from, slot, false); err != nil {
			return err
		}
	}
	return nil
}

// active lists the agents with turns in the period.
func (r *Runner) active(ctx context.Context, from, to time.Time) ([]string, error) {
	if r.Sources.Transcripts == nil {
		return nil, nil
	}
	rows, err := r.Sources.Transcripts.QueryContext(ctx, `SELECT DISTINCT agent FROM sessions
		WHERE updated_at >= ? AND created_at < ? ORDER BY agent`, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Handle is the jobs.Handler for Kind.
func (r *Runner) Handle(ctx context.Context, j *jobs.Job, owner string) (string, error) {
	var p payload
	if err := j.Decode(&p); err != nil {
		return "", jobs.Permanent{Err: err}
	}
	ag, ok := r.Agents[j.Agent]
	if !ok {
		return "", jobs.Permanent{Err: fmt.Errorf("agent %q is not configured", j.Agent)}
	}
	ctx = llm.WithCaller(ctx, llm.Caller{Agent: j.Agent, Session: "retro"})
	ret, err := r.Get(ctx, j.ID)
	if err != nil && !errors.Is(err, errNoRetro) {
		return "", err
	}
	if ret != nil && ret.Done {
		return ret.Summary, nil
	}
	if ret == nil {
		from, to := time.UnixMilli(p.From), time.UnixMilli(p.To)
		ev, plan, err := r.Plan(ctx, ag, from, to)
		if be := new(llm.BudgetError); errors.As(err, &be) {
			r.log().Info("retro skipped by a spending limit", "job", j.ID, "agent", j.Agent, "limit", be.Key)
			return "skipped: " + be.Message, nil
		}
		if err != nil {
			return "", err
		}
		if err := r.Queue.Owns(ctx, j.ID, owner); err != nil {
			return "", err
		}
		if err := r.insert(ctx, &Retro{ID: j.ID, Agent: j.Agent, From: from, To: to, Quiet: p.Quiet, Plan: *plan, Evidence: ev}); err != nil {
			return "", err
		}
		if ret, err = r.Get(ctx, j.ID); err != nil {
			return "", err
		}
	}
	for i := ret.Next; i < len(ret.Actions); i++ {
		if r.crashAfter != nil && r.crashAfter(i) {
			return "", errCrash
		}
		if err := r.Queue.Owns(ctx, j.ID, owner); err != nil {
			return "", err
		}
		a := &ret.Actions[i]
		if r.Apply == ApplySafe && a.Status == Proposed && (a.Kind == KindSkill || a.Kind == KindMemory) {
			actx := history.WithActor(ctx, history.Actor{By: history.ByRetro, Tool: "retro", Background: true,
				Tainted: true, Note: actionNote(ret.ID, a.N)})
			status, result, err := r.apply(actx, ret, a, true)
			if err != nil {
				if ctx.Err() != nil {
					return "", err
				}
				status, result = Failed, err.Error()
				r.log().Warn("retro change not applied", "job", j.ID, "action", a.N, "kind", a.Kind, "err", err)
			}
			if err := r.setAction(ctx, ret.ID, a.N, status, result); err != nil {
				return "", err
			}
			a.Status, a.Result = status, result
		}
		if _, err := r.db().ExecContext(ctx, `UPDATE retros SET next = ?, updated_at = ? WHERE id = ?`, i+1, r.now().UnixMilli(), ret.ID); err != nil {
			return "", err
		}
		ret.Next = i + 1
	}
	if err := r.Queue.Owns(ctx, j.ID, owner); err != nil {
		return "", err
	}
	if err := r.writeReport(ctx, ret); err != nil {
		return "", err
	}
	if !ret.Delivered && !ret.Quiet && !ret.Plan.Quiet() && ag.Deliver != "" && r.Deliver != nil {
		if err := r.Deliver(ctx, "retro_dlv_"+ret.ID, ag.Deliver, ChatSummary(ret)); err != nil {
			// The report is written; a chat that cannot be reached will
			// not become reachable on a retry.
			r.log().Warn("retro summary not delivered", "job", j.ID, "to", ag.Deliver, "err", err)
		} else if _, err := r.db().ExecContext(ctx, `UPDATE retros SET delivered = 1 WHERE id = ?`, ret.ID); err != nil {
			return "", err
		}
	}
	summary := ret.Line()
	if _, err := r.db().ExecContext(ctx, `UPDATE retros SET done = 1, summary = ?, updated_at = ? WHERE id = ?`,
		summary, r.now().UnixMilli(), ret.ID); err != nil {
		return "", err
	}
	r.log().Info("retro done", "agent", j.Agent, "job", j.ID, "summary", summary)
	return summary, nil
}

// Plan gathers the evidence for an agent and period and asks the model
// for its plan. It changes nothing: Handle stores the plan, and a dry run
// only shows it.
func (r *Runner) Plan(ctx context.Context, ag Agent, from, to time.Time) (*Evidence, *Plan, error) {
	ev, err := Gather(ctx, r.Sources, ag, from, to)
	if err != nil {
		return nil, nil, err
	}
	prev, err := r.previous(ctx, ag.Name, to)
	if err != nil {
		return nil, nil, err
	}
	soul, _ := os.ReadFile(filepath.Join(ag.Dir, history.SoulFile))
	var list []skills.Skill
	if r.Skills != nil {
		list, _ = r.Skills.List(ag.Name)
	}
	var scheds []string
	if r.Schedules != nil {
		if all, err := r.Schedules.List(ctx, ag.Name); err == nil {
			for _, s := range all {
				state := "on"
				if !s.Enabled {
					state = "paused"
				}
				scheds = append(scheds, fmt.Sprintf("- %s %q (%s, %s): %s", s.ID, s.Name, s.Spec, state, cutText(oneLine(s.Prompt), 200)))
			}
		}
	}
	if len(ag.Chain) == 0 {
		return nil, nil, fmt.Errorf("agent %s has no models", ag.Name)
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	resp, err := r.Models.Send(cctx, ag.Chain, llm.Request{
		System: systemPrompt, MaxTokens: planTokens,
		Messages: []llm.Message{llm.UserText(input(ev, string(soul), list, scheds, prev))},
	}, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("retro model: %w", err)
	}
	plan, err := ParsePlan(resp.Message.Text(), ev, prev)
	if err != nil {
		return nil, nil, fmt.Errorf("retro model answer: %w", err)
	}
	return ev, plan, nil
}

// Retro is one stored retro.
type Retro struct {
	ID        string        `json:"id"`
	Agent     string        `json:"agent"`
	From      time.Time     `json:"from"`
	To        time.Time     `json:"to"`
	Quiet     bool          `json:"quiet"`
	Plan      Plan          `json:"plan"`
	Evidence  *Evidence     `json:"evidence,omitempty"`
	Actions   []ActionState `json:"actions"`
	Next      int           `json:"-"`
	Report    string        `json:"report,omitempty"` // relative to the home
	Delivered bool          `json:"delivered"`
	Done      bool          `json:"done"`
	Summary   string        `json:"summary,omitempty"` // one line, once done
	Created   time.Time     `json:"created_at"`
}

// ActionState is a proposed action and what became of it.
type ActionState struct {
	N int `json:"n"`
	Action
	Status  string    `json:"status"`
	Result  string    `json:"result,omitempty"`
	Updated time.Time `json:"updated_at"`
}

// Line is the retro in one line, for logs and lists.
func (ret *Retro) Line() string {
	if ret.Plan.Quiet() {
		return "nothing notable"
	}
	counts := map[string]int{}
	for _, a := range ret.Actions {
		counts[a.Status]++
	}
	s := fmt.Sprintf("%d went badly, %d proposed", len(ret.Plan.WentBadly), len(ret.Actions))
	for _, st := range []string{Applied, Approved, Rejected, Failed} {
		if counts[st] > 0 {
			s += fmt.Sprintf(", %d %s", counts[st], st)
		}
	}
	return s
}

var errNoRetro = errors.New("no such retro")

func (r *Runner) insert(ctx context.Context, ret *Retro) error {
	plan, err := json.Marshal(ret.Plan)
	if err != nil {
		return err
	}
	ev, err := json.Marshal(ret.Evidence)
	if err != nil {
		return err
	}
	now := r.now().UnixMilli()
	tx, err := r.db().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO retros (id, agent, period_from, period_to, quiet, plan, evidence, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		ret.ID, ret.Agent, ret.From.UnixMilli(), ret.To.UnixMilli(), ret.Quiet, string(plan), string(ev), now, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // stored by an earlier attempt
	}
	for i, a := range ret.Plan.Actions {
		data, err := json.Marshal(a)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO retro_actions (retro, n, kind, target, action, status, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, ret.ID, i+1, a.Kind, a.Target, string(data), Proposed, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Get reads a retro with its actions and evidence.
func (r *Runner) Get(ctx context.Context, id string) (*Retro, error) {
	ret := &Retro{}
	var from, to, created int64
	var plan, ev string
	err := r.db().QueryRowContext(ctx, `SELECT id, agent, period_from, period_to, quiet, plan, evidence, next, report,
		delivered, done, summary, created_at FROM retros WHERE id = ?`, id).Scan(&ret.ID, &ret.Agent, &from, &to, &ret.Quiet,
		&plan, &ev, &ret.Next, &ret.Report, &ret.Delivered, &ret.Done, &ret.Summary, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w %q", errNoRetro, id)
	}
	if err != nil {
		return nil, err
	}
	ret.From, ret.To, ret.Created = time.UnixMilli(from), time.UnixMilli(to), time.UnixMilli(created)
	if err := json.Unmarshal([]byte(plan), &ret.Plan); err != nil {
		return nil, fmt.Errorf("retro %s plan: %w", id, err)
	}
	if err := json.Unmarshal([]byte(ev), &ret.Evidence); err != nil {
		return nil, fmt.Errorf("retro %s evidence: %w", id, err)
	}
	if ret.Actions, err = r.actions(ctx, id); err != nil {
		return nil, err
	}
	return ret, nil
}

func (r *Runner) actions(ctx context.Context, id string) ([]ActionState, error) {
	rows, err := r.db().QueryContext(ctx, `SELECT n, action, status, result, updated_at FROM retro_actions WHERE retro = ? ORDER BY n`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActionState
	for rows.Next() {
		var a ActionState
		var data string
		var updated int64
		if err := rows.Scan(&a.N, &data, &a.Status, &a.Result, &updated); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(data), &a.Action); err != nil {
			return nil, err
		}
		a.Updated = time.UnixMilli(updated)
		out = append(out, a)
	}
	return out, rows.Err()
}

// List returns an agent's (or with "", every agent's) retros, newest
// first, without their evidence.
func (r *Runner) List(ctx context.Context, agent string, limit int) ([]Retro, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := r.db().QueryContext(ctx, `SELECT id FROM retros WHERE (? = '' OR agent = ?)
		ORDER BY period_to DESC, created_at DESC LIMIT ?`, agent, agent, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Retro, 0, len(ids))
	for _, id := range ids {
		ret, err := r.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		ret.Evidence = nil
		out = append(out, *ret)
	}
	return out, nil
}

// Resolve finds a retro by id, or the agent's latest for "" or "latest".
func (r *Runner) Resolve(ctx context.Context, agent, id string) (*Retro, error) {
	if id != "" && id != "latest" {
		return r.Get(ctx, id)
	}
	list, err := r.List(ctx, agent, 1)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("agent %s has no retros yet", agent)
	}
	return r.Get(ctx, list[0].ID)
}

// previous is the agent's last finished retro before to, for follow-up.
func (r *Runner) previous(ctx context.Context, agent string, to time.Time) (*Previous, error) {
	var id string
	err := r.db().QueryRowContext(ctx, `SELECT id FROM retros WHERE agent = ? AND done = 1 AND period_to <= ?
		ORDER BY period_to DESC, created_at DESC LIMIT 1`, agent, to.UnixMilli()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ret, err := r.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(ret.Actions) == 0 && len(ret.Plan.WentBadly) == 0 {
		return nil, nil // nothing to follow up
	}
	return &Previous{ID: ret.ID, Summary: ret.Plan.Summary, Badly: ret.Plan.WentBadly, Actions: ret.Actions}, nil
}

func (r *Runner) setAction(ctx context.Context, id string, n int, status, result string) error {
	_, err := r.db().ExecContext(ctx, `UPDATE retro_actions SET status = ?, result = ?, updated_at = ? WHERE retro = ? AND n = ?`,
		status, cutText(result, 4000), r.now().UnixMilli(), id, n)
	return err
}

func actionNote(id string, n int) string { return fmt.Sprintf("retro %s action %d", id, n) }

// Agents names the agents a runner knows, sorted.
func (r *Runner) AgentNames() []string {
	names := make([]string, 0, len(r.Agents))
	for n := range r.Agents {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// relPath is a report path relative to the home, with forward slashes.
func (r *Runner) relPath(path string) string {
	if rel, err := filepath.Rel(r.Home, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return path
}
