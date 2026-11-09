package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
	"github.com/habakkuk-ai/habakkuk/internal/tools/sandbox"
)

func openWith(t *testing.T, agentExtra string) (*App, string) {
	t.Helper()
	home := t.TempDir()
	ws := filepath.Join(home, "ws")
	cfg := strings.Replace(config.Template, "workspace: ~/habakkuk-workspace", "workspace: "+ws+agentExtra, 1)
	if err := store.WriteFileAtomic(filepath.Join(home, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Open(context.Background(), home, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a, ws
}

func TestBackendsFollowConfig(t *testing.T) {
	a, ws := openWith(t, "\n    sandbox: docker\n    docker:\n      network: false\n      memory: 1g")
	env := a.Runner.Env(a.Runner.Agents["habakkuk"], "s")
	d, ok := env.Backend.(*sandbox.Docker)
	if !ok || d.Workspace != ws || d.Network || d.Memory != "1g" || (d.User == "" && platform.HasUnixPermissions) {
		t.Fatalf("backend %#v", env.Backend)
	}
	if env.Procs == nil || env.Checkpoints == nil {
		t.Fatal("env not equipped")
	}

	t.Setenv("HABAKKUK_TEST_KEY", "-----BEGIN OPENSSH PRIVATE KEY-----\\nabc\\n-----END OPENSSH PRIVATE KEY-----")
	a, _ = openWith(t, "\n    sandbox: ssh\n    ssh:\n      host: h.example\n      workdir: /srv\n      key_env: HABAKKUK_TEST_KEY")
	env = a.Runner.Env(a.Runner.Agents["habakkuk"], "s")
	s, ok := env.Backend.(*sandbox.SSH)
	if !ok || s.Host != "h.example" || s.KeyFile == "" || env.Checkpoints != nil {
		t.Fatalf("ssh backend %#v, checkpoints %v", env.Backend, env.Checkpoints)
	}
	key, err := os.ReadFile(s.KeyFile)
	st, _ := os.Stat(s.KeyFile)
	if err != nil || !strings.Contains(string(key), "\nabc\n") || (platform.HasUnixPermissions && st.Mode().Perm() != 0o600) {
		t.Fatalf("key file %q %v %v", key, err, st.Mode())
	}

	// A missing key never falls back to running commands here.
	a, _ = openWith(t, "\n    sandbox: ssh\n    ssh:\n      host: h.example\n      workdir: /srv\n      key_env: HABAKKUK_TEST_NO_SUCH_KEY")
	env = a.Runner.Env(a.Runner.Agents["habakkuk"], "s")
	if _, _, err := env.Backend.Command(context.Background(), "true", ws, false); err == nil || !env.Backend.Remote() {
		t.Fatalf("broken ssh backend ran a command: %v", err)
	}
	res := a.Tools.Execute(context.Background(), env, llm.Block{Type: llm.ToolUse, ToolUseID: "x", ToolName: "write_file",
		Input: json.RawMessage(`{"path":"a.txt","content":"x"}`)})
	if !res.IsError || !strings.Contains(res.Text, "remote host") {
		t.Fatalf("local write allowed for a remote agent: %+v", res)
	}
}

func TestProcessExitStartsATurn(t *testing.T) {
	a, _ := openWith(t, "")
	ctx := context.Background()
	sess, err := a.Runner.NewSession(ctx, "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.notifyProcess(ctx, sess.ID, "p_abc", "[Background process p_abc exited with code 0]"); err != nil {
		t.Fatal(err)
	}
	// The same process again is the same job.
	if err := a.notifyProcess(ctx, sess.ID, "p_abc", "again"); err != nil {
		t.Fatal(err)
	}
	j, err := a.Queue.Get(ctx, "turn_p_abc")
	if err != nil {
		t.Fatal(err)
	}
	var p agent.TurnPayload
	if err := j.Decode(&p); err != nil || p.Session != sess.ID || !strings.Contains(p.Text, "exited with code 0") {
		t.Fatalf("payload %+v %v", p, err)
	}
}

func TestUndoRestoresTheLastTurn(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	a, ws := openWith(t, "")
	ctx := context.Background()
	sess, err := a.Runner.NewSession(ctx, "habakkuk")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(ws, "f.txt"), []byte("before"), 0o644)
	env := a.Runner.Env(a.Runner.Agents["habakkuk"], sess.ID)
	env.Session, env.Task = sess.ID, "change f"
	res := a.Tools.Execute(ctx, env, llm.Block{Type: llm.ToolUse, ToolUseID: "x", ToolName: "write_file",
		Input: json.RawMessage(`{"path":"f.txt","content":"after"}`)})
	if res.IsError {
		t.Fatal(res.Text)
	}
	msg, err := a.undo(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "f.txt")); string(b) != "before" || !strings.Contains(msg, `"change f"`) || !strings.Contains(msg, "f.txt") {
		t.Fatalf("f.txt %q, reply %q", b, msg)
	}
	if msg, _ := a.undo(ctx, sess.ID); !strings.Contains(msg, "no changes to undo") {
		t.Fatalf("second undo: %q", msg)
	}
}

var _ tools.Backend = unavailable{}
