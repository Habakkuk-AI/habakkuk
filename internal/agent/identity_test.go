package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

func hasSpec(specs []llm.ToolSpec, name string) bool {
	for _, s := range specs {
		if s.Name == name {
			return true
		}
	}
	return false
}

func TestIdentityRoundTripAndChecks(t *testing.T) {
	dir := t.TempDir()
	if _, ok := ReadIdentity(dir); ok {
		t.Fatal("identity before any was written")
	}
	if err := WriteIdentity(dir, Identity{Name: " Jarvis ", Emoji: "🦊", Style: "warm,\n brief  and dry"}); err != nil {
		t.Fatal(err)
	}
	id, ok := ReadIdentity(dir)
	if !ok || id.Name != "Jarvis" || id.Emoji != "🦊" || id.Style != "warm, brief and dry" {
		t.Fatalf("read back %+v %v", id, ok)
	}
	for _, bad := range []Identity{
		{},
		{Name: strings.Repeat("x", maxIdentityName+1)},
		{Name: "two\nlines"},
		{Name: "name: injected"},
		{Name: "Ok", Emoji: "abc"},
		{Name: "Ok", Style: strings.Repeat("s", maxIdentityStyle+1)},
	} {
		if err := WriteIdentity(dir, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if id, _ := ReadIdentity(dir); id.Name != "Jarvis" {
		t.Fatalf("a refused write changed the identity: %+v", id)
	}
}

func TestNeedsOnboarding(t *testing.T) {
	fresh := t.TempDir()
	if !NeedsOnboarding(fresh) || NeedsOnboarding("") {
		t.Fatal("fresh agent should need the welcome; no directory should not")
	}
	if !claimOnboarding(fresh) || claimOnboarding(fresh) || NeedsOnboarding(fresh) {
		t.Fatal("the welcome is claimed exactly once")
	}
	if err := ResetOnboarding(fresh); err != nil || !NeedsOnboarding(fresh) {
		t.Fatalf("reset: %v", err)
	}

	profiled := t.TempDir()
	os.WriteFile(filepath.Join(profiled, "USER.md"), []byte("## Basics\nName: Ade"), 0o600)
	if NeedsOnboarding(profiled) {
		t.Fatal("an agent with a user profile (a migrated one) needs no welcome")
	}
	named := t.TempDir()
	if err := WriteIdentity(named, Identity{Name: "Habakkuk"}); err != nil || NeedsOnboarding(named) {
		t.Fatalf("an agent with an identity needs no welcome: %v", err)
	}
}

func TestPromptIdentityAndWelcome(t *testing.T) {
	dir := t.TempDir()
	p := BuildSystemPrompt("habakkuk", dir, "/ws", Snapshot{Onboarding: true, Memory: true})
	for _, want := range []string{"You are Habakkuk,", "# Welcome a new user", "introduce yourself as Habakkuk", "set_identity", "user_profile_update"} {
		if !strings.Contains(p, want) {
			t.Errorf("welcome prompt lacks %q", want)
		}
	}
	if p2 := BuildSystemPrompt("habakkuk", dir, "/ws", Snapshot{Onboarding: true}); strings.Contains(p2, "user_profile_update") {
		t.Error("welcome names the profile tool to an agent without memory")
	}
	if p := BuildSystemPrompt("habakkuk", dir, "/ws", Snapshot{}); strings.Contains(p, "Welcome a new user") {
		t.Error("welcome in an ordinary session")
	}

	WriteIdentity(dir, Identity{Name: "Jarvis", Emoji: "🦊", Style: "brief"})
	p = BuildSystemPrompt("habakkuk", dir, "/ws", Snapshot{})
	for _, want := range []string{"You are Jarvis,", "# Your identity", "- Name: Jarvis", "- Signature emoji: 🦊", "- How to talk: brief"} {
		if !strings.Contains(p, want) {
			t.Errorf("identity prompt lacks %q:\n%s", want, p)
		}
	}
	// A hand-written SOUL.md stays first; the chosen name is stated after it.
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("You are Habakkuk, AY's assistant."), 0o600)
	p = BuildSystemPrompt("habakkuk", dir, "/ws", Snapshot{})
	if !strings.HasPrefix(p, "You are Habakkuk, AY's assistant.") || !strings.Contains(p, "- Name: Jarvis") {
		t.Fatalf("soul and identity:\n%s", p)
	}
}

func TestChatSessionsWelcomeOnce(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &fakeModel{})
	prompt := func(opts SessionOptions) string {
		s, err := h.runner.NewSessionWith(ctx, "habakkuk", opts)
		if err != nil {
			t.Fatal(err)
		}
		return s.SystemPrompt
	}
	if strings.Contains(prompt(SessionOptions{}), "Welcome a new user") {
		t.Fatal("a session that is not a chat (a schedule, a webhook) took the welcome")
	}
	if !strings.Contains(prompt(SessionOptions{Chat: true}), "Welcome a new user") {
		t.Fatal("the first chat has no welcome")
	}
	if strings.Contains(prompt(SessionOptions{Chat: true}), "Welcome a new user") {
		t.Fatal("the welcome ran twice")
	}
	if !strings.Contains(prompt(SessionOptions{Onboard: true}), "Welcome a new user") {
		t.Fatal("an explicit onboarding had no welcome")
	}
}

func TestSetIdentityTool(t *testing.T) {
	h := newHarness(t, &fakeModel{})
	h.runner.RegisterIdentity(h.runner.Tools)
	if specs := h.runner.Tools.Specs([]string{"core"}); !hasSpec(specs, identityToolName) {
		t.Fatal("set_identity is not in the core toolset")
	}
	tool := identityTool{h.runner}
	env := &tools.Env{Agent: "habakkuk"}
	out, err := tool.Run(context.Background(), env, json.RawMessage(`{"name":"Ada","style":"casual"}`))
	if err != nil || !strings.Contains(out, "Ada") {
		t.Fatalf("run: %q %v", out, err)
	}
	if id, _ := ReadIdentity(h.runner.Agents["habakkuk"].Dir); id.Name != "Ada" || id.Style != "casual" {
		t.Fatalf("saved %+v", id)
	}
	if NeedsOnboarding(h.runner.Agents["habakkuk"].Dir) {
		t.Fatal("setting the identity should end the welcome")
	}
	env.Taint = &tools.Taint{}
	env.Taint.Mark()
	if _, err := tool.Run(context.Background(), env, json.RawMessage(`{"name":"Evil"}`)); err == nil {
		t.Fatal("identity changed in a tainted turn")
	}
	if _, err := tool.Run(context.Background(), &tools.Env{Agent: "nobody"}, json.RawMessage(`{"name":"X"}`)); err == nil {
		t.Fatal("unknown agent accepted")
	}
}
