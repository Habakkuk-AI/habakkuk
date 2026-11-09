package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

type answer bool

func (a answer) Approve(context.Context, ApprovalRequest) (bool, error) { return bool(a), nil }

func setup(t *testing.T, mode Mode, approver Approver) (*Registry, *Env) {
	t.Helper()
	r := NewRegistry()
	RegisterBuiltins(r)
	dir := t.TempDir()
	blobs, _ := store.NewBlobs(filepath.Join(dir, "blobs"))
	ws := filepath.Join(dir, "ws")
	os.MkdirAll(ws, 0o755)
	return r, &Env{Workspace: ws, Mode: mode, Approver: approver, Blobs: blobs, BlobDir: filepath.Join(dir, "blobs")}
}

// byOS picks the sh or the PowerShell spelling of a test command: exec runs
// PowerShell on Windows.
func byOS(sh, powershell string) string {
	if runtime.GOOS == "windows" {
		return powershell
	}
	return sh
}

func call(name string, input any) llm.Block {
	b, _ := json.Marshal(input)
	return llm.Block{Type: llm.ToolUse, ToolUseID: "t1", ToolName: name, Input: b}
}

func TestPermissionMatrix(t *testing.T) {
	inside := map[string]any{"path": "a.txt", "content": "x"}
	outside := map[string]any{"path": filepath.Join(os.TempDir(), "habakkuk-test-outside-"+time.Now().Format("150405.000")), "content": "x"}
	defer os.Remove(outside["path"].(string))
	cases := []struct {
		mode     Mode
		approver Approver
		tool     string
		input    any
		allowed  bool
	}{
		{ModeReadOnly, answer(true), "write_file", inside, false},
		{ModeReadOnly, nil, "list_dir", map[string]any{}, true},
		{ModeAsk, nil, "write_file", inside, false},
		{ModeAsk, answer(true), "write_file", inside, true},
		{ModeAsk, answer(false), "write_file", inside, false},
		{ModeWorkspace, nil, "write_file", inside, true},
		{ModeWorkspace, nil, "write_file", outside, false},
		{ModeWorkspace, answer(true), "write_file", outside, true},
		{ModeWorkspace, nil, "exec", map[string]any{"command": "true"}, false},
		{ModeWorkspace, answer(true), "exec", map[string]any{"command": "true"}, true},
		{ModeFull, nil, "exec", map[string]any{"command": "true"}, true},
	}
	for _, c := range cases {
		r, env := setup(t, c.mode, c.approver)
		res := r.Execute(context.Background(), env, call(c.tool, c.input))
		denied := strings.Contains(res.Text, "denied by permission policy")
		if denied == c.allowed {
			t.Errorf("mode=%s approver=%v tool=%s: allowed=%v, got result %q", c.mode, c.approver, c.tool, c.allowed, res.Text)
		}
	}
}

func TestEditFileRequiresUniqueMatch(t *testing.T) {
	r, env := setup(t, ModeFull, nil)
	p := filepath.Join(env.Workspace, "f.txt")
	os.WriteFile(p, []byte("a b a"), 0o644)
	res := r.Execute(context.Background(), env, call("edit_file", map[string]any{"path": "f.txt", "old_string": "a", "new_string": "z"}))
	if !res.IsError || !strings.Contains(res.Text, "matches 2 times") {
		t.Fatalf("ambiguous edit: %q", res.Text)
	}
	res = r.Execute(context.Background(), env, call("edit_file", map[string]any{"path": "f.txt", "old_string": "a b", "new_string": "z"}))
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got, _ := os.ReadFile(p); string(got) != "z a" {
		t.Fatalf("file = %q", got)
	}
}

func TestExecTimeoutKillsProcessTree(t *testing.T) {
	r, env := setup(t, ModeFull, nil)
	start := time.Now()
	res := r.Execute(context.Background(), env, call("exec", map[string]any{"command": byOS("sleep 30 & sleep 30", "Start-Sleep 30"), "timeout_seconds": 1}))
	if !res.IsError || !strings.Contains(res.Text, "timed out") {
		t.Fatalf("result %q", res.Text)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("timeout did not kill the process group")
	}
}

func TestLargeOutputGoesToBlob(t *testing.T) {
	r, env := setup(t, ModeFull, nil)
	r.MaxInline = 1000
	res := r.Execute(context.Background(), env, call("exec", map[string]any{"command": byOS("seq 1 5000", "1..5000")}))
	if len(res.Text) > 1500 || !strings.Contains(res.Text, "full output saved at") {
		t.Fatalf("len %d: %q", len(res.Text), res.Text[:200])
	}
}

func TestInvalidJSONInputIsRejected(t *testing.T) {
	r, env := setup(t, ModeFull, nil)
	res := r.Execute(context.Background(), env, llm.Block{ToolUseID: "x", ToolName: "exec", Input: json.RawMessage(`{"command": "ec`)})
	if !res.IsError || !strings.Contains(res.Text, "not valid JSON") {
		t.Fatalf("result %q", res.Text)
	}
}

func TestSpecsAreSorted(t *testing.T) {
	r, _ := setup(t, ModeFull, nil)
	specs := r.Specs([]string{"core"})
	for i := 1; i < len(specs); i++ {
		if specs[i-1].Name > specs[i].Name {
			t.Fatalf("specs not sorted: %s before %s", specs[i-1].Name, specs[i].Name)
		}
	}
}

type fixed struct {
	name, desc string
}

func (f fixed) Spec() llm.ToolSpec { return llm.ToolSpec{Name: f.name, Description: f.desc} }
func (fixed) Risk() Risk           { return RiskRead }
func (f fixed) Run(context.Context, *Env, json.RawMessage) (string, error) {
	return f.desc, nil
}

type callIDEcho struct{}

func (callIDEcho) Spec() llm.ToolSpec { return llm.ToolSpec{Name: "echo_id"} }
func (callIDEcho) Risk() Risk         { return RiskRead }
func (callIDEcho) Run(ctx context.Context, _ *Env, _ json.RawMessage) (string, error) {
	return CallID(ctx), nil
}

func TestToolsSeeTheirCallID(t *testing.T) {
	r := NewRegistry()
	r.Register("x", callIDEcho{})
	if res := r.Execute(context.Background(), &Env{}, call("echo_id", map[string]any{})); res.Text != "t1" {
		t.Fatalf("call id = %q", res.Text)
	}
}

func TestRegisterOrReplace(t *testing.T) {
	r := NewRegistry()
	r.RegisterOrReplace("mcp:x", fixed{"b", "one"})
	r.RegisterOrReplace("mcp:x", fixed{"a", "one"})
	r.RegisterOrReplace("mcp:x", fixed{"b", "two"})
	specs := r.Specs([]string{"mcp:x", "mcp:x"})
	if len(specs) != 2 || specs[0].Name != "a" || specs[1].Description != "two" {
		t.Fatalf("specs: %+v", specs)
	}
	if res := r.Execute(context.Background(), &Env{Mode: ModeFull}, call("b", map[string]any{})); res.Text != "two" {
		t.Fatalf("replaced tool not used: %+v", res)
	}
}
