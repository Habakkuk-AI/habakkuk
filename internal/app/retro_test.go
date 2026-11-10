package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/retro"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

type retroModel struct {
	mu     sync.Mutex
	chains [][]llm.Target
	answer string
}

func (m *retroModel) Send(_ context.Context, chain []llm.Target, _ llm.Request, _ func(llm.Event)) (*llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chains = append(m.chains, chain)
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: m.answer}}}}, nil
}

func TestRetroWiring(t *testing.T) {
	a := openWithMemory(t, "retro:\n  every: weekly\n  at: \"mon 09:00\"\n  model: anthropic/claude-sonnet-5-5\n  apply: safe\n  deliver: none")
	r := a.Retro
	if r == nil || r.Spec != "0 9 * * 1" || r.Apply != retro.ApplySafe {
		t.Fatalf("runner %+v", r)
	}
	ag := r.Agents["habakkuk"]
	// retro.model first, then the agent's own chain without repeating it.
	if len(ag.Chain) != 2 || ag.Chain[0].Model != "claude-sonnet-5-5" || ag.Chain[1].Model != "claude-opus-5-5" {
		t.Fatalf("chain %v", ag.Chain)
	}
	if len(ag.Models) != 1 || ag.Models[0] != "anthropic/claude-opus-5-5" || ag.Deliver != "" {
		t.Fatalf("agent %+v", ag)
	}
	var found bool
	for _, p := range a.Pools(true) {
		if _, ok := p.Handlers[retro.Kind]; ok {
			found = true
		}
	}
	if !found || !a.serving {
		t.Fatal("no retro pool in the serving process")
	}
}

func TestRetroChatCommand(t *testing.T) {
	ctx := context.Background()
	a := openWithMemory(t, "")
	if a.Retro.Spec != "" {
		t.Fatalf("retros on without a section: %q", a.Retro.Spec)
	}
	m := &retroModel{answer: `{"summary": "A note would help.", "went_badly": [{"what": "x", "cause": "y", "evidence": ["session:SESSION"]}],
		"actions": [{"kind": "memory", "target": "Retro test note", "change": "Remember this.", "why": "it helps", "risk": "low"},
		            {"kind": "config", "target": "agents.habakkuk.effort", "change": "medium", "why": "slow turns", "risk": "low"}]}`}
	a.Retro.Models = m
	sess, err := a.Sessions.Create(ctx, "habakkuk", "system")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Sessions.Append(ctx, sess.ID, "turn_x", 0, llm.UserText("hello"), "", nil); err != nil {
		t.Fatal(err)
	}
	m.answer = strings.ReplaceAll(m.answer, "SESSION", sess.ID)
	cmd := a.ChatCommands()["/retro"]
	if !cmd.OwnerOnly || !cmd.Async {
		t.Fatalf("/retro %+v", cmd)
	}
	out := cmd.Run(ctx, sess.ID, "")
	if !strings.Contains(out, "**Retro for habakkuk**") || !strings.Contains(out, "1. memory Retro test note") || !strings.Contains(out, "agents/habakkuk/retros/") {
		t.Fatalf("/retro:\n%s", out)
	}
	if out := cmd.Run(ctx, sess.ID, "apply 1 2"); !strings.Contains(out, "1. memory Retro test note: applied") || !strings.Contains(out, "2. config agents.habakkuk.effort: approved") {
		t.Fatalf("/retro apply:\n%s", out)
	}
	if _, err := a.Memory.Read(ctx, "habakkuk", "retro-test-note"); err != nil {
		t.Fatal(err)
	}
	if out := cmd.Run(ctx, sess.ID, "reject 1"); !strings.Contains(out, "already applied") {
		t.Fatalf("/retro reject an applied one: %s", out)
	}
	if out := cmd.Run(ctx, sess.ID, "dance"); !strings.HasPrefix(out, "Usage: /retro") {
		t.Fatalf("usage: %s", out)
	}
	// The report is on disk, and config.yaml was not touched.
	ret, _ := a.Retro.Resolve(ctx, "habakkuk", "latest")
	if _, err := os.Stat(filepath.Join(a.Cfg.Home, filepath.FromSlash(ret.Report))); err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(filepath.Join(a.Cfg.Home, "config.yaml"))
	if strings.Contains(string(cfg), "effort: medium") {
		t.Fatal("config.yaml edited")
	}
}
