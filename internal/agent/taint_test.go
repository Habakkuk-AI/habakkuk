package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

func TestHostedSearchTaintsTheTurn(t *testing.T) {
	env := &tools.Env{Taint: tools.NewTaint(false)}
	markHostedContent(env, llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Text, Text: "hi"}}})
	if env.Taint.Marked() {
		t.Fatal("plain text tainted the turn")
	}
	markHostedContent(env, llm.Message{Role: llm.Assistant, Blocks: []llm.Block{{Type: llm.Opaque, Raw: []byte(`{}`)}}})
	if !env.Taint.Marked() {
		t.Fatal("hosted search did not taint the turn")
	}
}

func TestPromptIncludesUserProfile(t *testing.T) {
	dir := t.TempDir()
	if p := BuildSystemPrompt("habakkuk", dir, "/ws", Snapshot{}); strings.Contains(p, "About the user") {
		t.Fatalf("profile section without memory or USER.md:\n%s", p)
	}
	if p := BuildSystemPrompt("habakkuk", dir, "/ws", Snapshot{Memory: true}); !strings.Contains(p, "no profile of the user yet") {
		t.Fatalf("empty profile:\n%s", p)
	}
	os.WriteFile(filepath.Join(dir, "USER.md"), []byte("## Basics\n\n- Name: Ade\n"), 0o600)
	p := BuildSystemPrompt("habakkuk", dir, "/ws", Snapshot{Memory: true})
	if !strings.Contains(p, "<user_profile>\n## Basics\n\n- Name: Ade\n</user_profile>") || !strings.Contains(p, "user_profile_update") {
		t.Fatalf("profile:\n%s", p)
	}
	if strings.Index(p, "# About the user") > strings.Index(p, "# Memory") {
		t.Fatal("profile after memory")
	}
	os.WriteFile(filepath.Join(dir, "USER.md"), []byte(strings.Repeat("é", maxProfileChars)), 0o600)
	if p := BuildSystemPrompt("habakkuk", dir, "/ws", Snapshot{}); !strings.Contains(p, "USER.md is cut") || strings.Contains(p, "user_profile_update") {
		t.Fatal("long profile not cut, or tools named without the memory toolset")
	}
}
