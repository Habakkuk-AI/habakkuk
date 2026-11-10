package retro

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/usage"
	"github.com/habakkuk-ai/habakkuk/internal/ops/logs"
)

// Limits on what evidence holds, so a busy week stays a readable prompt.
const (
	maxIssues      = 15  // failed, retried and interrupted jobs listed
	maxSamples     = 5   // examples per tool, approval kind, fallback
	maxSlowest     = 5   // slowest turns listed
	maxListed      = 15  // schedules, stalled work, deliveries, corrections
	maxChanges     = 30  // memory and skill changes listed
	maxSampleText  = 240 // characters of an error or a message
	maxRows        = 50000
	defaultStall   = 3 // days without progress before a card or task is stalled
	maxCorrectText = 400
)

// Sources are the stores evidence is read from. Nil fields are skipped.
type Sources struct {
	Control     *sql.DB // control.db: jobs, schedules, cards, conversations
	Transcripts *sql.DB // transcripts.db: sessions and messages
	Usage       *usage.Store
	Pricer      *usage.Pricer
	History     *history.Log
	// LogHome is where habakkuk.log is, for model fallbacks and their
	// reasons; "" skips the log.
	LogHome string
	// Undos lists an agent's workspace /undo uses.
	Undos func(ctx context.Context, agent string) ([]Undo, error)
	// Redact hides secrets in text taken from the stores.
	Redact func(string) string
	// StallDays is how long a card or delegated task may go without
	// progress before it counts as stalled (default 3).
	StallDays int
}

// Undo is one /undo of an agent's workspace.
type Undo struct {
	ID      string
	At      time.Time
	Session string
	Task    string
}

// Agent is what a retro needs to know about the agent it looks at.
type Agent struct {
	Name string
	// Chain is the models the retro itself asks, first to last.
	Chain []llm.Target
	// Models are the "<provider>/<model>" a turn is meant to use (the
	// main model, the fast model); answers from others are fallbacks.
	Models []string
	// Fallbacks are the agent's other models, for reading fallback reasons
	// from the log.
	Fallbacks []string
	Home      string // the home chat; deliveries there are the agent's
	Deliver   string // where its retro summaries go; "" for none
	Dir       string // agents/<agent>
}

// Evidence is what a retro looks at, gathered from Habakkuk's own records
// without a model. Every item names where it came from in Ref: job:ID,
// session:ID, card:ID, schedule:ID, change:ID, checkpoint:ID or log:TIME.
type Evidence struct {
	Agent string    `json:"agent"`
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`

	Sessions    int             `json:"sessions"`
	Turns       TurnStats       `json:"turns"`
	Jobs        []JobCount      `json:"jobs,omitempty"`
	Problems    []JobIssue      `json:"problems,omitempty"`
	ToolErrors  []ToolErrors    `json:"tool_errors,omitempty"`
	Approvals   Approvals       `json:"approvals"`
	Fallbacks   []Fallback      `json:"fallbacks,omitempty"`
	Cost        []Cost          `json:"cost,omitempty"`
	Schedules   []ScheduleIssue `json:"schedules,omitempty"`
	SchedulesOK int             `json:"schedules_ok"`
	CardsDone   int             `json:"cards_done"`
	Stalled     []Stalled       `json:"stalled,omitempty"`
	Deliveries  []Delivery      `json:"deliveries,omitempty"`
	Corrections []Correction    `json:"corrections,omitempty"`
	Changes     []Change        `json:"changes,omitempty"`
}

// TurnStats sums up the agent's turns.
type TurnStats struct {
	Count  int        `json:"count"`
	Done   int        `json:"done"`
	Failed int        `json:"failed"`
	P50MS  int64      `json:"p50_ms"`
	P95MS  int64      `json:"p95_ms"`
	Slow   []SlowTurn `json:"slowest,omitempty"`
}

// SlowTurn is one of the slowest turns, with the message that started it.
type SlowTurn struct {
	Ref     string `json:"ref"`
	Session string `json:"session"`
	MS      int64  `json:"ms"`
	First   string `json:"first"`
}

// JobCount is how the agent's jobs of one kind went.
type JobCount struct {
	Kind        string `json:"kind"`
	Total       int    `json:"total"`
	Failed      int    `json:"failed"`
	Retried     int    `json:"retried"`
	Interrupted int    `json:"interrupted"`
	Canceled    int    `json:"canceled"`
}

// JobIssue is one job that failed, was retried, interrupted or canceled.
type JobIssue struct {
	Ref      string `json:"ref"`
	Kind     string `json:"kind"`
	What     string `json:"what"` // failed | retried | interrupted | canceled
	Attempts int    `json:"attempts"`
	Error    string `json:"error,omitempty"`
	Session  string `json:"session,omitempty"`
}

// ToolErrors counts one tool's errors.
type ToolErrors struct {
	Tool    string   `json:"tool"`
	Count   int      `json:"count"`
	Samples []Sample `json:"samples,omitempty"`
}

// Sample is an example with where it came from.
type Sample struct {
	Ref  string `json:"ref"`
	Text string `json:"text"`
}

// Approvals counts what happened to calls that needed approval, as the
// transcripts show it. A human's yes leaves no mark there, so it is not
// counted.
type Approvals struct {
	ReviewerApproved int      `json:"reviewer_approved"`
	ReviewerDenied   int      `json:"reviewer_denied"`
	ReviewerUnsure   int      `json:"reviewer_unsure"` // and no human to ask
	UserDeclined     int      `json:"user_declined"`   // a no, or no answer in time
	NoApprover       int      `json:"no_approver"`     // nobody attending the run
	GuestDenied      int      `json:"guest_denied"`
	Samples          []Sample `json:"samples,omitempty"`
}

// Fallback is answers from a model the agent did not mean to use, or a
// fallback or credential cool-down the log recorded.
type Fallback struct {
	From   string   `json:"from,omitempty"`
	To     string   `json:"to"`
	Reason string   `json:"reason,omitempty"`
	Count  int      `json:"count"`
	Refs   []string `json:"refs,omitempty"`
}

// Cost is the period's spending on one model.
type Cost struct {
	Model    string  `json:"model"`
	Calls    int     `json:"calls"`
	Input    int64   `json:"input_tokens"`
	Output   int64   `json:"output_tokens"`
	USD      float64 `json:"usd"`
	Unpriced int     `json:"unpriced,omitempty"`
}

// ScheduleIssue is a schedule whose runs had trouble.
type ScheduleIssue struct {
	Ref         string   `json:"ref"`
	Name        string   `json:"name"`
	Spec        string   `json:"spec"`
	Enabled     bool     `json:"enabled"`
	LastStatus  string   `json:"last_status,omitempty"`
	FailStreak  int      `json:"fail_streak"`
	Runs        int      `json:"runs"`
	Errors      int      `json:"errors"`
	BudgetSkips int      `json:"budget_skips"`
	TurnsFailed int      `json:"turns_failed"`
	Denied      int      `json:"approvals_denied"`
	Refs        []string `json:"refs,omitempty"`
}

// Stalled is a board card or delegated task with no progress for days.
type Stalled struct {
	Ref    string `json:"ref"`
	What   string `json:"what"` // card | task
	Title  string `json:"title"`
	Status string `json:"status"`
	Days   int    `json:"days"`
}

// Delivery is a reply or notice that failed to reach its chat or kept
// being retried.
type Delivery struct {
	Ref      string `json:"ref"`
	Target   string `json:"target"`
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error,omitempty"`
}

// Correction is a sign the user was not happy with an answer.
type Correction struct {
	Ref  string `json:"ref"`
	Kind string `json:"kind"` // correction | repeat | undo | undo-memory
	Text string `json:"text"`
}

// Change is one change to the agent's memory or skills.
type Change struct {
	Ref  string `json:"ref"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	Op   string `json:"op"`
	By   string `json:"by"`
}

// Refs is every evidence id the evidence holds, for checking a plan's
// citations.
func (e *Evidence) Refs() map[string]bool {
	out := map[string]bool{}
	add := func(refs ...string) {
		for _, r := range refs {
			if r != "" {
				out[r] = true
			}
		}
	}
	for _, s := range e.Turns.Slow {
		add(s.Ref)
		if s.Session != "" {
			add("session:" + s.Session)
		}
	}
	for _, p := range e.Problems {
		add(p.Ref)
		if p.Session != "" {
			add("session:" + p.Session)
		}
	}
	for _, t := range e.ToolErrors {
		for _, s := range t.Samples {
			add(s.Ref)
		}
	}
	for _, s := range e.Approvals.Samples {
		add(s.Ref)
	}
	for _, f := range e.Fallbacks {
		add(f.Refs...)
	}
	for _, s := range e.Schedules {
		add(s.Ref)
		add(s.Refs...)
	}
	for _, s := range e.Stalled {
		add(s.Ref)
	}
	for _, d := range e.Deliveries {
		add(d.Ref)
	}
	for _, c := range e.Corrections {
		add(c.Ref)
	}
	for _, c := range e.Changes {
		add(c.Ref)
	}
	return out
}

// Gather reads the evidence for one agent and period.
func Gather(ctx context.Context, src Sources, ag Agent, from, to time.Time) (*Evidence, error) {
	g := &gatherer{src: src, ag: ag, from: from, to: to,
		ev: &Evidence{Agent: ag.Name, From: from, To: to}, turnStatus: map[string]string{}, denied: map[string]int{}}
	if g.src.Redact == nil {
		g.src.Redact = logs.RedactPatterns
	}
	if g.src.StallDays <= 0 {
		g.src.StallDays = defaultStall
	}
	steps := []func(context.Context) error{g.jobs, g.transcripts, g.schedules, g.board, g.tasks, g.deliveries, g.cost, g.logFallbacks, g.changes, g.undos}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return nil, err
		}
	}
	return g.ev, nil
}

type gatherer struct {
	src      Sources
	ag       Agent
	from, to time.Time
	ev       *Evidence
	// turnStatus is each turn job's status, so a schedule's runs can be
	// matched to the turns they started.
	turnStatus map[string]string
	// denied counts approvals nobody could give, by turn job.
	denied map[string]int
}

func (g *gatherer) cut(s string, n int) string {
	return cutText(strings.Join(strings.Fields(g.src.Redact(s)), " "), n)
}

func (g *gatherer) span() (int64, int64) { return g.from.UnixMilli(), g.to.UnixMilli() }

// jobs counts the agent's jobs by kind, lists the ones that went wrong,
// and works out turn latency.
func (g *gatherer) jobs(ctx context.Context) error {
	if g.src.Control == nil {
		return nil
	}
	from, to := g.span()
	rows, err := g.src.Control.QueryContext(ctx, `SELECT id, kind, status, attempts, last_error, created_at, updated_at,
		COALESCE(json_extract(payload, '$.session'), ''), COALESCE(json_extract(payload, '$.text'), ''),
		COALESCE(json_extract(payload, '$.compact'), 0)
		FROM jobs WHERE agent = ? AND kind != 'retro' AND updated_at >= ? AND created_at < ? ORDER BY created_at LIMIT ?`,
		g.ag.Name, from, to, maxRows)
	if err != nil {
		return fmt.Errorf("evidence: jobs: %w", err)
	}
	defer rows.Close()
	counts := map[string]*JobCount{}
	var issues []JobIssue
	type turn struct {
		id, session, first string
		ms                 int64
	}
	var done []turn
	for rows.Next() {
		var id, kind, status, lastErr, session, text string
		var attempts int
		var created, updated int64
		var compact bool
		if err := rows.Scan(&id, &kind, &status, &attempts, &lastErr, &created, &updated, &session, &text, &compact); err != nil {
			return err
		}
		c := counts[kind]
		if c == nil {
			c = &JobCount{Kind: kind}
			counts[kind] = c
		}
		c.Total++
		what := ""
		switch {
		case status == "failed":
			c.Failed++
			what = "failed"
		case status == "canceled":
			c.Canceled++
			what = "canceled"
		case attempts > 1 && (lastErr == "" || strings.Contains(lastErr, "lease expired")):
			c.Interrupted++
			what = "interrupted"
		case attempts > 1:
			c.Retried++
			what = "retried"
		}
		if what != "" {
			issues = append(issues, JobIssue{Ref: "job:" + id, Kind: kind, What: what, Attempts: attempts,
				Error: g.cut(lastErr, maxSampleText), Session: session})
		}
		if kind != "turn" && kind != "subturn" {
			continue
		}
		g.turnStatus[id] = status
		if compact || created < from {
			continue
		}
		g.ev.Turns.Count++
		switch status {
		case "done":
			g.ev.Turns.Done++
			done = append(done, turn{id: id, session: session, first: text, ms: updated - created})
		case "failed":
			g.ev.Turns.Failed++
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, k := range sortedKeys(counts) {
		g.ev.Jobs = append(g.ev.Jobs, *counts[k])
	}
	order := map[string]int{"failed": 0, "interrupted": 1, "retried": 2, "canceled": 3}
	slices.SortStableFunc(issues, func(a, b JobIssue) int { return order[a.What] - order[b.What] })
	g.ev.Problems = issues[:min(len(issues), maxIssues)]

	if len(done) > 0 {
		ms := make([]int64, len(done))
		for i, t := range done {
			ms[i] = t.ms
		}
		slices.Sort(ms)
		g.ev.Turns.P50MS = ms[len(ms)/2]
		g.ev.Turns.P95MS = ms[min(len(ms)-1, len(ms)*95/100)]
		slices.SortStableFunc(done, func(a, b turn) int { return cmp.Compare(b.ms, a.ms) })
		for _, t := range done[:min(len(done), maxSlowest)] {
			if t.ms <= 0 {
				break
			}
			g.ev.Turns.Slow = append(g.ev.Turns.Slow, SlowTurn{Ref: "job:" + t.id, Session: t.session, MS: t.ms,
				First: g.cut(firstLine(t.first), 160)})
		}
	}
	return nil
}

// Phrases users write when an answer missed. Only short messages count: a
// long one is usually a new request.
var reCorrection = regexp.MustCompile(`(?i)^(no|nope|nah)\b|\bwrong\b|\bnot what i (asked|meant|wanted|said)\b|\bthat['’]?s not (it|right|what)\b|\bthat is not (it|right|what)\b|\byou (didn['’]?t|did not|forgot|missed|ignored)\b|\bi (already )?(said|told you|asked you)\b|\b(undo|revert) (that|it|this)\b|\b(incorrect|not correct)\b|\btry again\b`)

// transcripts reads the agent's messages in the period: sessions, tool
// errors, approvals, answers from fallback models and user corrections.
func (g *gatherer) transcripts(ctx context.Context) error {
	if g.src.Transcripts == nil {
		return nil
	}
	from, to := g.span()
	rows, err := g.src.Transcripts.QueryContext(ctx, `SELECT m.session_id, m.seq, m.role, m.blocks, m.model, m.job_id
		FROM messages m JOIN sessions s ON s.id = m.session_id
		WHERE s.agent = ? AND m.created_at >= ? AND m.created_at < ? ORDER BY m.session_id, m.seq LIMIT ?`,
		g.ag.Name, from, to, maxRows)
	if err != nil {
		return fmt.Errorf("evidence: transcripts: %w", err)
	}
	defer rows.Close()
	sessions := map[string]bool{}
	tools := map[string]*ToolErrors{}
	fallbacks := map[string]*Fallback{}
	names := map[string]string{} // tool_use id -> tool, per session
	var recent []string          // the session's last user messages, normalized
	var current string
	ap := &g.ev.Approvals
	for rows.Next() {
		var session, role, blocks, model, job string
		var seq int
		if err := rows.Scan(&session, &seq, &role, &blocks, &model, &job); err != nil {
			return err
		}
		if session != current {
			current, recent = session, nil
			clear(names)
		}
		sessions[session] = true
		ref := "session:" + session
		var bs []llm.Block
		if json.Unmarshal([]byte(blocks), &bs) != nil {
			continue
		}
		if role == string(llm.Assistant) && model != "" && len(g.ag.Models) > 0 && !slices.Contains(g.ag.Models, model) {
			f := fallbacks[model]
			if f == nil {
				f = &Fallback{To: model, Reason: "answered by a model other than the agent's own: a fallback, or a /model choice"}
				if len(g.ag.Models) > 0 {
					f.From = g.ag.Models[0]
				}
				fallbacks[model] = f
			}
			f.Count++
			if len(f.Refs) < maxSamples && !slices.Contains(f.Refs, ref) {
				f.Refs = append(f.Refs, ref)
			}
		}
		for _, b := range bs {
			switch b.Type {
			case llm.ToolUse:
				names[b.ToolUseID] = b.ToolName
			case llm.ToolResult:
				if strings.Contains(b.Text, "[approved by the reviewer:") {
					ap.ReviewerApproved++
				}
				if !b.IsError {
					continue
				}
				if g.approval(b.Text, ref, job) {
					continue
				}
				tool := names[b.ToolUseID]
				if tool == "" {
					tool = "unknown"
				}
				t := tools[tool]
				if t == nil {
					t = &ToolErrors{Tool: tool}
					tools[tool] = t
				}
				t.Count++
				if len(t.Samples) < maxSamples {
					t.Samples = append(t.Samples, Sample{Ref: ref, Text: g.cut(b.Text, maxSampleText)})
				}
			case llm.Text:
				if role != string(llm.User) {
					continue
				}
				text := strings.TrimSpace(b.Text)
				if text == "" || strings.HasPrefix(text, "[") {
					continue // notices, scheduled task headers
				}
				norm := normalize(text)
				switch {
				case repeated(norm, recent):
					g.correction(Correction{Ref: ref, Kind: "repeat", Text: g.cut(text, maxSampleText)})
				case len(text) <= maxCorrectText && reCorrection.MatchString(text) && !politeNo(text):
					g.correction(Correction{Ref: ref, Kind: "correction", Text: g.cut(text, maxSampleText)})
				}
				recent = append(recent, norm)
				if len(recent) > 3 {
					recent = recent[1:]
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	g.ev.Sessions = len(sessions)
	list := make([]*ToolErrors, 0, len(tools))
	for _, t := range tools {
		list = append(list, t)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Count != list[j].Count {
			return list[i].Count > list[j].Count
		}
		return list[i].Tool < list[j].Tool
	})
	for _, t := range list[:min(len(list), maxListed)] {
		g.ev.ToolErrors = append(g.ev.ToolErrors, *t)
	}
	for _, k := range sortedKeys(fallbacks) {
		g.ev.Fallbacks = append(g.ev.Fallbacks, *fallbacks[k])
	}
	return nil
}

// approval counts a tool result that is an approval refusal, and reports
// whether it was one.
func (g *gatherer) approval(text, ref, job string) bool {
	if !strings.Contains(text, "denied by permission policy") {
		return false
	}
	ap := &g.ev.Approvals
	switch {
	case strings.Contains(text, "the reviewer denied"):
		ap.ReviewerDenied++
	case strings.Contains(text, "the reviewer would not approve"):
		ap.ReviewerUnsure++
	case strings.Contains(text, "the user declined"):
		ap.UserDeclined++
	case strings.Contains(text, "no one is attending this run"):
		ap.NoApprover++
		g.denied[job]++
	case strings.Contains(text, "other than the owner"):
		ap.GuestDenied++
	default:
		return false // a mode or policy refusal: counted with tool errors
	}
	if len(ap.Samples) < 2*maxSamples {
		ap.Samples = append(ap.Samples, Sample{Ref: ref, Text: g.cut(text, maxSampleText)})
	}
	return true
}

func (g *gatherer) correction(c Correction) {
	if len(g.ev.Corrections) < maxListed {
		g.ev.Corrections = append(g.ev.Corrections, c)
	}
}

// politeNo drops "no thanks" and "no problem", which are not corrections.
func politeNo(text string) bool {
	l := strings.ToLower(text)
	for _, p := range []string{"no thank", "no problem", "no worries", "no rush", "no need to"} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

var reWord = regexp.MustCompile(`[\p{L}\p{N}]+`)

func normalize(s string) string {
	return strings.Join(reWord.FindAllString(strings.ToLower(s), -1), " ")
}

// repeated reports whether a user message says again what one of the
// last few said: the same words, or nearly (four words or more).
func repeated(norm string, recent []string) bool {
	if norm == "" {
		return false
	}
	words := strings.Fields(norm)
	for _, r := range recent {
		if r == norm && len(words) >= 2 {
			return true
		}
		if len(words) >= 4 && jaccard(words, strings.Fields(r)) >= 0.8 {
			return true
		}
	}
	return false
}

func jaccard(a, b []string) float64 {
	set := map[string]int{}
	for _, w := range a {
		set[w] |= 1
	}
	for _, w := range b {
		set[w] |= 2
	}
	inter := 0
	for _, v := range set {
		if v == 3 {
			inter++
		}
	}
	if len(set) == 0 {
		return 0
	}
	return float64(inter) / float64(len(set))
}

// schedules finds the agent's schedules whose runs had trouble in the
// period: script errors, budget skips, failed turns, missed slots, and
// approvals nobody was there to give.
func (g *gatherer) schedules(ctx context.Context) error {
	if g.src.Control == nil {
		return nil
	}
	rows, err := g.src.Control.QueryContext(ctx, `SELECT id, name, spec, enabled, last_status, fail_streak
		FROM schedules WHERE agent = ? ORDER BY name, id`, g.ag.Name)
	if err != nil {
		return fmt.Errorf("evidence: schedules: %w", err)
	}
	byID := map[string]*ScheduleIssue{}
	var ids []string
	for rows.Next() {
		s := &ScheduleIssue{}
		if err := rows.Scan(&s.Ref, &s.Name, &s.Spec, &s.Enabled, &s.LastStatus, &s.FailStreak); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, s.Ref)
		byID[s.Ref] = s
		s.LastStatus = g.cut(s.LastStatus, maxSampleText)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	from, to := g.span()
	rows, err = g.src.Control.QueryContext(ctx, `SELECT id, status, result, COALESCE(json_extract(payload, '$.schedule'), '')
		FROM jobs WHERE kind = 'scheduled' AND agent = ? AND created_at >= ? AND created_at < ? LIMIT ?`,
		g.ag.Name, from, to, maxRows)
	if err != nil {
		return fmt.Errorf("evidence: scheduled runs: %w", err)
	}
	for rows.Next() {
		var id, status, result, sched string
		if err := rows.Scan(&id, &status, &result, &sched); err != nil {
			rows.Close()
			return err
		}
		s := byID[sched]
		if s == nil {
			continue // deleted since
		}
		s.Runs++
		bad := false
		switch {
		case status == "failed" || strings.HasPrefix(result, "error:"):
			s.Errors++
			bad = true
		case strings.HasPrefix(result, "skipped:"):
			s.BudgetSkips++
			bad = true
		}
		turn := "turn_" + id
		if g.turnStatus[turn] == "failed" {
			s.TurnsFailed++
			bad = true
		}
		if n := g.denied[turn]; n > 0 {
			s.Denied += n
			bad = true
		}
		if bad && len(s.Refs) < maxSamples {
			s.Refs = append(s.Refs, "job:"+id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		s := byID[id]
		trouble := s.FailStreak > 0 || s.Errors+s.BudgetSkips+s.TurnsFailed+s.Denied > 0 ||
			strings.HasPrefix(s.LastStatus, "missed") || strings.HasPrefix(s.LastStatus, "error")
		switch {
		case trouble && len(g.ev.Schedules) < maxListed:
			s.Ref = "schedule:" + s.Ref
			g.ev.Schedules = append(g.ev.Schedules, *s)
		case !trouble && s.Runs > 0:
			g.ev.SchedulesOK++
		}
	}
	return nil
}

// board finds the agent's open cards with no progress for StallDays, and
// counts the cards it finished.
func (g *gatherer) board(ctx context.Context) error {
	if g.src.Control == nil {
		return nil
	}
	from, to := g.span()
	if err := g.src.Control.QueryRowContext(ctx, `SELECT count(*) FROM cards WHERE assignee = ? AND status = 'done'
		AND updated_at >= ? AND updated_at < ?`, g.ag.Name, from, to).Scan(&g.ev.CardsDone); err != nil {
		return fmt.Errorf("evidence: board: %w", err)
	}
	stall := g.to.AddDate(0, 0, -g.src.StallDays).UnixMilli()
	rows, err := g.src.Control.QueryContext(ctx, `SELECT c.id, c.title, c.status, max(c.updated_at,
			COALESCE((SELECT max(at) FROM card_events e WHERE e.card = c.id), 0),
			COALESCE((SELECT max(at) FROM card_comments m WHERE m.card = c.id), 0)) AS last
		FROM cards c WHERE (c.assignee = ? OR c.created_by = ?) AND c.status IN ('todo','doing','blocked')
		AND c.created_at < ? ORDER BY last LIMIT ?`, g.ag.Name, g.ag.Name, to, maxRows)
	if err != nil {
		return fmt.Errorf("evidence: board: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, title, status string
		var last int64
		if err := rows.Scan(&id, &title, &status, &last); err != nil {
			return err
		}
		if last >= stall || len(g.ev.Stalled) >= maxListed {
			continue
		}
		g.ev.Stalled = append(g.ev.Stalled, Stalled{Ref: "card:" + id, What: "card", Title: g.cut(title, 120),
			Status: status, Days: int(g.to.Sub(time.UnixMilli(last)) / (24 * time.Hour))})
	}
	return rows.Err()
}

// tasks finds delegated tasks (sub-agent jobs) the agent was given or
// gave that are still open after StallDays.
func (g *gatherer) tasks(ctx context.Context) error {
	if g.src.Control == nil {
		return nil
	}
	stall := g.to.AddDate(0, 0, -g.src.StallDays).UnixMilli()
	rows, err := g.src.Control.QueryContext(ctx, `SELECT id, agent, status, created_at,
		COALESCE(json_extract(payload, '$.sub.task'), ''), COALESCE(json_extract(payload, '$.sub.parent'), '')
		FROM jobs WHERE kind = 'subturn' AND status IN ('pending','running') AND created_at < ? ORDER BY created_at LIMIT ?`,
		stall, maxRows)
	if err != nil {
		return fmt.Errorf("evidence: tasks: %w", err)
	}
	type task struct {
		id, agent, status, title, parent string
		created                          int64
	}
	var open []task
	for rows.Next() {
		var t task
		if err := rows.Scan(&t.id, &t.agent, &t.status, &t.created, &t.title, &t.parent); err != nil {
			rows.Close()
			return err
		}
		open = append(open, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range open {
		mine := t.agent == g.ag.Name
		if !mine && t.parent != "" && g.src.Transcripts != nil {
			var owner string
			if g.src.Transcripts.QueryRowContext(ctx, `SELECT agent FROM sessions WHERE id = ?`, t.parent).Scan(&owner) == nil {
				mine = owner == g.ag.Name
			}
		}
		if !mine || len(g.ev.Stalled) >= maxListed {
			continue
		}
		title := t.title
		if title == "" {
			title = "delegated task"
		}
		g.ev.Stalled = append(g.ev.Stalled, Stalled{Ref: "job:" + t.id, What: "task", Title: g.cut(title, 120),
			Status: t.status, Days: int(g.to.Sub(time.UnixMilli(t.created)) / (24 * time.Hour))})
	}
	return nil
}

// deliveries finds replies and notices to the agent's chats that failed
// or kept being retried.
func (g *gatherer) deliveries(ctx context.Context) error {
	if g.src.Control == nil {
		return nil
	}
	from, to := g.span()
	rows, err := g.src.Control.QueryContext(ctx, `SELECT id, status, attempts, last_error,
		COALESCE(json_extract(payload, '$.to.account'), ''), COALESCE(json_extract(payload, '$.to.conversation'), ''),
		COALESCE(json_extract(payload, '$.to.thread'), '')
		FROM jobs WHERE kind = 'deliver' AND updated_at >= ? AND created_at < ?
		AND (status = 'failed' OR attempts > 1) ORDER BY attempts DESC, created_at LIMIT ?`, from, to, maxRows)
	if err != nil {
		return fmt.Errorf("evidence: deliveries: %w", err)
	}
	type dlv struct {
		Delivery
		account, conv, thread string
	}
	var list []dlv
	for rows.Next() {
		var d dlv
		var lastErr string
		if err := rows.Scan(&d.Ref, &d.Status, &d.Attempts, &lastErr, &d.account, &d.conv, &d.thread); err != nil {
			rows.Close()
			return err
		}
		d.Ref, d.Error = "job:"+d.Ref, g.cut(lastErr, maxSampleText)
		d.Target = d.account + ":" + d.conv
		if d.thread != "" {
			d.Target += ":" + d.thread
		}
		list = append(list, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	owner := map[string]string{}
	for _, d := range list {
		agent, ok := owner[d.Target]
		if !ok {
			// Conversations bound to the agent; ignore a lookup error and
			// fall back to the home chat check.
			_ = g.src.Control.QueryRowContext(ctx, `SELECT agent FROM conversations WHERE account = ? AND conversation = ? AND thread = ?`,
				d.account, d.conv, d.thread).Scan(&agent)
			owner[d.Target] = agent
		}
		if agent != g.ag.Name && d.Target != g.ag.Home {
			continue
		}
		if len(g.ev.Deliveries) < maxListed {
			g.ev.Deliveries = append(g.ev.Deliveries, d.Delivery)
		}
	}
	return nil
}

// cost totals the agent's model spending by model.
func (g *gatherer) cost(ctx context.Context) error {
	if g.src.Usage == nil {
		return nil
	}
	calls, err := g.src.Usage.Calls(ctx, usage.Filter{Since: g.from, Agent: g.ag.Name})
	if err != nil {
		return fmt.Errorf("evidence: usage: %w", err)
	}
	calls = slices.DeleteFunc(calls, func(c usage.Call) bool { return !c.At.Before(g.to) })
	pr := g.src.Pricer
	if pr == nil {
		pr = &usage.Pricer{}
	}
	s := usage.Summarize(calls, pr)
	for model, t := range s.ByModel {
		g.ev.Cost = append(g.ev.Cost, Cost{Model: model, Calls: t.Calls, Input: t.InputTokens + t.CacheReadTokens + t.CacheWriteTokens,
			Output: t.OutputTokens, USD: t.Cost, Unpriced: t.Unpriced})
	}
	slices.SortFunc(g.ev.Cost, func(a, b Cost) int {
		if c := cmp.Compare(b.USD, a.USD); c != 0 {
			return c
		}
		return cmp.Compare(a.Model, b.Model)
	})
	g.ev.Cost = g.ev.Cost[:min(len(g.ev.Cost), maxListed)]
	return nil
}

// logFallbacks reads model fallbacks and credential cool-downs, with
// their reasons (auth, rate limit, ...), from habakkuk.log. The log does
// not say which agent a call was for, so it keeps those involving the
// agent's models and providers.
func (g *gatherer) logFallbacks(ctx context.Context) error {
	if g.src.LogHome == "" {
		return nil
	}
	mine := map[string]bool{}
	endpoints := map[string]bool{}
	for _, m := range slices.Concat(g.ag.Models, g.ag.Fallbacks) {
		mine[m] = true
		if p, _, ok := strings.Cut(m, "/"); ok {
			endpoints[p] = true
		}
	}
	found := map[string]*Fallback{}
	err := logs.Scan(g.src.LogHome, func(r logs.Record) {
		if r.Time.Before(g.from) || !r.Time.Before(g.to) {
			return
		}
		var key string
		var f Fallback
		switch r.Msg {
		case "falling back":
			f = Fallback{From: attr(r, "from"), To: attr(r, "to"), Reason: attr(r, "reason")}
			if !mine[f.From] {
				return
			}
			key = f.From + ">" + f.To + ":" + f.Reason
		case "credential cooling down":
			ep := attr(r, "endpoint")
			if !endpoints[ep] {
				return
			}
			f = Fallback{From: ep, To: "credential " + attr(r, "credential") + " cooling down", Reason: attr(r, "reason")}
			key = ep + ":" + f.To + ":" + f.Reason
		default:
			return
		}
		cur := found[key]
		if cur == nil {
			cur = &f
			found[key] = cur
		}
		cur.Count++
		if len(cur.Refs) < 2 {
			cur.Refs = append(cur.Refs, "log:"+r.Time.UTC().Format(time.RFC3339))
		}
	})
	if err != nil {
		return nil // the log is a bonus; the stores are the evidence
	}
	for _, k := range sortedKeys(found) {
		if len(g.ev.Fallbacks) < maxListed {
			g.ev.Fallbacks = append(g.ev.Fallbacks, *found[k])
		}
	}
	return ctx.Err()
}

func attr(r logs.Record, key string) string {
	for _, a := range r.Attrs {
		if a.Key == key {
			var s string
			if json.Unmarshal(a.Value, &s) == nil {
				return s
			}
			return string(a.Value)
		}
	}
	return ""
}

// changes lists changes to the agent's memory and skills, and counts
// /undo-memory uses as corrections.
func (g *gatherer) changes(ctx context.Context) error {
	if g.src.History == nil {
		return nil
	}
	cs, err := g.src.History.List(ctx, history.Filter{Agent: g.ag.Name, Since: g.from, Limit: 1000})
	if err != nil {
		return fmt.Errorf("evidence: memory history: %w", err)
	}
	for _, c := range cs {
		if !c.At.Before(g.to) {
			continue
		}
		ref := "change:" + c.ID
		if c.Tool == "/undo-memory" {
			g.correction(Correction{Ref: ref, Kind: "undo-memory", Text: g.cut(c.Describe(), maxSampleText)})
		}
		if len(g.ev.Changes) < maxChanges {
			by := c.By
			if c.Tool != "" && c.Tool != c.By {
				by += " via " + c.Tool
			}
			g.ev.Changes = append(g.ev.Changes, Change{Ref: ref, Kind: c.Kind, Name: c.Name, Op: c.Op(), By: by})
		}
	}
	return nil
}

// undos counts the user putting back files a turn changed.
func (g *gatherer) undos(ctx context.Context) error {
	if g.src.Undos == nil {
		return nil
	}
	list, err := g.src.Undos(ctx, g.ag.Name)
	if err != nil {
		return nil // checkpoints need git; without it there is nothing to count
	}
	for _, u := range list {
		if u.At.Before(g.from) || !u.At.Before(g.to) {
			continue
		}
		text := "undid the file changes of: " + u.Task
		if u.Session != "" {
			text += " (session " + u.Session + ")"
		}
		g.correction(Correction{Ref: "checkpoint:" + u.ID, Kind: "undo", Text: g.cut(text, maxSampleText)})
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" && (!strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]")) {
			return line
		}
	}
	return strings.TrimSpace(s)
}

// cutText shortens s to about n bytes on a character boundary.
func cutText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
