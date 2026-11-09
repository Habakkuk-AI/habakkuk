package hooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/testutil"
)

func TestToolBeforeVetoes(t *testing.T) {
	testutil.UnixOnly(t, "hook commands are sh scripts (grep, &&)")
	r := New([]*config.Hook{
		{Event: "tool_before", Match: "^exec$", Command: `grep -q '"rm' && { echo "no deleting"; exit 1; }; exit 0`},
		{Event: "tool_before", Match: "^write_file$", Command: "exit 3"},
	}, t.TempDir(), nil)
	ctx := context.Background()
	if v := r.Fire(ctx, "tool_before", "exec", map[string]any{"input": map[string]string{"command": "ls"}}); v != "" {
		t.Fatalf("harmless call vetoed: %q", v)
	}
	if v := r.Fire(ctx, "tool_before", "exec", map[string]any{"input": map[string]string{"command": "rm -rf x"}}); v != "no deleting" {
		t.Fatalf("veto = %q", v)
	}
	if v := r.Fire(ctx, "tool_before", "read_file", nil); v != "" {
		t.Fatalf("unmatched tool vetoed: %q", v)
	}
	if v := r.Fire(ctx, "tool_before", "write_file", nil); !strings.Contains(v, "exit status 3") {
		t.Fatalf("silent failure should still veto with a reason: %q", v)
	}
}

func TestTimeoutVetoesAndKillsTheGroup(t *testing.T) {
	testutil.UnixOnly(t, "hook command backgrounds a child with sh &")
	r := New([]*config.Hook{{Event: "tool_before", Command: "sleep 5 & sleep 5", Timeout: 100 * time.Millisecond}}, "", nil)
	start := time.Now()
	v := r.Fire(context.Background(), "tool_before", "exec", nil)
	if !strings.Contains(v, "timed out") {
		t.Fatalf("veto = %q", v)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout did not stop the hook")
	}
}

func TestBackgroundHooksGetPayloadAndEnv(t *testing.T) {
	testutil.UnixOnly(t, "hook command is an sh script (printf, cat, $VAR)")
	home := t.TempDir()
	out := filepath.Join(home, "out")
	r := New([]*config.Hook{
		{Event: "turn_end", Match: "hab", Command: `{ printf '%s ' "$HABAKKUK_EVENT"; cat; } > ` + out},
		{Event: "turn_end", Match: "^scout$", Command: "touch " + filepath.Join(home, "wrong")},
	}, home, nil)
	if r.Count() != 2 {
		t.Fatalf("count = %d", r.Count())
	}
	v := r.Fire(context.Background(), "turn_end", "habakkuk", map[string]string{"final": "done"})
	if v != "" {
		t.Fatal("a background event vetoed")
	}
	r.Wait()
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	ev, body, _ := strings.Cut(string(data), " ")
	var p map[string]string
	if ev != "turn_end" || json.Unmarshal([]byte(body), &p) != nil || p["final"] != "done" {
		t.Fatalf("hook saw %q", data)
	}
	if _, err := os.Stat(filepath.Join(home, "wrong")); err == nil {
		t.Fatal("a hook ran for a subject its match rejects")
	}

	r.Set(nil) // hot reload to no hooks
	if r.Count() != 0 {
		t.Fatal("hooks not replaced")
	}
	var nilRunner *Runner
	if nilRunner.Fire(context.Background(), "tool_before", "x", nil) != "" {
		t.Fatal("nil runner vetoed")
	}
}

func TestConfigValidatesHooks(t *testing.T) {
	base := "version: 1\nproviders: {a: {type: anthropic, keys_env: [K]}}\nagents: {habakkuk: {model: a/m}}\n"
	if _, err := config.Parse([]byte(base + "hooks:\n  - {event: tool_before, command: 'exit 0', match: '^exec$', timeout: 2s}\n")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"hooks:\n  - {event: tool_whenever, command: x}\n",
		"hooks:\n  - {event: turn_end}\n",
		"hooks:\n  - {event: turn_end, command: x, match: '('}\n",
		"channels: {tg: {type: telegram, token_env: T, progress: loud}}\n",
	} {
		if _, err := config.Parse([]byte(base + bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
