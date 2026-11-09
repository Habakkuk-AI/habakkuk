package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

type fakeHome struct {
	mu   sync.Mutex
	news []HomeNews
}

func (f *fakeHome) ReportHome(_ context.Context, n HomeNews) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.news = append(f.news, n)
	return nil
}

func (f *fakeHome) take() []HomeNews {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.news
	f.news = nil
	return out
}

// handle queues t and runs it to the end on this goroutine.
func (h *harness) handle(t *testing.T, turn Turn) string {
	t.Helper()
	ctx := context.Background()
	if _, err := h.runner.SubmitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	kind := turn.Kind
	if kind == "" {
		kind = KindTurn
	}
	j, err := h.queue.Claim(ctx, "w1", time.Minute, kind)
	if err != nil || j == nil {
		t.Fatalf("claim: %v", err)
	}
	final, err := h.runner.HandleTurn(ctx, j, "w1")
	if err != nil {
		t.Fatal(err)
	}
	return final
}

func TestHomeNewsOnlyForWorkFromElsewhere(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &fakeModel{})
	home := &fakeHome{}
	h.runner.Home = home
	spec := h.runner.Agents["habakkuk"]
	spec.Home, spec.ReportHome = "wa:creator", ReportAll
	session := func() string {
		s, err := h.runner.NewSession(ctx, "habakkuk")
		if err != nil {
			t.Fatal(err)
		}
		return s.ID
	}

	// A local client's turn: its user sees the answer.
	h.handle(t, Turn{Session: session(), Text: "list my files"})
	if got := home.take(); len(got) != 0 {
		t.Fatalf("local turn reported: %+v", got)
	}

	// A chat turn is handed over with its origin (the gateway drops the
	// home chat's own); all reports the start, then the result.
	origin := json.RawMessage(`{"account":"slack","conversation":"C1"}`)
	sid := session()
	final := h.handle(t, Turn{Session: sid, Text: "check the workspace\nplease", Origin: origin})
	got := home.take()
	if len(got) != 2 || !got[0].Started || got[1].Started || got[1].Final != final || got[0].Final != "" {
		t.Fatalf("chat turn news: %+v", got)
	}
	n := got[1]
	if n.Home != "wa:creator" || n.Agent != "habakkuk" || n.Session != sid || n.Kind != KindTurn ||
		string(n.Origin) != string(origin) || n.Task != "check the workspace" || n.Job != got[0].Job {
		t.Fatalf("news: %+v", n)
	}

	// Unattended work with no reply address, named by its source.
	h.handle(t, Turn{Session: session(), Text: "[Scheduled task \"ads\" (daily)]\ncheck ads", Source: SourceSchedule, Task: `scheduled task "ads"`})
	if got := home.take(); len(got) != 2 || got[1].Source != SourceSchedule || got[1].Task != `scheduled task "ads"` {
		t.Fatalf("schedule news: %+v", got)
	}
	h.handle(t, Turn{Session: session(), Text: "[A2A message from another agent]\nsummarise sales", Source: SourceA2A})
	if got := home.take(); len(got) != 2 || got[1].Task != "summarise sales" {
		t.Fatalf("a2a news: %+v", got)
	}

	// results: no start line.
	spec.ReportHome = ReportResults
	h.handle(t, Turn{Session: session(), Text: "again", Origin: origin})
	if got := home.take(); len(got) != 1 || got[0].Started {
		t.Fatalf("results news: %+v", got)
	}

	// off, or no home: nothing.
	spec.ReportHome = ReportOff
	h.handle(t, Turn{Session: session(), Text: "quiet", Origin: origin})
	spec.Home, spec.ReportHome = "", ReportResults
	h.handle(t, Turn{Session: session(), Text: "no home", Origin: origin})
	if got := home.take(); len(got) != 0 {
		t.Fatalf("off or homeless reported: %+v", got)
	}
}

// A delegated task reports with the delegating agent and chat; one an
// agent delegated to itself does not, since its parent turn reports.
func TestHomeNewsForSubturns(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &fakeModel{})
	home := &fakeHome{}
	h.runner.Home = home
	me := h.runner.Agents["habakkuk"]
	me.Home, me.ReportHome = "wa:creator", ReportResults
	boss := *me
	boss.Name, boss.Home = "boss", ""
	h.runner.Agents["boss"] = &boss
	origin := json.RawMessage(`{"account":"slack","conversation":"C1"}`)

	sub := func(parentAgent string) []HomeNews {
		parent, err := h.runner.NewSession(ctx, parentAgent)
		if err != nil {
			t.Fatal(err)
		}
		child, err := h.runner.NewSession(ctx, "habakkuk")
		if err != nil {
			t.Fatal(err)
		}
		h.handle(t, Turn{Kind: KindSubturn, Session: child.ID, Text: subturnPreamble + "count files",
			Sub: &SubturnSpec{Parent: parent.ID, ParentOrigin: origin, Task: "count files"}})
		return home.take()
	}
	got := sub("boss")
	if len(got) != 1 || got[0].Kind != KindSubturn || got[0].ParentAgent != "boss" ||
		string(got[0].ParentOrigin) != string(origin) || got[0].Task != "count files" || got[0].Final == "" {
		t.Fatalf("delegated news: %+v", got)
	}
	if got := sub("habakkuk"); len(got) != 0 {
		t.Fatalf("self-delegated task reported: %+v", got)
	}
}

// toolNames records the tools offered on each model call.
type toolNames struct {
	mu    sync.Mutex
	names [][]string
}

func (m *toolNames) Send(_ context.Context, _ []llm.Target, req llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	var names []string
	for _, t := range req.Tools {
		names = append(names, t.Name)
	}
	m.mu.Lock()
	m.names = append(m.names, names)
	m.mu.Unlock()
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "ok"}}}, StopReason: llm.StopEnd}, nil
}

type stubHomeTool struct{}

func (stubHomeTool) Risk() tools.Risk { return tools.RiskRead }
func (stubHomeTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: MessageHomeName, Schema: json.RawMessage(`{"type":"object"}`)}
}
func (stubHomeTool) Run(context.Context, *tools.Env, json.RawMessage) (string, error) { return "", nil }

func TestMessageHomeOfferedOnlyWithAHome(t *testing.T) {
	ctx := context.Background()
	m := &toolNames{}
	h := newHarness(t, m)
	h.runner.Tools.Register("core", stubHomeTool{})
	offered := func() bool {
		s, _ := h.runner.NewSession(ctx, "habakkuk")
		h.handle(t, Turn{Session: s.ID, Text: "hi"})
		m.mu.Lock()
		defer m.mu.Unlock()
		return slices.Contains(m.names[len(m.names)-1], MessageHomeName)
	}
	if offered() {
		t.Fatal("message_home offered to an agent without a home")
	}
	h.runner.Agents["habakkuk"].Home = "wa:creator"
	if !offered() {
		t.Fatal("message_home not offered to an agent with a home")
	}
}

func TestTaskLine(t *testing.T) {
	for in, want := range map[string]string{
		"[Scheduled task \"x\" (daily)]\n\ndo it\nmore": "do it",
		"  plain  ":       "plain",
		"[only a header]": "",
	} {
		if got := taskLine(in); got != want {
			t.Errorf("taskLine(%q) = %q, want %q", in, got, want)
		}
	}
	if !strings.HasPrefix(subturnPreamble, "[") {
		t.Fatal("the sub-turn preamble is no longer a bracketed header")
	}
}
