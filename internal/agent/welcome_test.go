package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeepWelcomeAfterSetupNamesTheAgent(t *testing.T) {
	dir := t.TempDir()
	// Setup names the agent and records the user's name before they meet.
	if err := WriteIdentity(dir, Identity{Name: "Nova"}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "USER.md"), []byte("## Basics\n\n- Name: Ada\n"), 0o600)
	if NeedsOnboarding(dir) {
		t.Fatal("an identity alone should still mean the agent has met its user")
	}
	if err := KeepWelcome(dir); err != nil {
		t.Fatal(err)
	}
	if !NeedsOnboarding(dir) {
		t.Fatal("KeepWelcome did not bring the welcome back")
	}
	// The first chat claims it; after that it is done, as before.
	if !claimOnboarding(dir) || NeedsOnboarding(dir) {
		t.Fatal("the welcome ran more than once")
	}
	prompt := BuildSystemPrompt("habakkuk", dir, "/ws", Snapshot{Onboarding: true})
	for _, want := range []string{"introduce yourself as Nova", "- Name: Nova", "- Name: Ada"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}
