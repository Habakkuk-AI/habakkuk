package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

func rig(t *testing.T) (*Store, string) {
	t.Helper()
	needGit(t)
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	os.MkdirAll(ws, 0o755)
	return &Store{Root: filepath.Join(dir, "checkpoints")}, ws
}

func put(t *testing.T, ws string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(ws, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func get(ws, p string) string {
	b, err := os.ReadFile(filepath.Join(ws, p))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func TestRestoreWholeWorkspace(t *testing.T) {
	s, ws := rig(t)
	ctx := context.Background()
	put(t, ws, map[string]string{"a.txt": "a1", "dir/b.txt": "b1", ".gitignore": "*.tmp\n", "scratch.tmp": "t1"})
	cp, err := s.Snapshot(ctx, "habakkuk", ws, Meta{Kind: KindTurn, Session: "s1", Task: "edit things\nmore"})
	if err != nil {
		t.Fatal(err)
	}
	if cp.Kind != KindTurn || cp.Session != "s1" || cp.Task != "edit things" || len(cp.ID) != 10 {
		t.Fatalf("checkpoint %+v", cp)
	}
	// The turn edits, adds and deletes.
	put(t, ws, map[string]string{"a.txt": "a2", "new.txt": "n", "scratch.tmp": "t2"})
	os.Remove(filepath.Join(ws, "dir/b.txt"))

	stat, err := s.DiffStat(ctx, "habakkuk", ws, cp.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a.txt", "dir/b.txt", "new.txt"} {
		if !strings.Contains(stat, want) {
			t.Fatalf("stat lacks %s:\n%s", want, stat)
		}
	}
	if strings.Contains(stat, "scratch.tmp") {
		t.Fatalf("ignored file in stat:\n%s", stat)
	}
	if _, err := s.Restore(ctx, "habakkuk", ws, cp.ID, "", Meta{Kind: KindRollback}); err != nil {
		t.Fatal(err)
	}
	if get(ws, "a.txt") != "a1" || get(ws, "dir/b.txt") != "b1" || get(ws, "new.txt") != "<missing>" {
		t.Fatalf("not restored: a=%q b=%q new=%q", get(ws, "a.txt"), get(ws, "dir/b.txt"), get(ws, "new.txt"))
	}
	if get(ws, "scratch.tmp") != "t2" {
		t.Fatal("an ignored file was touched")
	}
	// The replaced state is a checkpoint, so the rollback can be undone.
	list, err := s.List(ctx, "habakkuk", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Kind != KindRollback || !strings.HasPrefix(list[0].Undoes, cp.ID) {
		t.Fatalf("list %+v", list)
	}
	if _, err := s.Restore(ctx, "habakkuk", ws, list[0].ID, "", Meta{Kind: KindRollback}); err != nil {
		t.Fatal(err)
	}
	if get(ws, "a.txt") != "a2" || get(ws, "new.txt") != "n" || get(ws, "dir/b.txt") != "<missing>" {
		t.Fatal("rolling back the rollback failed")
	}
}

func TestRestoreOnePath(t *testing.T) {
	s, ws := rig(t)
	ctx := context.Background()
	put(t, ws, map[string]string{"a.txt": "a1", "b.txt": "b1"})
	cp, err := s.Snapshot(ctx, "habakkuk", ws, Meta{Kind: KindTurn})
	if err != nil {
		t.Fatal(err)
	}
	put(t, ws, map[string]string{"a.txt": "a2", "b.txt": "b2"})
	stat, err := s.DiffStat(ctx, "habakkuk", ws, cp.ID, "a.txt")
	if err != nil || !strings.Contains(stat, "a.txt") || strings.Contains(stat, "b.txt") {
		t.Fatalf("stat %q %v", stat, err)
	}
	if _, err := s.Restore(ctx, "habakkuk", ws, cp.ID, filepath.Join(ws, "a.txt"), Meta{Kind: KindRollback}); err != nil {
		t.Fatal(err)
	}
	if get(ws, "a.txt") != "a1" || get(ws, "b.txt") != "b2" {
		t.Fatalf("a=%q b=%q", get(ws, "a.txt"), get(ws, "b.txt"))
	}
	if _, err := s.Restore(ctx, "habakkuk", ws, cp.ID, filepath.Join(t.TempDir(), "passwd"), Meta{Kind: KindRollback}); err == nil {
		t.Fatal("restored a path outside the workspace")
	}
	if _, err := s.Restore(ctx, "habakkuk", ws, "zzzz", "", Meta{Kind: KindRollback}); err == nil {
		t.Fatal("accepted a bad id")
	}
}

func TestUserGitAndNestedReposAreLeftAlone(t *testing.T) {
	s, ws := rig(t)
	ctx := context.Background()
	gitIn := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	put(t, ws, map[string]string{"own.txt": "1", "sub/inner.txt": "i1"})
	gitIn(ws, "init", "-q")
	gitIn(ws, "add", "own.txt")
	gitIn(ws, "commit", "-qm", "user commit")
	gitIn(filepath.Join(ws, "sub"), "init", "-q")
	head := get(ws, ".git/HEAD")
	index, _ := os.ReadFile(filepath.Join(ws, ".git/index"))

	cp, err := s.Snapshot(ctx, "habakkuk", ws, Meta{Kind: KindTurn, Session: "s"})
	if err != nil {
		t.Fatal(err)
	}
	put(t, ws, map[string]string{"own.txt": "2", "sub/inner.txt": "i2"})
	if _, err := s.Restore(ctx, "habakkuk", ws, cp.ID, "", Meta{Kind: KindRollback}); err != nil {
		t.Fatal(err)
	}
	if get(ws, "own.txt") != "1" {
		t.Fatal("workspace file not restored")
	}
	if get(ws, "sub/inner.txt") != "i2" {
		t.Fatal("a nested repository was touched")
	}
	index2, _ := os.ReadFile(filepath.Join(ws, ".git/index"))
	if get(ws, ".git/HEAD") != head || string(index) != string(index2) {
		t.Fatal("the user's .git changed")
	}
}

func TestSizeCaps(t *testing.T) {
	s, ws := rig(t)
	ctx := context.Background()
	s.MaxFileBytes, s.MaxBytes = 100, 150
	put(t, ws, map[string]string{"small.txt": "s1", "big.bin": strings.Repeat("x", 200)})
	cp, err := s.Snapshot(ctx, "habakkuk", ws, Meta{Kind: KindTurn})
	if err != nil {
		t.Fatal(err)
	}
	put(t, ws, map[string]string{"small.txt": "s2", "big.bin": strings.Repeat("y", 200)})
	if _, err := s.Restore(ctx, "habakkuk", ws, cp.ID, "", Meta{Kind: KindRollback}); err != nil {
		t.Fatal(err)
	}
	if get(ws, "small.txt") != "s1" || get(ws, "big.bin") != strings.Repeat("y", 200) {
		t.Fatal("caps not applied: the big file was snapshotted or the small one not")
	}
	put(t, ws, map[string]string{"m1.txt": strings.Repeat("a", 90), "m2.txt": strings.Repeat("b", 90)})
	if _, err := s.Snapshot(ctx, "habakkuk", ws, Meta{Kind: KindTurn}); !errors.Is(err, ErrTooBig) {
		t.Fatalf("total cap: %v", err)
	}
	s.AgentMaxBytes = func(string) int64 { return 1000 }
	if _, err := s.Snapshot(ctx, "habakkuk", ws, Meta{Kind: KindTurn}); err != nil {
		t.Fatalf("per-agent cap: %v", err)
	}
}

func TestUndoWalksBackThroughTurns(t *testing.T) {
	s, ws := rig(t)
	ctx := context.Background()
	if _, _, err := s.Undo(ctx, "habakkuk", ws, "s1"); !errors.Is(err, ErrNothing) {
		t.Fatalf("undo with no checkpoints: %v", err)
	}
	put(t, ws, map[string]string{"f.txt": "v0"})
	for i, v := range []string{"v1", "v2"} {
		if _, err := s.Snapshot(ctx, "habakkuk", ws, Meta{Kind: KindTurn, Session: "s1", Task: "turn " + v}); err != nil {
			t.Fatal(i, err)
		}
		put(t, ws, map[string]string{"f.txt": v})
	}
	// Another session's turn is not undone from s1.
	if _, err := s.Snapshot(ctx, "habakkuk", ws, Meta{Kind: KindTurn, Session: "s2"}); err != nil {
		t.Fatal(err)
	}
	cp, stat, err := s.Undo(ctx, "habakkuk", ws, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if get(ws, "f.txt") != "v1" || cp.Task != "turn v2" || !strings.Contains(stat, "f.txt") {
		t.Fatalf("first undo: f=%q cp=%+v stat=%q", get(ws, "f.txt"), cp, stat)
	}
	if _, _, err := s.Undo(ctx, "habakkuk", ws, "s1"); err != nil {
		t.Fatal(err)
	}
	if get(ws, "f.txt") != "v0" {
		t.Fatalf("second undo: %q", get(ws, "f.txt"))
	}
	if _, _, err := s.Undo(ctx, "habakkuk", ws, "s1"); !errors.Is(err, ErrNothing) {
		t.Fatalf("third undo: %v", err)
	}
}

func TestNoGit(t *testing.T) {
	s := &Store{Root: t.TempDir(), Git: ""}
	t.Setenv("PATH", t.TempDir())
	if _, err := s.Snapshot(context.Background(), "habakkuk", t.TempDir(), Meta{Kind: KindTurn}); !errors.Is(err, ErrNoGit) {
		t.Fatalf("err = %v", err)
	}
}

// counter counts snapshots requested through the registry.
type counter struct{ n atomic.Int32 }

func (c *counter) Checkpoint(context.Context, *tools.Env) { c.n.Add(1) }

func TestRegistryCheckpointsOncePerTurnBeforeChanges(t *testing.T) {
	r := tools.NewRegistry()
	tools.RegisterBuiltins(r)
	ws := t.TempDir()
	c := &counter{}
	env := &tools.Env{Workspace: ws, Mode: tools.ModeFull, Checkpoints: tools.OncePerTurn(c)}
	exec := func(name string, input any) {
		b, _ := json.Marshal(input)
		if res := r.Execute(context.Background(), env, llm.Block{Type: llm.ToolUse, ToolUseID: "x", ToolName: name, Input: b}); res.IsError {
			t.Fatalf("%s: %s", name, res.Text)
		}
	}
	elsewhere := t.TempDir() // outside the workspace, on every OS
	os.WriteFile(filepath.Join(elsewhere, "hosts"), []byte("127.0.0.1 localhost\n"), 0o644)
	exec("read_file", map[string]any{"path": filepath.Join(elsewhere, "hosts")})
	exec("list_dir", map[string]any{})
	exec("write_file", map[string]any{"path": filepath.Join(t.TempDir(), "out.txt"), "content": "x"}) // outside the workspace
	if c.n.Load() != 0 {
		t.Fatal("checkpointed before any workspace change")
	}
	exec("write_file", map[string]any{"path": "a.txt", "content": "x"})
	exec("edit_file", map[string]any{"path": "a.txt", "old_string": "x", "new_string": "y"})
	exec("exec", map[string]any{"command": "exit 0"})
	if c.n.Load() != 1 {
		t.Fatalf("checkpoints in one turn: %d", c.n.Load())
	}
	// A new turn gets a fresh wrapper; exec in the workspace counts.
	env.Checkpoints = tools.OncePerTurn(c)
	exec("exec", map[string]any{"command": "exit 0"})
	if c.n.Load() != 2 {
		t.Fatalf("exec did not checkpoint: %d", c.n.Load())
	}
	exec("exec", map[string]any{"command": "true", "cwd": elsewhere})

	env.Checkpoints = tools.OncePerTurn(c)
	exec("exec", map[string]any{"command": "true", "cwd": elsewhere})

	if c.n.Load() != 2 {
		t.Fatal("exec outside the workspace checkpointed")
	}
}

func TestStoreImplementsCheckpointer(t *testing.T) {
	s, ws := rig(t)
	s.Enabled = func(agent string) bool { return agent == "on" }
	put(t, ws, map[string]string{"a": "1"})
	s.Checkpoint(context.Background(), &tools.Env{Agent: "off", Workspace: ws})
	s.Checkpoint(context.Background(), &tools.Env{Agent: "on", Workspace: ws, Session: "s", Task: "do it"})
	if l, _ := s.List(context.Background(), "off", 0); len(l) != 0 {
		t.Fatal("disabled agent checkpointed")
	}
	l, _ := s.List(context.Background(), "on", 0)
	if len(l) != 1 || l[0].Session != "s" || l[0].Task != "do it" {
		t.Fatalf("list %+v", l)
	}
}
