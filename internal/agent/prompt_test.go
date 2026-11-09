package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromptWithoutKnowledgeHasNoSections(t *testing.T) {
	p := BuildSystemPrompt("habakkuk", t.TempDir(), "/ws", Snapshot{})
	if strings.Contains(p, "# Memory") || strings.Contains(p, "# Skills") {
		t.Fatalf("sections without tools:\n%s", p)
	}
}

func TestPromptSnapshotContents(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("You are Habakkuk."), 0o600)
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Be brief."), 0o600)
	snap := Snapshot{
		Memory:   true,
		Memories: []MemoryRef{{Title: "Coffee preference", Slug: "coffee-preference"}, {Title: "Home city", Slug: "home-city", Untrusted: true}},
		MemTotal: 2,
		Skills:   true,
		SkillSet: []SkillRef{{"weekly-report", "Write the weekly report."}, {"deploy", "Ship the app."}, {"bare", ""}},
	}
	p := BuildSystemPrompt("habakkuk", dir, "/ws", snap)
	for _, want := range []string{
		"You are Habakkuk.", "# Standing instructions\n\nBe brief.",
		"# Memory", "memory_search", "memory_save",
		"- Coffee preference (coffee-preference)\n- Home city (home-city) [untrusted]",
		"# Skills", "skill_view",
		"- bare: (no description)\n- deploy: Ship the app.\n- weekly-report: Write the weekly report.",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	if strings.Index(p, "# Memory") > strings.Index(p, "# Skills") || strings.Index(p, "Be brief.") > strings.Index(p, "# Memory") {
		t.Error("sections out of order")
	}
	if strings.Contains(p, "more not listed") {
		t.Error("overflow note without overflow")
	}

	empty := BuildSystemPrompt("habakkuk", dir, "/ws", Snapshot{Memory: true, Skills: true})
	if !strings.Contains(empty, "no saved memories yet") || !strings.Contains(empty, "No skills are installed yet") {
		t.Fatalf("empty listings:\n%s", empty)
	}
}

func TestPromptCaps(t *testing.T) {
	snap := Snapshot{Memory: true, Skills: true, MemTotal: 500}
	for i := range 200 {
		snap.Memories = append(snap.Memories, MemoryRef{Title: fmt.Sprintf("Memory %03d %s", i, strings.Repeat("x", 300)), Slug: fmt.Sprintf("memory-%03d", i)})
		snap.SkillSet = append(snap.SkillSet, SkillRef{Name: fmt.Sprintf("skill-%03d", i), Description: strings.Repeat("d", 1000)})
	}
	p := BuildSystemPrompt("habakkuk", t.TempDir(), "/ws", snap)
	mem := p[strings.Index(p, "# Memory"):strings.Index(p, "# Skills")]
	skills := p[strings.Index(p, "# Skills"):]
	if n := strings.Count(mem, "\n- "); n == 0 || n > maxMemoryTitles {
		t.Fatalf("listed %d memories", n)
	}
	if len(mem) > maxMemoryBytes+len(memoryInstructions)+300 {
		t.Fatalf("memory section is %d bytes", len(mem))
	}
	if !strings.Contains(mem, "more not listed; find them with memory_search") {
		t.Fatal("no overflow note for memories")
	}
	if strings.Contains(mem, strings.Repeat("x", maxTitleChars)) {
		t.Fatal("long title not clipped")
	}
	if n := strings.Count(skills, "\n- "); n == 0 || n > maxSkills {
		t.Fatalf("listed %d skills", n)
	}
	if len(skills) > maxSkillsBytes+len(skillInstructions)+300 {
		t.Fatalf("skills section is %d bytes", len(skills))
	}
	if !strings.Contains(skills, "more skills are installed but not listed") {
		t.Fatal("no overflow note for skills")
	}
	// The memories listed are the first (most recent) ones.
	if !strings.Contains(mem, "(memory-000)") || strings.Contains(mem, "(memory-199)") {
		t.Fatal("wrong memories kept")
	}
}

func TestPromptIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("You are Habakkuk."), 0o600)
	a := Snapshot{
		Memory: true, Memories: []MemoryRef{{Title: "B", Slug: "b"}, {Title: "A", Slug: "a"}}, MemTotal: 2,
		Skills: true, SkillSet: []SkillRef{{"zeta", "z"}, {"alpha", "a"}, {"mid", "m"}},
	}
	// Same skills in another order (directory listing order is not stable).
	b := a
	b.SkillSet = []SkillRef{{"mid", "m"}, {"zeta", "z"}, {"alpha", "a"}}
	first := BuildSystemPrompt("habakkuk", dir, "/ws", a)
	for range 5 {
		if got := BuildSystemPrompt("habakkuk", dir, "/ws", b); got != first {
			t.Fatalf("prompt changed:\n%s\n---\n%s", first, got)
		}
	}
	if a.SkillSet[0].Name != "zeta" {
		t.Fatal("BuildSystemPrompt reordered the caller's slice")
	}
}
