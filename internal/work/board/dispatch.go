package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
)

// KindCard is the job that runs one attempt at a card.
const KindCard = "card"

// Dispatcher turns ready cards into jobs, runs them, and tells the card's
// creator how they ended.
//
// A card is dispatched by a compare-and-swap that moves it from todo to
// doing and records the job ID of the next attempt, derived from the card
// ID and the attempt number. Only one process can win that swap, and the ID
// makes enqueueing idempotent, so each attempt runs at most once however
// many processes poll. A crash between the swap and the enqueue is repaired
// on the next tick, which enqueues the job for any doing card that lacks
// one.
type Dispatcher struct {
	Store  *Store
	Queue  *jobs.Queue
	Runner *agent.Runner
	Log    *slog.Logger
	// Deliver queues text for a chat reply address under a fixed ID. It is
	// used for cards whose creator has no session to tell (the CLI); nil
	// drops those notices.
	Deliver func(ctx context.Context, id string, to json.RawMessage, text string) error
	Every   time.Duration // poll interval; default 5s

	wake chan struct{}
	once sync.Once
}

type payload struct {
	Card    string `json:"card"`
	Attempt int    `json:"attempt"`
}

func (d *Dispatcher) init() {
	d.once.Do(func() { d.wake = make(chan struct{}, 1) })
}

// Wake makes a running dispatcher look at the board now rather than at the
// next tick. It never blocks, and does nothing if the loop is not running.
func (d *Dispatcher) Wake() {
	d.init()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run polls the board until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) {
	d.init()
	every := d.Every
	if every == 0 {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := d.Tick(ctx); err != nil && ctx.Err() == nil {
			d.Log.Error("board tick failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.wake:
		}
	}
}

func jobID(card string, attempt int) string {
	return fmt.Sprintf("card_%s_%d", strings.TrimPrefix(card, "crd_"), attempt)
}

// Tick dispatches every ready card, repairs attempts a crash left behind,
// and sends owed notifications. It returns how many cards it dispatched.
func (d *Dispatcher) Tick(ctx context.Context) (int, error) {
	ready, err := d.Store.Ready(ctx)
	if err != nil {
		return 0, err
	}
	started := 0
	for _, c := range ready {
		won, err := d.Store.start(ctx, &c)
		if err != nil {
			return started, err
		}
		if !won {
			continue // another process took this attempt, or the card changed
		}
		if err := d.enqueue(ctx, &c); err != nil {
			return started, err
		}
		started++
	}
	if err := d.repair(ctx); err != nil {
		return started, err
	}
	if err := d.notifyAll(ctx); err != nil {
		return started, err
	}
	if started > 0 && d.Runner.Wake != nil {
		d.Runner.Wake()
	}
	return started, nil
}

func (d *Dispatcher) enqueue(ctx context.Context, c *Card) error {
	_, err := d.Queue.Enqueue(ctx, jobs.NewJob{
		ID: c.Job, Kind: KindCard, Agent: c.Assignee, Priority: 5,
		Payload: payload{Card: c.ID, Attempt: c.Attempt},
	})
	return err
}

// start claims the card's next attempt: todo -> doing, if nobody changed
// the card since it was read. On success c holds the claimed attempt.
func (s *Store) start(ctx context.Context, c *Card) (bool, error) {
	won := false
	next, err := s.mutate(ctx, c.ID, func(cur *Card, ch *change) error {
		if cur.Status != Todo || cur.Attempt != c.Attempt || cur.Assignee != c.Assignee {
			return errNoChange
		}
		won = true
		cur.Status, cur.Attempt, cur.Session = Doing, cur.Attempt+1, ""
		cur.Job = jobID(cur.ID, cur.Attempt)
		ch.event("board", "dispatched", fmt.Sprintf("attempt %d to %s", cur.Attempt, cur.Assignee))
		return nil
	})
	if err != nil || !won {
		return false, err
	}
	*c = *next
	return true, nil
}

// repair looks at cards in progress: a missing job (crash right after the
// claim) is enqueued, and a job that failed for good blocks its card.
func (d *Dispatcher) repair(ctx context.Context) error {
	doing, err := d.Store.List(ctx, Filter{Status: string(Doing), Limit: 1000})
	if err != nil {
		return err
	}
	for _, c := range doing {
		j, err := d.Queue.Get(ctx, c.Job)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			d.Log.Warn("re-enqueueing a card attempt lost in a crash", "card", c.ID, "job", c.Job)
			if err := d.enqueue(ctx, &c); err != nil {
				return err
			}
			if d.Runner.Wake != nil {
				d.Runner.Wake()
			}
		case err != nil:
			return err
		case j.Status == jobs.Failed || j.Status == jobs.Canceled:
			why := "the run was " + string(j.Status)
			if j.LastError != "" {
				why += ": " + j.LastError
			}
			if err := d.end(ctx, c.ID, c.Job, "board", "The run stopped before the card was finished ("+why+")."); err != nil {
				return err
			}
		}
	}
	return nil
}

// Handle is the jobs.Handler for KindCard. It runs the attempt as a turn in
// a fresh session for the assignee. The turn is the card job itself, so a
// crash anywhere resumes the same transcript. If the turn ends with the card
// still in progress, the card is blocked with the agent's last reply.
func (d *Dispatcher) Handle(ctx context.Context, j *jobs.Job, owner string) (string, error) {
	var p payload
	if err := j.Decode(&p); err != nil {
		return "", jobs.Permanent{Err: err}
	}
	c, err := d.Store.Get(ctx, p.Card)
	if err != nil {
		return "", jobs.Permanent{Err: err}
	}
	if c.Status != Doing || c.Job != j.ID {
		return "card is " + string(c.Status) + "; this attempt has nothing to do", nil
	}
	if _, ok := d.Runner.Agents[c.Assignee]; !ok {
		return "", d.end(ctx, c.ID, j.ID, "board", fmt.Sprintf("Agent %q is not configured.", c.Assignee))
	}
	if be := d.Runner.BudgetBlocked(c.Assignee); be != nil {
		return "skipped: " + be.Message, d.end(ctx, c.ID, j.ID, "board", be.Message)
	}
	if c.Session == "" {
		sess, err := d.Runner.NewSession(ctx, c.Assignee)
		if err != nil {
			return "", err
		}
		// A crash before this write leaves an empty session behind, nothing
		// worse; the rerun makes another.
		if c, err = d.Store.mutate(ctx, c.ID, func(cur *Card, ch *change) error {
			if cur.Status != Doing || cur.Job != j.ID || cur.Session != "" {
				return errNoChange
			}
			cur.Session = sess.ID
			ch.event(cur.Assignee, "started", "session "+sess.ID)
			return nil
		}); err != nil {
			return "", err
		}
		if c.Status != Doing || c.Job != j.ID {
			return "card moved on before the run started", nil
		}
		if c, err = d.Store.Get(ctx, c.ID); err != nil {
			return "", err
		}
	}
	text, tainted, err := d.prompt(ctx, c)
	if err != nil {
		return "", err
	}
	turn := *j
	if turn.Payload, err = json.Marshal(agent.TurnPayload{Session: c.Session, Text: text,
		Source: agent.SourceBoard, Task: fmt.Sprintf("card %s %q", c.ID, c.Title), Tainted: tainted}); err != nil {
		return "", jobs.Permanent{Err: err}
	}
	final, err := d.Runner.HandleTurn(ctx, &turn, owner)
	if d.Runner.Untrusted(turn.ID) {
		// Outside content steered the run: whatever it left on the card,
		// and the notice to whoever made it, carry the taint, even when it
		// ended without board_complete or board_block.
		if merr := d.Store.MarkTainted(ctx, c.ID); merr != nil {
			d.Log.Error("mark card tainted failed", "card", c.ID, "err", merr)
		}
	}
	if err != nil {
		if errors.Is(err, jobs.ErrLeaseLost) || ctx.Err() != nil {
			return "", err
		}
		if errors.As(err, new(jobs.Permanent)) || j.Attempts >= j.MaxAttempts {
			if berr := d.end(ctx, c.ID, j.ID, "board", "The run failed: "+err.Error()); berr != nil {
				d.Log.Error("block card failed", "card", c.ID, "err", berr)
			}
		}
		return "", err
	}
	reply := strings.TrimSpace(final)
	if reply == "" {
		reply = "(no reply)"
	}
	if err := d.end(ctx, c.ID, j.ID, c.Assignee, "The run ended without board_complete or board_block. Last reply:\n"+reply); err != nil {
		return "", err
	}
	return final, nil
}

// end blocks the card with a comment if the given attempt is still the
// card's current one and still in progress; otherwise it does nothing.
func (d *Dispatcher) end(ctx context.Context, card, job, actor, why string) error {
	c, err := d.Store.mutate(ctx, card, func(cur *Card, ch *change) error {
		if cur.Status != Doing || cur.Job != job {
			return errNoChange
		}
		ch.comment(actor, why)
		cur.Status = Blocked
		return nil
	})
	if err == nil && c.notify {
		d.Wake()
	}
	return err
}

// prompt is the assignee's first message for an attempt: the card, the
// results of what it depended on, and the discussion so far.
func (d *Dispatcher) prompt(ctx context.Context, c *Card) (text string, tainted bool, err error) {
	tainted = c.Tainted
	var b strings.Builder
	fmt.Fprintf(&b, "[Board card %s, assigned to you by %s. Do the work now; nobody is watching live. "+
		"When it is finished, call board_complete with the result and proof (file paths, links, command output). "+
		"If you cannot finish it, call board_block with the reason. Your final reply alone does not complete the card.]\n\n",
		c.ID, c.CreatedBy)
	fmt.Fprintf(&b, "Title: %s\nPriority: %d\n", c.Title, c.Priority)
	if c.Parent != "" {
		if parent, err := d.Store.Get(ctx, c.Parent); err == nil {
			fmt.Fprintf(&b, "Part of: %s %q\n", parent.ID, parent.Title)
		}
	}
	if c.Attempt > 1 {
		fmt.Fprintf(&b, "Attempt: %d (earlier attempts are summarised in the comments)\n", c.Attempt)
	}
	if body := strings.TrimSpace(c.Body); body != "" {
		b.WriteString("\n" + body + "\n")
	}
	if len(c.DependsOn) > 0 {
		b.WriteString("\nResults of the cards this one waited for:\n")
		for _, id := range c.DependsOn {
			dep, err := d.Store.Get(ctx, id)
			if err != nil {
				return "", false, err
			}
			tainted = tainted || dep.Tainted
			fmt.Fprintf(&b, "\n## %s %q (by %s)\n%s\n", dep.ID, dep.Title, orNone(dep.Assignee), dep.Result)
		}
	}
	if len(c.Comments) > 0 {
		b.WriteString("\nComments:\n")
		for _, cm := range c.Comments {
			fmt.Fprintf(&b, "- %s, %s: %s\n", cm.Author, cm.At.Format("2006-01-02 15:04"), cm.Text)
		}
	}
	return strings.TrimSpace(b.String()), tainted, nil
}

// notifyAll sends every notification the board owes.
func (d *Dispatcher) notifyAll(ctx context.Context) error {
	owed, err := d.Store.query(ctx, d.Store.db, `WHERE c.notify = 1 ORDER BY c.updated_at`)
	if err != nil {
		return err
	}
	for _, c := range owed {
		if err := d.notify(ctx, &c); err != nil {
			return err
		}
	}
	return nil
}

// notify tells whoever asked for a card that it is done or blocked. If an
// agent created it from a session, the news becomes a new turn there, so
// the creator can act on it (and, if that session is a chat, its reply goes
// to the chat). Otherwise a chat reply address gets the result directly.
// The job IDs derive from the card's revision, so a notice is sent once
// even if this runs again after a crash; the flag is cleared only if the
// card has not changed since.
func (d *Dispatcher) notify(ctx context.Context, c *Card) error {
	full, err := d.Store.Get(ctx, c.ID)
	if err != nil {
		return err
	}
	id := fmt.Sprintf("cardnote_%s_%d", strings.TrimPrefix(c.ID, "crd_"), c.Rev)
	what := "is done"
	detail := "Result:\n" + full.Result
	if c.Status == Blocked {
		what = "is blocked"
		detail = "Reason:\n" + lastComment(full)
	}
	sent := false
	if c.CreatorSession != "" && c.CreatedBy != User {
		if _, err := d.Runner.Sessions.Get(ctx, c.CreatorSession); err == nil {
			text := fmt.Sprintf("[Board update: card %s %q, which you gave to %s, %s.]\n%s\n\n", c.ID, c.Title, c.Assignee, what, detail)
			if c.Status == Done {
				text += "Use the result: carry on with the work that needed it, and tell the user if they are waiting for it."
			} else {
				text += "Decide what to do: answer with board_comment and reopen it with board_update (status todo), reassign it, or cancel it. Tell the user if it matters to them."
			}
			if _, err := d.Runner.SubmitTurn(ctx, agent.Turn{ID: "turn_" + id, Session: c.CreatorSession, Text: text, Origin: c.ReplyTo,
				Background: true, Tainted: c.Tainted}); err != nil {
				return err
			}
			if d.Runner.Wake != nil {
				d.Runner.Wake()
			}
			sent = true
		}
	}
	if !sent && len(c.ReplyTo) > 0 && d.Deliver != nil {
		text := fmt.Sprintf("Card %s %q (%s) %s.\n\n%s", c.ID, c.Title, orNone(c.Assignee), what, detail)
		if err := d.Deliver(ctx, "dlv_"+id, c.ReplyTo, text); err != nil {
			return err
		}
	}
	_, err = d.Store.db.ExecContext(ctx, `UPDATE cards SET notify = 0 WHERE id = ? AND rev = ?`, c.ID, c.Rev)
	return err
}

func lastComment(c *Card) string {
	if len(c.Comments) == 0 {
		return "(no reason given)"
	}
	return c.Comments[len(c.Comments)-1].Text
}
