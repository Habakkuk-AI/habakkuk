// Package notify watches the connected Habakkuk for things worth a desktop
// notification (tool calls waiting for approval, agent replies in the web
// chat) and for the status the tray shows (connected, channels healthy,
// approvals waiting).
//
// Replies come from each recent web conversation's Server-Sent Event stream
// (GET /api/chat/{conversation}/events); approvals and status are polled,
// since the API has no stream for them.
package notify

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/habakkukapi"
)

// Kind of notice.
type Kind string

const (
	Approval Kind = "approval"
	Reply    Kind = "reply"
)

// Notice is one notification to show.
type Notice struct {
	Kind         Kind
	Key          string // unique: approval:<id> or reply:<conversation>:<seq>
	Title        string
	Body         string
	Conversation string // the web conversation it belongs to, if any
	Approval     string // the approval's id
}

// Status is what the tray shows.
type Status struct {
	Connected    bool   `json:"connected"`
	Error        string `json:"error,omitempty"`
	Unauthorized bool   `json:"unauthorized,omitempty"`
	Channels     int    `json:"channels"`
	ChannelsUp   int    `json:"channels_up"`
	Approvals    int    `json:"approvals"`
	Running      int    `json:"running"`
	Pending      int    `json:"pending"`
}

// Healthy reports whether every channel is connected.
func (s Status) Healthy() bool { return s.Connected && s.ChannelsUp == s.Channels }

// Watcher follows one Habakkuk until its context ends.
type Watcher struct {
	Client *habakkukapi.Client
	Notify func(Notice)
	Status func(Status)
	// Quiet, when it returns true, suppresses reply notices for a
	// conversation (the user is looking at it).
	Quiet func(conversation string) bool
	Log   *slog.Logger

	ApprovalsEvery     time.Duration // default 5s
	StatusEvery        time.Duration // default 15s
	ConversationsEvery time.Duration // default 30s
	RetryAfter         time.Duration // a broken stream reconnects after this (default 3s)
	MaxStreams         int           // conversations followed at once (default 8)
	Recent             time.Duration // follow conversations active this recently (default 24h)
	Now                func() time.Time

	mu       sync.Mutex
	started  time.Time
	account  string
	seen     map[string]bool // approval ids already notified
	agents   map[string]string
	streams  map[string]context.CancelFunc
	lastSeq  map[string]int64
	wg       sync.WaitGroup
	approvMu sync.Mutex // one approvals poll at a time
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Run watches until ctx ends. Replies older than the start are never
// notified; approvals already waiting at the start are (once).
func (w *Watcher) Run(ctx context.Context) {
	w.mu.Lock()
	w.started = w.now()
	w.seen = map[string]bool{}
	w.agents = map[string]string{}
	w.streams = map[string]context.CancelFunc{}
	w.lastSeq = map[string]int64{}
	w.mu.Unlock()
	defer w.wg.Wait()

	approvals := time.NewTicker(or(w.ApprovalsEvery, 5*time.Second))
	defer approvals.Stop()
	// Status is polled every StatusEvery, but every second while Habakkuk
	// does not answer, so a starting serve shows up as soon as it listens.
	every := or(w.StatusEvery, 15*time.Second)
	status := time.NewTimer(every)
	defer status.Stop()
	convs := time.NewTicker(or(w.ConversationsEvery, 30*time.Second))
	defer convs.Stop()

	up := w.pollStatus(ctx)
	status.Reset(next(up, every))
	w.pollApprovals(ctx)
	w.pollConversations(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-status.C:
			up = w.pollStatus(ctx)
			status.Reset(next(up, every))
		case <-approvals.C:
			w.pollApprovals(ctx)
		case <-convs.C:
			w.pollConversations(ctx)
		}
	}
}

func (w *Watcher) logf(msg string, args ...any) {
	if w.Log != nil {
		w.Log.Debug(msg, args...)
	}
}

func next(up bool, every time.Duration) time.Duration {
	if up {
		return every
	}
	return min(every, time.Second)
}

// pollStatus reports the status and whether Habakkuk answered.
func (w *Watcher) pollStatus(ctx context.Context) bool {
	o, err := w.Client.Overview(ctx)
	if ctx.Err() != nil {
		return false
	}
	st := Status{}
	if err != nil {
		st.Error = err.Error()
		st.Unauthorized = habakkukapi.IsUnauthorized(err)
	} else {
		st.Connected = true
		st.Channels = len(o.Channels)
		for _, c := range o.Channels {
			if c.State == "connected" {
				st.ChannelsUp++
			}
		}
		st.Approvals = o.Approvals
		st.Running, st.Pending = o.Queue.Running, o.Queue.Pending
	}
	if w.Status != nil {
		w.Status(st)
	}
	return st.Connected
}

// pollApprovals notifies approvals not seen before and returns the list.
func (w *Watcher) pollApprovals(ctx context.Context) []habakkukapi.Approval {
	w.approvMu.Lock()
	defer w.approvMu.Unlock()
	w.ensureAccount(ctx)
	list, err := w.Client.Approvals(ctx)
	if err != nil {
		w.logf("approvals", "err", err)
		return nil
	}
	w.mu.Lock()
	var fresh []habakkukapi.Approval
	waiting := map[string]bool{}
	for _, a := range list {
		waiting[a.ID] = true
		if !w.seen[a.ID] {
			w.seen[a.ID] = true
			fresh = append(fresh, a)
		}
	}
	for id := range w.seen { // answered ones never come back
		if !waiting[id] {
			delete(w.seen, id)
		}
	}
	account := w.account
	w.mu.Unlock()
	for _, a := range fresh {
		n := Notice{Kind: Approval, Key: "approval:" + a.ID, Approval: a.ID,
			Title: "Approval needed: " + a.Tool, Body: cut(a.Summary, 240)}
		if a.To.Account == account {
			n.Conversation = a.To.Conversation
		}
		if w.Notify != nil {
			w.Notify(n)
		}
	}
	return list
}

// ensureAccount learns the web channel's account name, which approvals
// waiting in a web conversation carry.
func (w *Watcher) ensureAccount(ctx context.Context) bool {
	w.mu.Lock()
	known := w.account != ""
	w.mu.Unlock()
	if known {
		return true
	}
	info, err := w.Client.App(ctx)
	if err != nil {
		w.logf("app info", "err", err)
		return false
	}
	w.mu.Lock()
	w.account = info.Account
	w.mu.Unlock()
	return true
}

func (w *Watcher) pollConversations(ctx context.Context) {
	if !w.ensureAccount(ctx) {
		return
	}
	list, err := w.Client.Conversations(ctx, 50)
	if err != nil {
		w.logf("conversations", "err", err)
		return
	}
	slices.SortFunc(list, func(a, b habakkukapi.Conversation) int { return b.LastAt.Compare(a.LastAt) })
	recent := w.now().Add(-or(w.Recent, 24*time.Hour))
	limit := w.MaxStreams
	if limit <= 0 {
		limit = 8
	}
	want := map[string]bool{}
	for _, c := range list {
		if len(want) == limit || c.LastAt.Before(recent) {
			break
		}
		want[c.Conversation] = true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range list {
		w.agents[c.Conversation] = c.Agent
	}
	for conv, cancel := range w.streams {
		if !want[conv] {
			cancel()
			delete(w.streams, conv)
		}
	}
	for conv := range want {
		if _, ok := w.streams[conv]; ok || ctx.Err() != nil {
			continue
		}
		sctx, cancel := context.WithCancel(ctx)
		w.streams[conv] = cancel
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.follow(sctx, conv)
		}()
	}
}

// Following reports which conversations have a stream open (for tests and
// the UI).
func (w *Watcher) Following() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for c := range w.streams {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}

func (w *Watcher) follow(ctx context.Context, conv string) {
	for ctx.Err() == nil {
		w.mu.Lock()
		after := w.lastSeq[conv]
		w.mu.Unlock()
		last, err := w.Client.Follow(ctx, conv, after, func(m habakkukapi.Message) { w.message(ctx, conv, m) })
		w.mu.Lock()
		w.lastSeq[conv] = max(w.lastSeq[conv], last)
		w.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		w.logf("stream ended", "conversation", conv, "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(or(w.RetryAfter, 3*time.Second)):
		}
	}
}

func (w *Watcher) message(ctx context.Context, conv string, m habakkukapi.Message) {
	w.mu.Lock()
	if m.Seq > w.lastSeq[conv] {
		w.lastSeq[conv] = m.Seq
	}
	old := m.At.Before(w.started)
	account, agent := w.account, w.agents[conv]
	w.mu.Unlock()
	if m.Role != "assistant" || old || strings.TrimSpace(m.Text) == "" {
		return
	}
	// An approval prompt is also a message in the chat; the approval's own
	// notice (with buttons) covers it.
	for _, a := range w.pollApprovals(ctx) {
		if a.To.Account == account && a.To.Conversation == conv && absDur(m.At.Sub(a.Since)) < time.Minute {
			return
		}
	}
	if w.Quiet != nil && w.Quiet(conv) {
		return
	}
	title := "Habakkuk replied"
	if agent != "" && agent != "habakkuk" {
		title = agent + " replied"
	}
	if w.Notify != nil {
		w.Notify(Notice{Kind: Reply, Key: "reply:" + conv + ":" + strconv.FormatInt(m.Seq, 10), Title: title,
			Body: cut(m.Text, 240), Conversation: conv})
	}
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// cut shortens s to n runes on one line, Markdown markers stripped enough to
// read in a notification.
func cut(s string, n int) string {
	s = strings.Join(strings.Fields(strings.NewReplacer("**", "", "`", "", "#", "").Replace(s)), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
