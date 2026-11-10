package state

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestSettingsAndGoal(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if st, err := s.Settings(ctx, "ses1"); err != nil || st != (Settings{}) {
		t.Fatalf("empty settings = %+v, %v", st, err)
	}
	s.SetEffort(ctx, "ses1", "max")
	s.SetPersonality(ctx, "ses1", "warm")
	if st, _ := s.Settings(ctx, "ses1"); st.Effort != "max" || st.Personality != "warm" {
		t.Fatalf("settings = %+v", st)
	}
	s.SetEffort(ctx, "ses1", "")
	if st, _ := s.Settings(ctx, "ses1"); st.Effort != "" || st.Personality != "warm" {
		t.Fatalf("clearing effort touched the persona: %+v", st)
	}

	if err := s.SetGoal(ctx, "ses1", "ship it", 3, `{"account":"tg"}`); err != nil {
		t.Fatal(err)
	}
	s.CountGoalTurn(ctx, "ses1", "turn_a")
	g, _ := s.CountGoalTurn(ctx, "ses1", "turn_a") // a rerun counts once
	if g.Turns != 1 || g.Text != "ship it" || g.Budget != 3 || g.Origin != `{"account":"tg"}` {
		t.Fatalf("goal = %+v", g)
	}
	g, _ = s.CountGoalTurn(ctx, "ses1", "turn_b")
	if g.Turns != 2 {
		t.Fatalf("turns = %d", g.Turns)
	}
	if ok, _ := s.ClearGoal(ctx, "ses1"); !ok {
		t.Fatal("clear found no goal")
	}
	if g, _ := s.CountGoalTurn(ctx, "ses1", "turn_c"); g != nil {
		t.Fatalf("goal after clear = %+v", g)
	}
}

func TestSteersAreTakenOnceInOrder(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	s.AddSteer(ctx, "ses1", "first")
	s.AddSteer(ctx, "ses1", "second")
	s.AddSteer(ctx, "ses2", "elsewhere")
	got, err := s.TakeSteers(ctx, "ses1")
	if err != nil || strings.Join(got, ",") != "first,second" {
		t.Fatalf("steers = %v, %v", got, err)
	}
	if got, _ := s.TakeSteers(ctx, "ses1"); len(got) != 0 {
		t.Fatalf("taken twice: %v", got)
	}
	if got, _ := s.TakeSteers(ctx, "ses2"); len(got) != 1 {
		t.Fatalf("other session lost its note: %v", got)
	}
}

func TestTodoToolAndPreamble(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	reg := tools.NewRegistry()
	RegisterTools(reg, s)
	env := &tools.Env{Session: "ses1", Mode: tools.ModeReadOnly} // planning works in every mode
	call := func(input string) llm.Block {
		return reg.Execute(ctx, env, llm.Block{Type: llm.ToolUse, ToolUseID: "c", ToolName: "todo", Input: json.RawMessage(input)})
	}
	res := call(`{"action":"add","items":["draft","review"," "]}`)
	if res.IsError || res.Text != "1. [ ] draft\n2. [ ] review" {
		t.Fatalf("add = %+v", res)
	}
	call(`{"action":"update","id":1,"status":"in_progress"}`)
	o := Overlay{Store: s}
	if p := o.Preamble(ctx, "ses1"); !strings.HasPrefix(p, TaskListMarker) || !strings.Contains(p, "1. [~] draft") {
		t.Fatalf("preamble = %q", p)
	}
	call(`{"action":"complete","id":1}`)
	call(`{"action":"complete","id":2}`)
	if p := o.Preamble(ctx, "ses1"); p != "" {
		t.Fatalf("a finished list is still shown: %q", p)
	}
	if res := call(`{"action":"update","id":9,"text":"x"}`); !res.IsError {
		t.Fatal("updated a missing item")
	}
	if res := call(`{"action":"update","id":1,"status":"maybe"}`); !res.IsError {
		t.Fatal("accepted a bad status")
	}
	res = call(`{"action":"clear"}`)
	if res.IsError || res.Text != "(the task list is empty)" {
		t.Fatalf("clear = %+v", res)
	}
	if res := call(`{"action":"list"}`); res.IsError {
		t.Fatalf("list = %+v", res)
	}
	if o.Preamble(ctx, "other") != "" {
		t.Fatal("task list leaked across sessions")
	}
}

func TestPersonalities(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "pirate.md"), []byte("Talk like a pirate.\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "warm.md"), []byte("Custom warmth."), 0o644)
	os.WriteFile(filepath.Join(dir, "Bad Name.md"), []byte("x"), 0o644)
	p := Personalities{Dir: dir}
	if got := strings.Join(p.List(), ","); got != "concise,formal,pirate,playful,technical,warm" {
		t.Fatalf("list = %s", got)
	}
	if s, _ := p.Text("Pirate"); s != "Talk like a pirate." {
		t.Fatalf("pirate = %q", s)
	}
	if s, _ := p.Text("warm"); s != "Custom warmth." {
		t.Fatalf("a file should override the preset: %q", s)
	}
	if _, err := p.Text("../etc/passwd"); err == nil {
		t.Fatal("path escaped the directory")
	}
	if _, err := p.Text("nope"); err == nil {
		t.Fatal("unknown persona accepted")
	}

	s := open(t)
	o := Overlay{Store: s, Personas: p}
	if o.System(ctx, "ses1") != "" || o.Effort(ctx, "ses1") != "" {
		t.Fatal("overlay set without settings")
	}
	s.SetPersonality(ctx, "ses1", "pirate")
	s.SetEffort(ctx, "ses1", "low")
	if sys := o.System(ctx, "ses1"); !strings.HasPrefix(sys, PersonaHeading) || !strings.HasSuffix(sys, "Talk like a pirate.") {
		t.Fatalf("system = %q", sys)
	}
	if o.Effort(ctx, "ses1") != "low" {
		t.Fatal("effort override lost")
	}
}

func TestDraftsAndPrune(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if d, _ := s.Draft(ctx, "turn_1"); d != nil {
		t.Fatal("draft from nowhere")
	}
	s.SaveDraft(ctx, "turn_1", Draft{Address: `{"account":"tg"}`, MessageID: "42"})
	if d, _ := s.Draft(ctx, "turn_1"); d == nil || d.MessageID != "42" {
		t.Fatalf("draft = %+v", d)
	}
	s.SetConversationAgent(ctx, "tg|1|", "scout")
	s.AddTodos(ctx, "ses1", []string{"a"})
	s.SetEffort(ctx, "ses1", "high")
	n, err := s.Prune(ctx, time.Now().Add(time.Hour))
	if err != nil || n != 3 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if a, _ := s.ConversationAgent(ctx, "tg|1|"); a != "scout" {
		t.Fatal("pruning dropped a conversation's chosen agent")
	}
	s.SetConversationAgent(ctx, "tg|1|", "")
	if a, _ := s.ConversationAgent(ctx, "tg|1|"); a != "" {
		t.Fatal("agent choice not cleared")
	}
}

// The record of an approval prompt waiting in a chat outlives the process
// that asked: it is what lets the next one explain a late "yes".
func TestAskedApprovals(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, ok, err := s.Asked(ctx, "tg|1|"); ok || err != nil {
		t.Fatalf("asked from nowhere: %v %v", ok, err)
	}
	if err := s.SaveAsked(ctx, "tg|1|", "aaaa"); err != nil {
		t.Fatal(err)
	}
	s.SaveAsked(ctx, "tg|1|", "bbbb") // a newer prompt replaces the record
	s.SaveAsked(ctx, "tg|2|", "cccc")
	if h, ok, _ := s.Asked(ctx, "tg|1|"); !ok || h != "bbbb" {
		t.Fatalf("asked = %q %v", h, ok)
	}
	if err := s.ForgetAsked(ctx, "tg|1|"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Asked(ctx, "tg|1|"); ok {
		t.Fatal("still on record after it was forgotten")
	}
	if n, err := s.Prune(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("pruned %d, %v; want the one old record", n, err)
	}
	if _, ok, _ := s.Asked(ctx, "tg|2|"); ok {
		t.Fatal("an old record survived pruning")
	}
}
