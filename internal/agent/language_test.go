package agent

import (
	"strings"
	"testing"
)

func TestPromptAnswersInTheUsersLanguage(t *testing.T) {
	p := BuildSystemPrompt("habakkuk", t.TempDir(), "/ws", Snapshot{})
	if !strings.Contains(p, "Answer in the language the user writes in") {
		t.Fatal("prompt does not tell the model to answer in the user's language")
	}
	if strings.Contains(p, "as Habakkuk's language") {
		t.Fatal("no configured language, but the prompt names one")
	}
	p = BuildSystemPrompt("habakkuk", t.TempDir(), "/ws", Snapshot{Language: "French"})
	if !strings.Contains(p, "The owner set French as Habakkuk's language") {
		t.Fatalf("configured language missing:\n%s", p)
	}
}
