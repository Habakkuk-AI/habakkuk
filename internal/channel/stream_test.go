package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/state"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// streamModel writes its answer in pieces, the way a real model streams,
// before it returns the whole response. A turn whose last message holds a
// tool result gets final; any other gets first.
type streamModel struct {
	mu     sync.Mutex
	gap    time.Duration
	first  stream
	final  stream
	called int
}

type stream struct {
	pieces []string
	tool   *llm.Response // returned after the pieces, instead of their text
}

func (m *streamModel) Send(ctx context.Context, _ []llm.Target, _ llm.Request, onEvent func(llm.Event)) (*llm.Response, error) {
	m.mu.Lock()
	m.called++
	s := m.first
	if m.called > 1 {
		s = m.final
	}
	m.mu.Unlock()
	for _, p := range s.pieces {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(m.gap):
		}
		onEvent(llm.Event{Type: llm.EventText, Text: p})
	}
	if s.tool != nil {
		return s.tool, nil
	}
	return text(strings.Join(s.pieces, "")), nil
}

// loudEditor is an editor whose Post is silent and that can post a message
// that notifies, as Telegram does.
type loudEditor struct{ *editor }

func (e loudEditor) PostLoud(_ context.Context, _ Address, text, replyTo string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.next++
	id := fmt.Sprint(e.next)
	e.log = append(e.log, "loud "+id+" "+text+" (quotes "+replyTo+")")
	return id, nil
}

func words(s string) []string { return strings.SplitAfter(s, " ") }

const longAnswer = "Here is what I found about the bakery's opening hours this week, and why Thursday is different."

// A reply shows while it is written, in a message of its own that notifies
// and quotes the question, and that message becomes the reply: no second
// copy is sent and nothing is deleted.
func TestStreamedReplyBecomesTheReply(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ed := loudEditor{&editor{recorder: r.a}}
	r.g.Adapters["tg"] = ed
	r.g.StreamEvery = 20 * time.Millisecond
	r.g.Runner.Models = &streamModel{gap: 15 * time.Millisecond, first: stream{pieces: words(longAnswer)}}
	send(r, "m1", "when is the bakery open")
	waitFor(t, "final edit", func() bool { return slices.Contains(ed.entries(), "edit 1 "+longAnswer) })
	time.Sleep(50 * time.Millisecond)
	log := ed.entries()
	if !strings.HasPrefix(log[0], "loud 1 Here is what I found") || !strings.Contains(log[0], streamCursor) || !strings.HasSuffix(log[0], "(quotes m1)") {
		t.Fatalf("the reply must start as a message that notifies and quotes: %q", log)
	}
	if n := len(slices.DeleteFunc(slices.Clone(log), func(s string) bool { return !strings.HasPrefix(s, "edit 1 ") })); n < 2 {
		t.Fatalf("want the message edited as the text arrives: %q", log)
	}
	for _, e := range log {
		if strings.HasPrefix(e, "send ") || strings.HasPrefix(e, "delete ") || strings.HasPrefix(e, "post ") {
			t.Fatalf("the streamed message is the reply; nothing else may be sent or removed: %q", log)
		}
	}
	if d, _ := r.g.State.Draft(context.Background(), TurnID(dm("m1", ""))); d != nil {
		t.Fatalf("the draft must be forgotten once it is the reply: %+v", d)
	}
}

// Where posts already notify (no LoudPoster), the reply is posted and
// edited the same way.
func TestStreamedReplyWithAnOrdinaryEditor(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ed := &editor{recorder: r.a}
	r.g.Adapters["tg"] = ed
	r.g.StreamEvery = 20 * time.Millisecond
	r.g.Runner.Models = &streamModel{gap: 15 * time.Millisecond, first: stream{pieces: words(longAnswer)}}
	send(r, "m1", "when is the bakery open")
	waitFor(t, "final edit", func() bool { return slices.Contains(ed.entries(), "edit 1 "+longAnswer) })
	if log := ed.entries(); !strings.HasPrefix(log[0], "post 1 Here is what I found") {
		t.Fatalf("want the reply posted, then edited: %q", log)
	}
	if len(r.a.texts()) != 0 {
		t.Fatalf("no second copy may be sent: %q", r.a.texts())
	}
}

// What the model writes before it calls a tool is not the reply: the
// message goes back to showing progress, and the reply, which came out
// different, is sent as a new message (so it notifies) once the work is
// done.
func TestTextBeforeAToolIsNotTheReply(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ed := loudEditor{&editor{recorder: r.a}}
	r.g.Adapters["tg"] = ed
	r.g.StreamEvery, r.g.ProgressAfter, r.g.ProgressEvery = 20*time.Millisecond, time.Hour, 20*time.Millisecond
	r.g.Runner.Models = &streamModel{gap: 15 * time.Millisecond,
		first: stream{pieces: words("Let me look through the folder first, this may take me a little while to finish."),
			tool: toolCall("c1", "list_dir", `{"path":"."}`)},
		final: stream{pieces: []string{"All done."}}}
	send(r, "m1", "tidy up")
	waitFor(t, "reply", hasText(r.a, "All done."))
	log := ed.entries()
	loud := slices.IndexFunc(log, func(s string) bool { return strings.HasPrefix(s, "loud 1 Let me look") })
	back := slices.IndexFunc(log, func(s string) bool { return strings.HasPrefix(s, "edit 1 _") })
	del := slices.Index(log, "delete 1")
	sent := slices.Index(log, "send All done.")
	if loud < 0 || back < loud || del < back || sent < del {
		t.Fatalf("want: streamed text, back to progress, draft removed, reply sent: %q", log)
	}
}

// A short "let me check" before a tool call never shows.
func TestShortTextBeforeAToolDoesNotShow(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ed := loudEditor{&editor{recorder: r.a}}
	r.g.Adapters["tg"] = ed
	r.g.StreamEvery, r.g.ProgressAfter = 10*time.Millisecond, time.Hour
	r.g.Runner.Models = &streamModel{gap: 5 * time.Millisecond,
		first: stream{pieces: []string{"Let me ", "check."}, tool: toolCall("c1", "list_dir", `{"path":"."}`)},
		final: stream{pieces: []string{"Nothing there."}}}
	send(r, "m1", "anything in the folder")
	waitFor(t, "reply", hasText(r.a, "Nothing there."))
	for _, e := range ed.entries() {
		if strings.Contains(e, "Let me") {
			t.Fatalf("a short line before a tool call showed: %q", ed.entries())
		}
	}
}

// With stream off, the reply arrives whole, as before.
func TestStreamOff(t *testing.T) {
	off := false
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}, Stream: &off})
	ed := loudEditor{&editor{recorder: r.a}}
	r.g.Adapters["tg"] = ed
	r.g.StreamEvery, r.g.ProgressAfter = 10*time.Millisecond, time.Hour
	r.g.Runner.Models = &streamModel{gap: 10 * time.Millisecond, first: stream{pieces: words(longAnswer)}}
	send(r, "m1", "when is the bakery open")
	waitFor(t, "reply", hasText(r.a, longAnswer))
	if log := ed.entries(); len(log) != 1 || log[0] != "send "+longAnswer {
		t.Fatalf("with stream off only the reply is sent: %q", log)
	}
}

// A reply that sends nothing never shows while it is written.
func TestSilentReplyIsNotStreamed(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ed := loudEditor{&editor{recorder: r.a}}
	r.g.Adapters["tg"] = ed
	r.g.StreamEvery = 5 * time.Millisecond
	r.g.Runner.Models = &streamModel{gap: 20 * time.Millisecond, first: stream{pieces: []string{"[SIL", "ENT]"}}}
	send(r, "m1", "anything new")
	time.Sleep(300 * time.Millisecond)
	if log := ed.entries(); len(log) != 0 {
		t.Fatalf("a silent reply showed: %q", log)
	}
}

// A delivery that reruns after a crash takes up where it stopped: the
// streamed message is the reply's first part whichever step it reached,
// and it is never deleted or sent a second time.
func TestAdoptedDraftSurvivesARerun(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ed := &editor{recorder: r.a}
	r.g.Adapters["tg"] = ed
	ctx := context.Background()
	to := Address{Account: "tg", Conversation: "100"}
	addr, _ := json.Marshal(to)
	if err := r.g.State.SaveDraft(ctx, "turn1", state.Draft{Address: string(addr), MessageID: "9", Answer: "Hello wor"}); err != nil {
		t.Fatal(err)
	}
	if !r.g.adoptDraft(ctx, "dlv_turn1", "turn1", ed, to, "Hello world") {
		t.Fatal("the draft showing this reply was not taken")
	}
	// The crash: the chunk count is lost, the draft's record is not.
	if err := r.g.Store.MarkSent(ctx, "dlv_turn1", 0); err != nil {
		t.Fatal(err)
	}
	if !r.g.adoptDraft(ctx, "dlv_turn1", "turn1", ed, to, "Hello world") {
		t.Fatal("a rerun did not find the draft it had taken")
	}
	r.g.finishDraft(ctx, "turn1", ed, to)
	if log := ed.entries(); !slices.Equal(log, []string{"edit 9 Hello world"}) {
		t.Fatalf("want one edit and nothing deleted: %q", log)
	}
	if n, _ := r.g.Store.Sent(ctx, "dlv_turn1"); n != 1 {
		t.Fatalf("want the first chunk counted as sent, got %d", n)
	}
}

// A draft is taken only for the reply it was showing.
func TestDraftIsNotTakenForADifferentReply(t *testing.T) {
	r := newStateRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ed := &editor{recorder: r.a}
	ctx := context.Background()
	to := Address{Account: "tg", Conversation: "100"}
	addr, _ := json.Marshal(to)
	r.g.State.SaveDraft(ctx, "turn1", state.Draft{Address: string(addr), MessageID: "9", Answer: "Here is the plan for"})
	if r.g.adoptDraft(ctx, "dlv_turn1", "turn1", ed, to, "Something went wrong: the model is unavailable") {
		t.Fatal("a failure notice took the streamed reply's message")
	}
	if len(ed.entries()) != 0 {
		t.Fatalf("nothing may be edited: %q", ed.entries())
	}
}

func TestStreamPreview(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Plain text so far", "Plain text so far" + streamCursor},
		{"Run `hk sta", "Run `hk sta`" + streamCursor},
		{"Like this:\n```go\nfunc main() {", "Like this:\n```go\nfunc main() {\n```"},
		{"This is **very", "This is **very**" + streamCursor},
		{"Done:\n```\nok\n```\nand `x` too", "Done:\n```\nok\n```\nand `x` too" + streamCursor},
	} {
		if got := streamPreview(c.in, 0); got != c.want {
			t.Errorf("streamPreview(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := streamPreview(strings.Repeat("a", 100), 50); got != strings.Repeat("a", 42)+"…"+streamCursor {
		t.Errorf("a long preview is not cut to the chunk: %q", got)
	}
}
