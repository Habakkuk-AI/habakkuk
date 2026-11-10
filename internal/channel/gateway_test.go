package channel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// recorder is a fake adapter that records what it sends.
type recorder struct {
	mu      sync.Mutex
	sent    []sent
	failAt  int // fail the send with this 1-based index once
	maxLen  int
	sendErr error
	files   []sentFile
	maxFile int64
	keys    []string // SendKey of every send attempt
}

type sent struct {
	To      Address
	Text    string
	ReplyTo string
}

func (r *recorder) Run(ctx context.Context, in Inbox) error { <-ctx.Done(); return nil }
func (r *recorder) Send(ctx context.Context, to Address, text, replyTo string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, SendKey(ctx))
	if r.sendErr != nil {
		return r.sendErr
	}
	if r.failAt == len(r.sent)+1 {
		r.failAt = 0
		return errors.New("network blip")
	}
	r.sent = append(r.sent, sent{to, text, replyTo})
	return nil
}

type sentFile struct {
	To                    Address
	Name, MediaType, Data string
	Caption               string
}

func (r *recorder) SendFile(_ context.Context, to Address, name, mediaType string, data []byte, caption string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files = append(r.files, sentFile{to, name, mediaType, string(data), caption})
	return nil
}
func (r *recorder) MaxFileSize() int64 {
	if r.maxFile > 0 {
		return r.maxFile
	}
	return 50 << 20
}
func (r *recorder) sentFiles() []sentFile {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.files)
}
func (r *recorder) Format(s string) string    { return s }
func (r *recorder) StaleAfter() time.Duration { return time.Minute }
func (r *recorder) MaxLen() int {
	if r.maxLen > 0 {
		return r.maxLen
	}
	return 4000
}
func (r *recorder) texts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.sent {
		out = append(out, s.Text)
	}
	return out
}

// scripted answers "echo: <text>", or asks to run a command first when the
// user says "run it", or to send a file when the user says "send file <path>".
type scripted struct {
	mu    sync.Mutex
	calls int
	reply string // overrides the echo
	last  llm.Request
	delay time.Duration // each model call takes this long
}

func (s *scripted) Send(ctx context.Context, _ []llm.Target, req llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	s.mu.Lock()
	s.calls++
	s.last = req
	delay := s.delay
	s.mu.Unlock()
	if delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	last := req.Messages[len(req.Messages)-1]
	if _, path, ok := strings.Cut(last.Text(), "send file "); ok {
		input, _ := json.Marshal(map[string]string{"path": path, "caption": "here it is"})
		return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{
			{Type: llm.ToolUse, ToolUseID: "t-send", ToolName: "send_file", Input: input},
		}}, StopReason: llm.StopToolUse}, nil
	}
	for _, b := range last.Blocks {
		if b.Type == llm.ToolResult {
			return text("tool said: " + b.Text), nil
		}
	}
	if strings.Contains(last.Text(), "run it") {
		return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{
			{Type: llm.ToolUse, ToolUseID: "t1", ToolName: "exec", Input: json.RawMessage(`{"command":"echo approved-run"}`)},
		}}, StopReason: llm.StopToolUse}, nil
	}
	if s.reply != "" {
		return text(s.reply), nil
	}
	_, msg, _ := strings.Cut(last.Text(), "\n") // drop the time stamp line
	return text("echo: " + msg), nil
}

func text(s string) *llm.Response {
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: s}}}, StopReason: llm.StopEnd}
}

type rig struct {
	g     *Gateway
	a     *recorder
	q     *jobs.Queue
	model *scripted
	pool  *jobs.Pool
	dir   string
	// stop shuts the rig down the way SIGTERM stops serve: the workers are
	// told to stop and waited for. It is safe to call more than once.
	stop func()
}

func newRig(t *testing.T, ch *config.Channel) *rig {
	t.Helper()
	return openRig(t, t.TempDir(), ch)
}

// openRig starts a gateway on the databases in dir. Calling it again on the
// same dir, after the first rig's stop, is a restart: the same jobs,
// conversations and transcripts, and nothing that was only in memory.
// prepare runs before the workers start, so a restarted rig is fully set up
// before it resumes the jobs it finds.
func openRig(t *testing.T, dir string, ch *config.Channel, prepare ...func(*rig)) *rig {
	t.Helper()
	ctx := context.Background()
	control, err := store.Open(ctx, filepath.Join(dir, "control.db"), slices.Concat(jobs.Migrations, Migrations))
	if err != nil {
		t.Fatal(err)
	}
	transcripts, err := store.Open(ctx, filepath.Join(dir, "transcripts.db"), session.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close(); transcripts.Close() })
	ws := filepath.Join(dir, "ws")
	os.MkdirAll(ws, 0o755)
	reg := tools.NewRegistry()
	tools.RegisterBuiltins(reg)
	model := &scripted{}
	q := jobs.NewQueue(control)
	runner := &agent.Runner{
		Agents: map[string]*agent.Spec{"habakkuk": {Name: "habakkuk", Chain: []llm.Target{{Endpoint: "fake", Model: "m"}},
			MaxTokens: 1000, MaxIterations: 10, Toolsets: []string{"core"}, Mode: tools.ModeWorkspace, Workspace: ws, Dir: dir}},
		Sessions: session.NewStore(transcripts), Queue: q, Models: model, Tools: reg, Bus: events.NewBus(), Log: quiet,
		Env: func(s *agent.Spec, _ string) *tools.Env { return &tools.Env{Workspace: s.Workspace, Mode: s.Mode} },
	}
	if ch.Type == "" {
		ch.Type = "telegram"
	}
	if ch.Agent == "" {
		ch.Agent = "habakkuk"
	}
	if ch.Groups == "" {
		ch.Groups = "mention"
	}
	rec := &recorder{}
	blobs, err := store.NewBlobs(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	runner.Media = blobs.Get
	g := &Gateway{
		Channels: map[string]*config.Channel{"tg": ch}, Adapters: map[string]Adapter{"tg": rec},
		Runner: runner, Queue: q, Store: NewStore(control), Log: quiet, ApprovalTimeout: 5 * time.Second,
		Blobs: blobs,
	}
	runner.Approvers = g.ApproverFor
	RegisterTools(reg, g)
	pool := &jobs.Pool{Queue: q, Handlers: map[string]jobs.Handler{agent.KindTurn: g.HandleTurn, KindDeliver: g.HandleDeliver},
		Concurrency: 2, Lease: 5 * time.Second, Poll: 10 * time.Millisecond, Log: quiet}
	g.Wake = pool.Wake
	r := &rig{g: g, a: rec, q: q, model: model, pool: pool, dir: dir}
	for _, f := range prepare {
		f(r)
	}
	pctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { pool.Run(pctx); close(done) }()
	var once sync.Once
	r.stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(r.stop)
	return r
}

func dm(id, text string) Message {
	return Message{Account: "tg", Conversation: "100", ID: id, Sender: "7", SenderName: "Ay", Text: text, Direct: true}
}

// waitFor polls until cond holds or fails the test.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func hasText(r *recorder, sub string) func() bool {
	return func() bool {
		for _, s := range r.texts() {
			if strings.Contains(s, sub) {
				return true
			}
		}
		return false
	}
}

func TestMessageGetsReply(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	r.g.Handle(context.Background(), dm("m1", "hello there"))
	waitFor(t, "reply", hasText(r.a, "echo: hello there"))
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	if s := r.a.sent[0]; s.ReplyTo != "m1" || s.To.Conversation != "100" {
		t.Fatalf("reply addressed wrong: %+v", s)
	}
}

func TestRedeliveredMessageRunsOnce(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ctx := context.Background()
	r.g.Handle(ctx, dm("m1", "once"))
	r.g.Handle(ctx, dm("m1", "once")) // platform redelivery
	waitFor(t, "reply", hasText(r.a, "echo: once"))
	time.Sleep(150 * time.Millisecond)
	if n := len(r.a.texts()); n != 1 {
		t.Fatalf("got %d replies, want 1", n)
	}
	if r.model.calls != 1 {
		t.Fatalf("model called %d times", r.model.calls)
	}
}

func TestUnknownSenderPairs(t *testing.T) {
	r := newRig(t, &config.Channel{})
	ctx := context.Background()
	r.g.Handle(ctx, dm("m1", "hi"))
	waitFor(t, "pairing code", hasText(r.a, "habakkuk pair "))
	msg := r.a.texts()[0]
	code := strings.TrimSuffix(strings.Fields(msg[strings.Index(msg, "habakkuk pair "):])[2], "`")
	r.g.Handle(ctx, dm("m2", "hi again")) // throttled: no second notice, no turn
	time.Sleep(100 * time.Millisecond)
	if len(r.a.texts()) != 1 || r.model.calls != 0 {
		t.Fatalf("unknown sender got through: %q", r.a.texts())
	}
	if _, sender, _, err := r.g.Store.Pair(ctx, code); err != nil || sender != "7" {
		t.Fatalf("pair: %v %q", err, sender)
	}
	r.g.Handle(ctx, dm("m3", "now?"))
	waitFor(t, "reply after pairing", hasText(r.a, "echo: now?"))
}

func TestGroupNeedsMention(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"*"}})
	ctx := context.Background()
	g := Message{Account: "tg", Conversation: "-5", ID: "g1", Sender: "8", SenderName: "Bo", Text: "chatter"}
	r.g.Handle(ctx, g)
	g.ID, g.Text, g.Mentioned = "g2", "what's up", true
	r.g.Handle(ctx, g)
	waitFor(t, "group reply", hasText(r.a, "echo: Bo: what's up"))
	if strings.Contains(strings.Join(r.a.texts(), "|"), "chatter") {
		t.Fatal("answered a message that did not mention the bot")
	}
}

func TestNewCommandStartsFreshSession(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ctx := context.Background()
	r.g.Handle(ctx, dm("m1", "first"))
	waitFor(t, "first reply", hasText(r.a, "echo: first"))
	to := Address{Account: "tg", Conversation: "100"}
	before, _ := r.g.Store.Conversation(ctx, to)
	r.g.Handle(ctx, dm("m2", "/new"))
	waitFor(t, "ack", hasText(r.a, "fresh conversation"))
	after, _ := r.g.Store.Conversation(ctx, to)
	if before.SessionID == after.SessionID {
		t.Fatal("/new kept the old session")
	}
}

func TestIdleConversationGetsNewSession(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}, SessionIdle: time.Hour})
	ctx := context.Background()
	now := time.Now()
	r.g.Now = func() time.Time { return now }
	to := Address{Account: "tg", Conversation: "100"}
	a, _ := r.g.conversation(ctx, r.g.Channels["tg"], to, "", false)
	b, _ := r.g.conversation(ctx, r.g.Channels["tg"], to, "", false)
	now = now.Add(2 * time.Hour)
	r.g.Store.now = func() time.Time { return now }
	c, _ := r.g.conversation(ctx, r.g.Channels["tg"], to, "", false)
	if a.SessionID != b.SessionID || b.SessionID == c.SessionID {
		t.Fatalf("sessions %s %s %s", a.SessionID, b.SessionID, c.SessionID)
	}
}

func TestApprovalInChat(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ctx := context.Background()
	r.g.Handle(ctx, dm("m1", "please run it"))
	waitFor(t, "approval prompt", hasText(r.a, "I'd like to run a command on your computer."))
	r.g.Handle(ctx, dm("m2", "what is this?"))
	waitFor(t, "reminder", hasText(r.a, "waiting on the request"))
	r.g.Handle(ctx, dm("m3", "yes"))
	waitFor(t, "tool output", hasText(r.a, "approved-run"))
}

func TestApprovalDenied(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ctx := context.Background()
	r.g.Handle(ctx, dm("m1", "run it"))
	waitFor(t, "approval prompt", hasText(r.a, "I'd like to run a command on your computer."))
	r.g.Handle(ctx, dm("m2", "no"))
	waitFor(t, "denial result", hasText(r.a, "tool said:"))
	if hasText(r.a, "approved-run\n")() {
		t.Fatal("denied command ran")
	}
}

func TestDeliveryResumesWithoutRepeatingChunks(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	r.a.maxLen = 120
	r.a.failAt = 2 // the second chunk fails once
	var long []string
	for i := 0; i < 8; i++ {
		long = append(long, strings.Repeat(string(rune('a'+i)), 40))
	}
	r.model.reply = strings.Join(long, "\n\n")
	r.g.Handle(context.Background(), dm("m1", "long please"))
	waitFor(t, "all chunks", hasText(r.a, strings.Repeat("h", 40)))
	joined := strings.Join(r.a.texts(), "\n")
	for i := 0; i < 8; i++ {
		if n := strings.Count(joined, strings.Repeat(string(rune('a'+i)), 40)); n != 1 {
			t.Fatalf("paragraph %d sent %d times", i, n)
		}
	}
	// The retried chunk carries the same send key as its failed attempt.
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	want := "dlv_" + TurnID(dm("m1", "")) + ".1"
	if len(r.a.keys) < 3 || r.a.keys[1] != want || r.a.keys[2] != want {
		t.Fatalf("send keys: %v", r.a.keys)
	}
}

func TestRerunTurnDeliversOnce(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ctx := context.Background()
	r.g.Handle(ctx, dm("m1", "hi"))
	waitFor(t, "reply", hasText(r.a, "echo: hi"))
	// Simulate a crash after the turn finished but before it was marked done:
	// the job runs again.
	id := TurnID(dm("m1", ""))
	j, _ := r.q.Get(ctx, id)
	if _, err := r.g.HandleTurn(ctx, j, "w-again"); err != nil && !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(r.a.texts()); n != 1 {
		t.Fatalf("got %d replies, want 1", n)
	}
}

func TestUndeliverableStopsRetrying(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	r.a.sendErr = Undeliverable{Err: errors.New("bot was blocked")}
	ctx := context.Background()
	r.g.Handle(ctx, dm("m1", "hi"))
	id := "dlv_" + TurnID(dm("m1", ""))
	waitFor(t, "delivery failed", func() bool {
		j, err := r.q.Get(ctx, id)
		return err == nil && j.Status == jobs.Failed && j.Attempts == 1
	})
}

func TestAttachmentsReachTheModelAndWorkspace(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	r.g.Transcribe = func(_ context.Context, name, mediaType string, data []byte) (string, error) {
		return "buy milk at " + string(data), nil
	}
	png := []byte("\x89PNG\r\n\x1a\nrest-of-image")
	m := dm("m1", "what is this?")
	m.Attachments = []Attachment{
		{Name: "../shot.png", Fetch: func(context.Context) ([]byte, error) { return png, nil }},
		{Name: "voice.ogg", MediaType: "audio/ogg", Fetch: func(context.Context) ([]byte, error) { return []byte("noon"), nil }},
		{Name: "big.zip", Size: 1 << 30, Fetch: func(context.Context) ([]byte, error) { panic("must not download") }},
		{Name: "gone.pdf", Fetch: func(context.Context) ([]byte, error) { return nil, errors.New("expired") }},
	}
	r.g.Handle(context.Background(), m)
	waitFor(t, "reply", hasText(r.a, "echo: what is this?"))

	r.model.mu.Lock()
	req := r.model.last
	r.model.mu.Unlock()
	user := req.Messages[0]
	text := user.Blocks[0].Text
	for _, want := range []string{"Attached shot.png (image/png", "Voice note transcript: buy milk at noon", "not downloaded: larger than 25 MB", "could not be downloaded: expired"} {
		if !strings.Contains(text, want) {
			t.Errorf("user text missing %q:\n%s", want, text)
		}
	}
	if len(user.Blocks) != 2 || user.Blocks[1].Type != llm.Image || user.Blocks[1].MediaType != "image/png" {
		t.Fatalf("blocks: %+v", user.Blocks)
	}
	data, err := req.Media(user.Blocks[1].Media)
	if err != nil || string(data) != string(png) {
		t.Fatalf("image bytes not loadable: %v", err)
	}
	saved, _ := filepath.Glob(filepath.Join(r.g.Runner.Agents["habakkuk"].Workspace, "inbox", "*-shot.png"))
	if len(saved) != 1 {
		t.Fatalf("image not saved to the workspace inbox: %v", saved)
	}
}

func TestNewDuringApprovalDeclinesIt(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ctx := context.Background()
	r.g.Handle(ctx, dm("m1", "run it"))
	waitFor(t, "approval prompt", hasText(r.a, "I'd like to run a command on your computer."))
	r.g.Handle(ctx, dm("m2", "/new"))
	waitFor(t, "fresh conversation", hasText(r.a, "fresh conversation"))
	waitFor(t, "old turn ends with the call declined", hasText(r.a, "tool said:"))
	if hasText(r.a, "waiting on the request")() {
		t.Fatal("/new was swallowed by the approval reminder")
	}
}

func TestSilentReplyIsNotSentAndDeliverSends(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	ctx := context.Background()
	r.model.reply = Silent
	m := dm("m1", "anything new?")
	r.g.Handle(ctx, m)
	waitFor(t, "turn done", func() bool {
		j, err := r.q.Get(ctx, TurnID(m))
		return err == nil && j.Status == jobs.Done
	})
	if got := r.a.texts(); len(got) != 0 {
		t.Fatalf("silent reply was sent: %q", got)
	}
	to, _ := json.Marshal(Address{Account: "tg", Conversation: "100"})
	if err := r.g.Deliver(ctx, "dlv_script", to, "3 new invoices"); err != nil {
		t.Fatal(err)
	}
	r.g.Deliver(ctx, "dlv_script", to, "3 new invoices") // same id: no second send
	waitFor(t, "script output", hasText(r.a, "3 new invoices"))
	time.Sleep(100 * time.Millisecond)
	if got := r.a.texts(); len(got) != 1 {
		t.Fatalf("sent %q", got)
	}
}
