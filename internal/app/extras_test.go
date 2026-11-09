package app

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func openTestApp(t *testing.T, extra string) (*App, string, *syncBuf) {
	t.Helper()
	home := t.TempDir()
	cfg := strings.Replace(config.Template, "workspace: ~/habakkuk-workspace", "workspace: "+filepath.Join(home, "ws"), 1) + extra
	if err := store.WriteFileAtomic(filepath.Join(home, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	a, err := Open(context.Background(), home, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a, home, logs
}

func TestExtrasAreWired(t *testing.T) {
	a, home, _ := openTestApp(t, "hooks:\n  - {event: turn_end, command: 'true'}\n")
	if _, err := os.Stat(filepath.Join(home, "state.db")); err != nil {
		t.Fatal("state.db not created")
	}
	if a.Hooks.Count() != 1 || a.Runner.Overlay == nil || a.Runner.Hooks == nil {
		t.Fatal("overlay or hooks not wired")
	}
	var names []string
	for _, s := range a.Tools.Specs(a.Cfg.Agents["habakkuk"].Toolsets) {
		names = append(names, s.Name)
	}
	for _, want := range []string{"todo", "session_search"} {
		if !strings.Contains(strings.Join(names, ","), want) {
			t.Errorf("tool %s not offered: %v", want, names)
		}
	}
	if err := a.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWatchReloadsHooksAndMemory(t *testing.T) {
	ctx := context.Background()
	a, home, logs := openTestApp(t, "")
	w := &watcher{a: a, seen: a.fingerprints()}
	if changed := w.check(ctx); len(changed) != 0 {
		t.Fatalf("changes with nothing touched: %v", changed)
	}

	// A memory note written by hand is indexed without a search.
	note := filepath.Join(home, "agents", "habakkuk", "memory", "boat.md")
	os.MkdirAll(filepath.Dir(note), 0o700)
	os.WriteFile(note, []byte("---\ntitle: Boat name\n---\nThe boat is called Heron."), 0o600)
	if changed := w.check(ctx); strings.Join(changed, ",") != "memory:habakkuk" {
		t.Fatalf("changed = %v", changed)
	}
	var n int
	a.Memory.DB().QueryRowContext(ctx, `SELECT count(*) FROM memories WHERE slug = 'boat'`).Scan(&n)
	if n != 1 {
		t.Fatal("memory not reindexed")
	}

	// Hooks reload in place; other sections ask for a restart.
	path := filepath.Join(home, "config.yaml")
	data, _ := os.ReadFile(path)
	next := strings.Replace(string(data), "effort: high", "effort: low", 1) + "hooks:\n  - {event: turn_start, command: 'true'}\n"
	os.WriteFile(path, []byte(next), 0o600)
	future := time.Now().Add(time.Minute)
	os.Chtimes(path, future, future)
	if changed := w.check(ctx); strings.Join(changed, ",") != "config" {
		t.Fatalf("changed = %v", changed)
	}
	if a.Hooks.Count() != 1 {
		t.Fatal("hooks not reloaded")
	}
	if !strings.Contains(logs.String(), "restart habakkuk serve to apply") || !strings.Contains(logs.String(), "sections=agents") {
		t.Fatalf("no restart notice: %s", logs.String())
	}

	// A broken config is reported and ignored.
	os.WriteFile(path, []byte("version: 1\nnonsense: [\n"), 0o600)
	os.Chtimes(path, future.Add(time.Minute), future.Add(time.Minute))
	w.check(ctx)
	if a.Hooks.Count() != 1 || !strings.Contains(logs.String(), "does not load") {
		t.Fatal("a broken config changed the running one")
	}
}
