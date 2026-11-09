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
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/consolidate"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

func openWithMemory(t *testing.T, section string) *App {
	t.Helper()
	home := t.TempDir()
	cfg := strings.Replace(config.Template, "workspace: ~/habakkuk-workspace", "workspace: "+filepath.Join(home, "ws"), 1)
	cfg = strings.Replace(cfg, "\nworkers:", "\n"+section+"\nworkers:", 1)
	if err := store.WriteFileAtomic(filepath.Join(home, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Open(context.Background(), home, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

func TestMemoryReviewWiring(t *testing.T) {
	a := openWithMemory(t, "memory:\n  review: true\n  nightly: \"04:15\"")
	if a.Consolidate == nil {
		t.Fatal("review on but no consolidator")
	}
	// The cheapest fallback first, then the rest of the chain.
	want := []llm.Target{{Endpoint: "anthropic", Model: "claude-sonnet-5-5"}, {Endpoint: "anthropic", Model: "claude-opus-5-5"}}
	if got := a.Consolidate.Chains["habakkuk"]; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("review chain %v", got)
	}
	if !a.Consolidate.Nightly || a.Consolidate.NightlyAt.Minutes() != 4*60+15 {
		t.Fatalf("nightly %v at %v", a.Consolidate.Nightly, a.Consolidate.NightlyAt)
	}
	var kinds []string
	for _, p := range a.Pools(true) {
		for k := range p.Handlers {
			kinds = append(kinds, k)
		}
	}
	if !strings.Contains(strings.Join(kinds, ","), consolidate.KindReview) {
		t.Fatalf("no worker for reviews: %v", kinds)
	}
	for _, p := range a.Pools(false) {
		if _, ok := p.Handlers[consolidate.KindReview]; ok {
			t.Fatal("reviews run outside habakkuk serve")
		}
	}

	b := openWithMemory(t, "memory:\n  review: true\n  review_model: anthropic/claude-haiku")
	if got := b.Consolidate.Chains["habakkuk"]; got[0].Model != "claude-haiku" || len(got) != 3 {
		t.Fatalf("review_model chain %v", got)
	}
	if c := openWithMemory(t, ""); c.Consolidate != nil || c.Memory.Embedder() != nil {
		t.Fatal("memory extras on without a memory section")
	}
}

func TestRememberAndForgetBackend(t *testing.T) {
	ctx := context.Background()
	a := openWithMemory(t, "")
	m := memoryCommands{a.Memory}
	out, err := m.Remember(ctx, "habakkuk", "ses_1", "My passport is in the blue folder, top drawer.\nRenewal due 2028.")
	if err != nil || !strings.Contains(out, "my-passport-is-in-the-blue-folder-top") {
		t.Fatalf("remember: %q %v", out, err)
	}
	note, err := a.Memory.Read(ctx, "habakkuk", "my-passport-is-in-the-blue-folder-top")
	if err != nil || note.Source != "user" || note.Session != "ses_1" || !strings.Contains(note.Body, "Renewal due 2028") {
		t.Fatalf("note: %+v %v", note, err)
	}
	// The same words again make a second note rather than replacing.
	if out, _ := m.Remember(ctx, "habakkuk", "ses_1", "My passport is in the blue folder, top drawer."); !strings.Contains(out, "-2`") {
		t.Fatalf("second remember: %q", out)
	}
	if _, err := m.Remember(ctx, "habakkuk", "ses_1", "github token ghp_0123456789abcdefghijABCDEFGHIJ012345"); err == nil {
		t.Fatal("secret remembered")
	}
	a.Memory.Save(ctx, "habakkuk", "Coffee preference", "Flat white.", nil)

	if out, _ := m.Forget(ctx, "habakkuk", "passport"); !strings.Contains(out, "Several memories match") {
		t.Fatalf("ambiguous forget: %q", out)
	}
	if out, _ := m.Forget(ctx, "habakkuk", "my-passport-is-in-the-blue-folder-top-2"); !strings.Contains(out, "Forgot `my-passport") {
		t.Fatalf("forget by slug: %q", out)
	}
	if out, _ := m.Forget(ctx, "habakkuk", "flat white"); !strings.Contains(out, `Forgot "Coffee preference"`) {
		t.Fatalf("forget by query: %q", out)
	}
	if out, _ := m.Forget(ctx, "habakkuk", "bicycle"); out != "No memory matches that." {
		t.Fatalf("forget nothing: %q", out)
	}
	trash, _ := filepath.Glob(filepath.Join(a.Memory.Dir("habakkuk"), ".trash", "*.md"))
	if len(trash) != 2 {
		t.Fatalf("trash %v", trash)
	}
}

func TestProfileInNewSessionPrompt(t *testing.T) {
	ctx := context.Background()
	a := openWithMemory(t, "")
	if _, err := a.Memory.UpdateProfile(ctx, "habakkuk", "Basics", "- Name: Ade", "append"); err != nil {
		t.Fatal(err)
	}
	sess, err := a.Runner.NewSession(ctx, "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sess.SystemPrompt, "# About the user") || !strings.Contains(sess.SystemPrompt, "<user_profile>\n## Basics\n\n- Name: Ade\n</user_profile>") {
		t.Fatalf("prompt:\n%s", sess.SystemPrompt)
	}
	if _, err := os.Stat(a.Memory.ProfilePath("habakkuk")); err != nil {
		t.Fatal(err)
	}
}
