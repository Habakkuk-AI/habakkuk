package secrets

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/testutil"
)

func parse(t *testing.T, extra string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`version: 1
providers:
  anthropic: {type: anthropic, keys_env: [ANTHROPIC_API_KEY]}
agents:
  habakkuk: {model: anthropic/claude-opus-5-5}
` + extra))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

type fakeRun struct {
	mu    sync.Mutex
	calls []string
	out   map[string]string // joined args -> stdout
	err   error
}

func (f *fakeRun) run(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.Join(append([]string{name}, args...), " ")
	if len(env) > 0 {
		key += " [" + strings.Join(env, " ") + "]"
	}
	f.calls = append(f.calls, key)
	if f.err != nil {
		return nil, f.err
	}
	v, ok := f.out[key]
	if !ok {
		return nil, errors.New("exit status 1")
	}
	return []byte(v), nil
}

func TestSources(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "tg.secret"), []byte("tg-token-from-file\n"), 0o600)
	cfg := parse(t, `secrets:
  sources:
    1password: {}
    bitwarden: {cache: 1m}
    file: {}
    vault: {type: command, command: 'vault kv get -field=v "secret/$HABAKKUK_SECRET_REF"'}
  refs:
    ANTHROPIC_API_KEY: op://Private/Anthropic/credential
    SLACK_BOT_TOKEN: bitwarden:slack-bot
    SLACK_APP_TOKEN: bitwarden:slack-bot#app token
    MATRIX_TOKEN: bitwarden:matrix#username
    TELEGRAM_BOT_TOKEN: file:tg.secret
    DISCORD_BOT_TOKEN: vault:habakkuk/discord
`)
	vault := `/bin/sh -c vault kv get -field=v "secret/$HABAKKUK_SECRET_REF"`
	if platform.IsWindows { // command sources run in PowerShell there
		name, args := platform.ShellArgv(`vault kv get -field=v "secret/$HABAKKUK_SECRET_REF"`)
		vault = strings.Join(append([]string{name}, args...), " ")
	}
	f := &fakeRun{out: map[string]string{
		"op read --no-newline op://Private/Anthropic/credential": "sk-ant-xyz",
		"bw get password slack-bot":                              "xoxb-1\n",
		"bw get item slack-bot":                                  `{"fields":[{"name":"app token","value":"xapp-2"}]}`,
		"bw get username matrix":                                 "@habakkuk:example.org",
		vault + ` [HABAKKUK_SECRET_REF=habakkuk/discord HABAKKUK_SECRET_NAME=DISCORD_BOT_TOKEN]`: "discord-tok\n",
	}}
	r := New(cfg, home, nil)
	r.Run = f.run
	ctx := context.Background()
	for name, want := range map[string]string{
		"ANTHROPIC_API_KEY":  "sk-ant-xyz",
		"SLACK_BOT_TOKEN":    "xoxb-1",
		"SLACK_APP_TOKEN":    "xapp-2",
		"MATRIX_TOKEN":       "@habakkuk:example.org",
		"TELEGRAM_BOT_TOKEN": "tg-token-from-file",
		"DISCORD_BOT_TOKEN":  "discord-tok",
		"UNREFERENCED":       "",
	} {
		if got := r.Get(ctx, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	// Cached: a second read runs nothing.
	n := len(f.calls)
	r.Get(ctx, "ANTHROPIC_API_KEY")
	if len(f.calls) != n {
		t.Fatal("cached value was read again")
	}
	// After the cache period it is read again.
	now := time.Now().Add(6 * time.Minute)
	r.Now = func() time.Time { return now }
	r.Get(ctx, "ANTHROPIC_API_KEY")
	if len(f.calls) != n+1 {
		t.Fatal("expired value was not read again")
	}
}

// A failing source is logged without the value and not asked again for a
// while; the caller falls back.
func TestFailureIsCachedBrieflyAndNeverLogsValues(t *testing.T) {
	cfg := parse(t, "secrets:\n  sources: {1password: {}}\n  refs: {ANTHROPIC_API_KEY: \"op://v/i/f\"}\n")
	var logs bytes.Buffer
	r := New(cfg, t.TempDir(), slog.New(slog.NewTextHandler(&logs, nil)))
	f := &fakeRun{err: errors.New("op: not signed in")}
	r.Run = f.run
	if v := r.Get(context.Background(), "ANTHROPIC_API_KEY"); v != "" {
		t.Fatalf("got %q", v)
	}
	r.Get(context.Background(), "ANTHROPIC_API_KEY")
	if len(f.calls) != 1 {
		t.Fatalf("failing source asked %d times", len(f.calls))
	}
	if !strings.Contains(logs.String(), "ANTHROPIC_API_KEY") || !strings.Contains(logs.String(), "not signed in") {
		t.Fatalf("log: %s", logs.String())
	}

	// A success never puts the value in the log.
	logs.Reset()
	f = &fakeRun{out: map[string]string{"op read --no-newline op://v/i/f": "super-secret-value"}}
	r.Run = f.run
	r.Forget()
	if r.Get(context.Background(), "ANTHROPIC_API_KEY") != "super-secret-value" || strings.Contains(logs.String(), "super-secret") {
		t.Fatalf("log: %s", logs.String())
	}
}

func TestNilResolver(t *testing.T) {
	var r *Resolver
	if r.Get(context.Background(), "X") != "" || r.Has("X") || r.Names() != nil {
		t.Fatal("nil resolver resolved something")
	}
	if New(parse(t, ""), "", nil) != nil {
		t.Fatal("resolver without refs")
	}
}

func TestExecKeepsStdoutOutOfErrors(t *testing.T) {
	testutil.UnixOnly(t, "runs /bin/sh")
	_, err := Exec(context.Background(), nil, "/bin/sh", "-c", "echo the-value; echo bad thing >&2; exit 3")
	if err == nil || strings.Contains(err.Error(), "the-value") || !strings.Contains(err.Error(), "bad thing") {
		t.Fatalf("%v", err)
	}
	out, err := Exec(context.Background(), []string{"HABAKKUK_SECRET_REF=x/y"}, "/bin/sh", "-c", `printf %s "$HABAKKUK_SECRET_REF"`)
	if err != nil || string(out) != "x/y" {
		t.Fatalf("%q %v", out, err)
	}
}
