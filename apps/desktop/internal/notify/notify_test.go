package notify

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/fakehabakkuk"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/habakkukapi"
)

type recorder struct {
	mu       sync.Mutex
	notices  []Notice
	statuses []Status
}

func (r *recorder) notify(n Notice) { r.mu.Lock(); r.notices = append(r.notices, n); r.mu.Unlock() }
func (r *recorder) status(s Status) { r.mu.Lock(); r.statuses = append(r.statuses, s); r.mu.Unlock() }

func (r *recorder) all() []Notice {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.notices)
}

func (r *recorder) last() (Status, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.statuses) == 0 {
		return Status{}, false
	}
	return r.statuses[len(r.statuses)-1], true
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// start runs a watcher against f with short intervals.
func start(t *testing.T, f *fakehabakkuk.Server, token string, quiet func(string) bool) (*Watcher, *recorder) {
	t.Helper()
	rec := &recorder{}
	w := &Watcher{
		Client: &habakkukapi.Client{BaseURL: f.URL, Token: token},
		Notify: rec.notify, Status: rec.status, Quiet: quiet,
		ApprovalsEvery: 20 * time.Millisecond, StatusEvery: 20 * time.Millisecond,
		ConversationsEvery: 20 * time.Millisecond, RetryAfter: 10 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the watcher did not stop")
		}
	})
	return w, rec
}

func TestStatus(t *testing.T) {
	f := fakehabakkuk.New("tok")
	defer f.Close()
	var o habakkukapi.Overview
	o.Channels = []habakkukapi.Channel{{Account: "tg", State: "connected"}, {Account: "slack", State: "backoff"}}
	o.Approvals = 2
	o.Queue.Running = 1
	f.SetOverview(o)
	_, rec := start(t, f, "tok", nil)
	waitFor(t, "a status", func() bool { _, ok := rec.last(); return ok })
	st, _ := rec.last()
	if !st.Connected || st.Channels != 2 || st.ChannelsUp != 1 || st.Approvals != 2 || st.Running != 1 || st.Healthy() {
		t.Fatalf("status = %+v", st)
	}
}

func TestStatusWrongToken(t *testing.T) {
	f := fakehabakkuk.New("tok")
	defer f.Close()
	_, rec := start(t, f, "wrong", nil)
	waitFor(t, "a status", func() bool { _, ok := rec.last(); return ok })
	st, _ := rec.last()
	if st.Connected || !st.Unauthorized || !strings.Contains(st.Error, "401") {
		t.Fatalf("status = %+v", st)
	}
}

func TestApprovalNotifiedOnce(t *testing.T) {
	f := fakehabakkuk.New("tok")
	defer f.Close()
	a := habakkukapi.Approval{ID: "ap1", Tool: "exec", Summary: "run **rm -rf build**", Since: time.Now()}
	a.To.Account, a.To.Conversation = "web", "c-1"
	f.AddApproval(a)
	_, rec := start(t, f, "tok", nil)
	waitFor(t, "the approval", func() bool { return len(rec.all()) == 1 })
	n := rec.all()[0]
	if n.Kind != Approval || n.Approval != "ap1" || n.Title != "Approval needed: exec" || n.Body != "run rm -rf build" || n.Conversation != "c-1" {
		t.Fatalf("notice = %+v", n)
	}
	time.Sleep(100 * time.Millisecond) // several more polls
	if got := rec.all(); len(got) != 1 {
		t.Fatalf("notified %d times", len(got))
	}
	f.AddApproval(habakkukapi.Approval{ID: "ap2", Tool: "write_file"})
	waitFor(t, "the second approval", func() bool { return len(rec.all()) == 2 })
	if n := rec.all()[1]; n.Approval != "ap2" || n.Conversation != "" {
		t.Fatalf("second = %+v", n)
	}
}

func TestRepliesFromStream(t *testing.T) {
	f := fakehabakkuk.New("tok")
	defer f.Close()
	old := time.Now().Add(-time.Hour)
	f.AddConversation(habakkukapi.Conversation{Conversation: "c-1", Agent: "ops", LastAt: time.Now()})
	f.AddConversation(habakkukapi.Conversation{Conversation: "c-stale", LastAt: old.Add(-48 * time.Hour)})
	f.Push("c-1", "user", "hi", old)
	f.Push("c-1", "assistant", "an old answer", old)
	quiet := make(chan bool, 1)
	quiet <- false
	isQuiet := func(string) bool { v := <-quiet; quiet <- v; return v }
	w, rec := start(t, f, "tok", isQuiet)

	waitFor(t, "the stream", func() bool { return f.HasFollow("c-1 after=0") })
	if got := w.Following(); !slices.Equal(got, []string{"c-1"}) {
		t.Fatalf("following %v; a stale conversation must not be followed", got)
	}
	f.Push("c-1", "user", "and now?", time.Now())
	f.Push("c-1", "assistant", "## Done.\nThe **build** passed.", time.Now())
	waitFor(t, "the reply", func() bool { return len(rec.all()) == 1 })
	n := rec.all()[0]
	if n.Kind != Reply || n.Key != "reply:c-1:4" || n.Title != "ops replied" || n.Body != "Done. The build passed." || n.Conversation != "c-1" {
		t.Fatalf("notice = %+v", n)
	}

	// The user is looking at the conversation: no notice.
	<-quiet
	quiet <- true
	f.Push("c-1", "assistant", "seen it", time.Now())
	time.Sleep(100 * time.Millisecond)
	if len(rec.all()) != 1 {
		t.Fatalf("notified while quiet: %+v", rec.all())
	}
	<-quiet
	quiet <- false

	// A dropped stream resumes after the last message it saw.
	f.DropStreams()
	waitFor(t, "the reconnect", func() bool { return f.HasFollow("c-1 after=5") })
	f.Push("c-1", "assistant", "after the reconnect", time.Now())
	waitFor(t, "the next reply", func() bool { return len(rec.all()) == 2 })
	if n := rec.all()[1]; n.Body != "after the reconnect" {
		t.Fatalf("notice = %+v", n)
	}
	for _, n := range rec.all() {
		if strings.Contains(n.Body, "old answer") {
			t.Fatal("history from before the start was notified")
		}
	}
}

func TestApprovalPromptIsNotAReply(t *testing.T) {
	f := fakehabakkuk.New("")
	defer f.Close()
	f.AddConversation(habakkukapi.Conversation{Conversation: "c-2", LastAt: time.Now()})
	_, rec := start(t, f, "", nil)
	waitFor(t, "the stream", func() bool { return f.HasFollow("c-2") })

	now := time.Now()
	a := habakkukapi.Approval{ID: "ap9", Tool: "exec", Summary: "deploy", Since: now}
	a.To.Account, a.To.Conversation = "web", "c-2"
	f.AddApproval(a)
	f.Push("c-2", "assistant", "Habakkuk wants to run exec: deploy. Approve?", now)
	waitFor(t, "a notice", func() bool { return len(rec.all()) >= 1 })
	time.Sleep(100 * time.Millisecond)
	got := rec.all()
	if len(got) != 1 || got[0].Kind != Approval {
		t.Fatalf("notices = %+v, want only the approval", got)
	}
}

func TestCut(t *testing.T) {
	if got := cut("a  b\n\nc", 10); got != "a b c" {
		t.Fatalf("cut = %q", got)
	}
	if got := cut(strings.Repeat("é", 20), 5); got != "éééé…" {
		t.Fatalf("cut = %q", got)
	}
}
