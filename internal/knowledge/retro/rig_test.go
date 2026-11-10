package retro

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/skills"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/usage"
	"github.com/habakkuk-ai/habakkuk/internal/work/board"
	"github.com/habakkuk-ai/habakkuk/internal/work/schedule"
)

// fakeModel answers with a canned plan and records what it was asked.
type fakeModel struct {
	mu     sync.Mutex
	answer string
	inputs []string
	fail   bool
}

func (m *fakeModel) Send(_ context.Context, chain []llm.Target, req llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return nil, fmt.Errorf("the model must not be called")
	}
	if len(chain) == 0 || chain[0].Model != "main" {
		return nil, fmt.Errorf("wrong chain %v", chain)
	}
	if req.System != systemPrompt {
		return nil, fmt.Errorf("wrong system prompt")
	}
	m.inputs = append(m.inputs, req.Messages[0].Text())
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: m.answer}}}}, nil
}

func (m *fakeModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.inputs)
}

type sent struct{ id, target, text string }

type rig struct {
	home        string
	control     *sql.DB
	transcripts *sql.DB
	queue       *jobs.Queue
	sessions    *session.Store
	usage       *usage.Store
	hist        *history.Log
	mem         *memory.Store
	skills      *skills.Library
	scheds      *schedule.Store
	model       *fakeModel
	r           *Runner
	now         time.Time
	from, to    time.Time

	mu        sync.Mutex
	delivered []sent
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	rg := &rig{home: t.TempDir(), model: &fakeModel{}, now: time.Now()}
	rg.from, rg.to = rg.now.Add(-7*24*time.Hour), rg.now.Add(time.Hour)
	migs := slices.Concat(jobs.Migrations, channel.Migrations, schedule.Migrations, board.Migrations)
	slices.SortStableFunc(migs, func(a, b store.Migration) int { return a.Version - b.Version })
	var err error
	if rg.control, err = store.Open(ctx, filepath.Join(rg.home, "control.db"), migs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rg.control.Close() })
	if rg.transcripts, err = store.Open(ctx, filepath.Join(rg.home, "transcripts.db"), session.Migrations); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rg.transcripts.Close() })
	if rg.usage, err = usage.Open(ctx, filepath.Join(rg.home, "usage.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rg.usage.Close() })
	rg.queue, rg.sessions = jobs.NewQueue(rg.control), session.NewStore(rg.transcripts)
	rg.scheds = schedule.NewStore(rg.control, time.UTC)
	rg.open(t)
	return rg
}

// open (re)opens memory.db and history.db and builds a fresh runner, as a
// restarted process would.
func (rg *rig) open(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if rg.mem != nil {
		rg.mem.Close()
		rg.hist.Close()
	}
	var err error
	if rg.mem, err = memory.Open(ctx, rg.home); err != nil {
		t.Fatal(err)
	}
	if rg.hist, err = history.Open(ctx, rg.home); err != nil {
		t.Fatal(err)
	}
	mem, hist := rg.mem, rg.hist
	t.Cleanup(func() { mem.Close(); hist.Close() })
	rg.mem.UseHistory(rg.hist)
	rg.skills = &skills.Library{Home: rg.home, History: rg.hist}
	rg.r = &Runner{
		Home: rg.home, Mem: rg.mem, Skills: rg.skills, Queue: rg.queue, Models: rg.model,
		Sources: Sources{Control: rg.control, Transcripts: rg.transcripts, Usage: rg.usage, History: rg.hist, LogHome: rg.home},
		Agents: map[string]Agent{"habakkuk": {
			Name: "habakkuk", Chain: []llm.Target{{Endpoint: "p", Model: "main"}},
			Models: []string{"p/main"}, Fallbacks: []string{"p/backup"}, Home: "tg:home", Deliver: "tg:home",
			Dir: filepath.Join(rg.home, "agents", "habakkuk"),
		}},
		Apply: ApplyPropose, Schedules: rg.scheds, Location: time.UTC,
		Deliver: func(_ context.Context, id, target, text string) error {
			rg.mu.Lock()
			defer rg.mu.Unlock()
			for _, d := range rg.delivered {
				if d.id == id {
					return nil
				}
			}
			rg.delivered = append(rg.delivered, sent{id, target, text})
			return nil
		},
		Now: func() time.Time { return rg.now },
	}
}

func (rg *rig) exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// job inserts a finished job with the given timestamps.
func (rg *rig) job(t *testing.T, id, kind, agent, status string, attempts int, lastErr, result string, payload any, created, updated time.Time) {
	t.Helper()
	data, _ := json.Marshal(payload)
	rg.exec(t, rg.control, `INSERT INTO jobs (id, kind, agent, payload, status, attempts, max_attempts, run_at, last_error, result, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 20, ?, ?, ?, ?, ?)`, id, kind, agent, string(data), status, attempts, created.UnixMilli(),
		lastErr, result, created.UnixMilli(), updated.UnixMilli())
}

// conversation writes a session with messages.
func (rg *rig) conversation(t *testing.T, job string, msgs ...llm.Message) string {
	t.Helper()
	ctx := context.Background()
	sess, err := rg.sessions.Create(ctx, "habakkuk", "system")
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range msgs {
		model := ""
		if m.Role == llm.Assistant {
			model = "p/main"
		}
		if m.Role == llm.Assistant && len(m.Blocks) > 0 && m.Blocks[0].Name != "" {
			model = m.Blocks[0].Name // tests mark a fallback answer this way
			m.Blocks[0].Name = ""
		}
		if err := rg.sessions.Append(ctx, sess.ID, job, i, m, model, nil); err != nil {
			t.Fatal(err)
		}
	}
	return sess.ID
}

func user(text string) llm.Message { return llm.UserText(text) }

func assistant(text string) llm.Message {
	return llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: text}}}
}

func callTool(id, name string) llm.Message {
	return llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.ToolUse, ToolUseID: id, ToolName: name, Input: json.RawMessage(`{}`)}}}
}

func toolResult(id, text string, isErr bool) llm.Message {
	return llm.Message{Role: llm.User, Blocks: []llm.Block{{Type: llm.ToolResult, ToolUseID: id, Text: text, IsError: isErr}}}
}

// run claims the retro job and runs it.
func (rg *rig) run(t *testing.T) (*jobs.Job, string, error) {
	t.Helper()
	j, err := rg.queue.Claim(context.Background(), "worker:test:1:x", time.Minute, Kind)
	if err != nil || j == nil {
		t.Fatalf("claim: %v %v", j, err)
	}
	out, err := rg.r.Handle(context.Background(), j, "worker:test:1:x")
	if err == nil {
		if err := rg.queue.Complete(context.Background(), j.ID, "worker:test:1:x", out); err != nil {
			t.Fatal(err)
		}
	}
	return j, out, err
}

// rerun runs a job again after a simulated crash: its lease is taken over.
func (rg *rig) rerun(t *testing.T, j *jobs.Job) (string, error) {
	t.Helper()
	rg.exec(t, rg.control, `UPDATE jobs SET status = 'running', lease_owner = 'worker:test:2:y', lease_until = ? WHERE id = ?`,
		time.Now().Add(time.Minute).UnixMilli(), j.ID)
	return rg.r.Handle(context.Background(), j, "worker:test:2:y")
}

func (rg *rig) changes(t *testing.T) []history.Change {
	t.Helper()
	cs, err := rg.hist.List(context.Background(), history.Filter{Agent: "habakkuk", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return cs
}
