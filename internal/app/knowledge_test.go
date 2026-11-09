package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

func TestNewSessionPromptCarriesKnowledgeSnapshot(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	cfg := strings.Replace(config.Template, "workspace: ~/habakkuk-workspace", "workspace: "+filepath.Join(home, "ws"), 1)
	if err := store.WriteFileAtomic(filepath.Join(home, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(home, "skills", "deploy", "SKILL.md")
	os.MkdirAll(filepath.Dir(skill), 0o700)
	os.WriteFile(skill, []byte("---\nname: deploy\ndescription: Ship the app.\n---\nSteps."), 0o600)

	a, err := Open(ctx, home, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, _, err := a.Memory.Save(ctx, "habakkuk", "Coffee preference", "Flat white.", nil); err != nil {
		t.Fatal(err)
	}
	sess, err := a.Runner.NewSession(ctx, "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Memory", "- Coffee preference (coffee-preference)", "# Skills", "- deploy: Ship the app."} {
		if !strings.Contains(sess.SystemPrompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, sess.SystemPrompt)
		}
	}
	specs := a.Tools.Specs(a.Cfg.Agents["habakkuk"].Toolsets)
	var names []string
	for _, s := range specs {
		names = append(names, s.Name)
	}
	for _, want := range []string{"memory_save", "memory_search", "memory_read", "memory_forget", "skill_view", "skill_file", "skill_save", "read_file"} {
		if !strings.Contains(strings.Join(names, ","), want) {
			t.Errorf("tool %s not offered: %v", want, names)
		}
	}
	// The prompt is frozen: a memory saved later does not change the session.
	a.Memory.Save(ctx, "habakkuk", "Home city", "Toronto", nil)
	again, err := a.Sessions.Get(ctx, sess.ID)
	if err != nil || again.SystemPrompt != sess.SystemPrompt {
		t.Fatalf("frozen prompt changed: %v", err)
	}
}
