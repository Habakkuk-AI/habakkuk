package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/testutil"
)

func writeHome(t *testing.T, extra string) string {
	t.Helper()
	home := t.TempDir()
	os.Chmod(home, 0o700)
	cfg := strings.Replace(config.Template, "workspace: ~/habakkuk-workspace", "workspace: "+filepath.Join(home, "ws"), 1) + extra
	if err := store.WriteFileAtomic(filepath.Join(home, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.Local)
	for in, want := range map[string]time.Time{
		"1h":               now.Add(-time.Hour),
		"2d":               now.Add(-48 * time.Hour),
		"2026-11-06":       time.Date(2026, 11, 6, 0, 0, 0, 0, time.Local),
		"2026-11-06 09:30": time.Date(2026, 11, 6, 9, 30, 0, 0, time.Local),
	} {
		got, err := parseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	if _, err := parseSince("yesterday", now); err == nil {
		t.Error("nonsense accepted")
	}
}

func TestLogsCmd(t *testing.T) {
	home := t.TempDir()
	recent := time.Now().UTC().Format(time.RFC3339)
	lines := `{"time":"2020-01-01T00:00:00Z","level":"ERROR","msg":"ancient"}
{"time":"` + recent + `","level":"INFO","msg":"hello","channel":"tg"}
{"time":"` + recent + `","level":"WARN","msg":"channel down","channel":"tg"}
{"time":"` + recent + `","level":"ERROR","msg":"model failed","provider":"anthropic"}
`
	os.WriteFile(filepath.Join(home, "habakkuk.log"), []byte(lines), 0o600)
	run := func(args ...string) string {
		var b strings.Builder
		if err := logsCmd(context.Background(), home, args, &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	if out := run("-level", "warn", "-since", "1h"); strings.Contains(out, "hello") || strings.Contains(out, "ancient") ||
		!strings.Contains(out, "channel down") || !strings.Contains(out, "model failed") {
		t.Fatalf("level+since: %s", out)
	}
	if out := run("-grep", "anthropic"); strings.Count(out, "\n") != 1 || !strings.Contains(out, "provider=anthropic") {
		t.Fatalf("grep: %s", out)
	}
	if out := run("-n", "1"); !strings.Contains(out, "model failed") || strings.Count(out, "\n") != 1 {
		t.Fatalf("-n: %s", out)
	}
	var b strings.Builder
	if err := logsCmd(context.Background(), home, []string{"-level", "loud"}, &b); err == nil {
		t.Fatal("bad level accepted")
	}
}

type fakeTailscale struct {
	calls [][]string
	out   map[string]string
	err   error
}

func (f *fakeTailscale) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.out[strings.Join(args, " ")]), nil
}

func TestExposeTailscale(t *testing.T) {
	ctx := context.Background()
	noHTTP := writeHome(t, "")
	var b strings.Builder
	if err := exposeCmd(ctx, noHTTP, []string{"tailscale"}, &b, (&fakeTailscale{}).run); err == nil || !strings.Contains(err.Error(), "no http section") {
		t.Fatalf("no http: %v", err)
	}
	noToken := writeHome(t, "http:\n  listen: 127.0.0.1:8484\n")
	if err := exposeCmd(ctx, noToken, []string{"tailscale"}, &b, (&fakeTailscale{}).run); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("no token: %v", err)
	}

	home := writeHome(t, "http:\n  listen: 0.0.0.0:9000\n  token_env: HABAKKUK_HTTP_TOKEN\n")
	f := &fakeTailscale{out: map[string]string{"status --json": `{"Self":{"DNSName":"box.tail1234.ts.net."}}`}}
	b.Reset()
	if err := exposeCmd(ctx, home, []string{"tailscale"}, &b, f.run); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls[0], " "); got != "serve --bg --https=443 http://127.0.0.1:9000" {
		t.Fatalf("ran tailscale %s", got)
	}
	for _, c := range f.calls {
		if c[0] == "funnel" {
			t.Fatal("ran funnel")
		}
	}
	if !strings.Contains(b.String(), "https://box.tail1234.ts.net") || !strings.Contains(b.String(), "public_url") {
		t.Fatalf("output: %s", b.String())
	}

	// -print runs nothing.
	f = &fakeTailscale{}
	b.Reset()
	if err := exposeCmd(ctx, home, []string{"tailscale", "-print", "-port", "8443"}, &b, f.run); err != nil || len(f.calls) != 0 ||
		!strings.Contains(b.String(), "tailscale serve --bg --https=8443 http://127.0.0.1:9000") {
		t.Fatalf("print: %v %v %s", err, f.calls, b.String())
	}

	// status warns when Funnel is on; off turns serve off.
	f = &fakeTailscale{out: map[string]string{"serve status": "# Funnel on:\n#     - https://box.ts.net\n"}}
	b.Reset()
	if err := exposeCmd(ctx, home, []string{"status"}, &b, f.run); err != nil || !strings.Contains(b.String(), "WARNING") {
		t.Fatalf("status: %v %s", err, b.String())
	}
	f = &fakeTailscale{}
	if err := exposeCmd(ctx, home, []string{"off"}, &b, f.run); err != nil || strings.Join(f.calls[0], " ") != "serve --https=443 off" {
		t.Fatalf("off: %v %v", err, f.calls)
	}
	f = &fakeTailscale{err: errors.New("not logged in")}
	if err := exposeCmd(ctx, home, []string{"status"}, &b, f.run); err == nil {
		t.Fatal("tailscale failure hidden")
	}
}

func TestServeStateFromHeartbeat(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	if s := serveState(home, now); !strings.Contains(s, "not running") {
		t.Fatal(s)
	}
	write := func(hb heartbeat) {
		data, _ := json.Marshal(hb)
		os.WriteFile(filepath.Join(home, heartbeatFile), data, 0o600)
	}
	write(heartbeat{PID: os.Getpid(), Version: "1.2.3", Started: now.Add(-2 * time.Hour), Beat: now.Add(-5 * time.Second), Listen: "127.0.0.1:8484"})
	if s := serveState(home, now); !strings.Contains(s, "running, pid") || !strings.Contains(s, "up 2h0m0s") || !strings.Contains(s, "127.0.0.1:8484") {
		t.Fatal(s)
	}
	write(heartbeat{PID: os.Getpid(), Started: now.Add(-time.Hour), Beat: now.Add(-5 * time.Minute)})
	if s := serveState(home, now); !strings.Contains(s, "stuck?") {
		t.Fatal(s)
	}
	write(heartbeat{PID: 1 << 22, Beat: now})
	if s := serveState(home, now); !strings.Contains(s, "exited") {
		t.Fatal(s)
	}
}

func TestStatusAndHeartbeat(t *testing.T) {
	home := writeHome(t, "")
	ctx := context.Background()
	a, closeApp, err := open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	stop := startHeartbeat(ctx, home, a, "127.0.0.1:1")
	if _, err := readHeartbeat(home); err != nil {
		t.Fatal(err)
	}
	a.Log.Error("something broke", "agent", "habakkuk")
	closeApp()

	var b strings.Builder
	if err := statusCmd(ctx, home, &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"habakkuk " + version, "serve:    running", "queue:    empty", "today:    no model calls", "something broke"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	stop()
	if _, err := os.Stat(filepath.Join(home, heartbeatFile)); !os.IsNotExist(err) {
		t.Fatal("heartbeat left behind")
	}
}

func TestSecurityCmd(t *testing.T) {
	testutil.UnixOnly(t, "expects a Unix permission finding and fix")
	home := writeHome(t, "")
	os.WriteFile(filepath.Join(home, ".env"), []byte("ANTHROPIC_API_KEY=x\n"), 0o644)
	os.Chmod(filepath.Join(home, ".env"), 0o644)
	var b strings.Builder
	err := securityCmd(home, []string{"audit"}, &b)
	if err == nil || !strings.Contains(b.String(), "[high] permissions") || !strings.Contains(b.String(), "-fix") {
		t.Fatalf("%v\n%s", err, b.String())
	}
	b.Reset()
	if err := securityCmd(home, []string{"audit", "-fix"}, &b); err != nil {
		t.Fatalf("%v\n%s", err, b.String())
	}
	if st, _ := os.Stat(filepath.Join(home, ".env")); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	if err := securityCmd(home, []string{"bogus"}, &b); err == nil {
		t.Fatal("unknown subcommand accepted")
	}
	// Without a subcommand it shows the effective policy, then audits.
	b.Reset()
	if err := securityCmd(home, nil, &b); err != nil {
		t.Fatalf("%v\n%s", err, b.String())
	}
	if out := b.String(); !strings.Contains(out, "Effective tool policy:") || !strings.Contains(out, "permission workspace") {
		t.Fatalf("no policy:\n%s", out)
	}
	b.Reset()
	if err := securityCmd(home, []string{"-network"}, &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"[on ] model", "never: Habakkuk has no usage telemetry", "[off] search", "only when you run habakkuk update"} {
		if !strings.Contains(out, want) {
			t.Errorf("network report lacks %q:\n%s", want, out)
		}
	}
}

// Secrets never reach habakkuk.log, whichever variable holds them.
func TestLogRedactsConfiguredSecrets(t *testing.T) {
	home := writeHome(t, "")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-test-value-123")
	ctx := context.Background()
	a, closeApp, err := open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	a.Log.Info("oops", "key", a.Secret("ANTHROPIC_API_KEY"))
	closeApp()
	data, _ := os.ReadFile(filepath.Join(home, "habakkuk.log"))
	if strings.Contains(string(data), "anthropic-test-value-123") || !strings.Contains(string(data), "[redacted]") {
		t.Fatalf("log: %s", data)
	}
}
