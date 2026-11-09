package logs

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/testutil"
)

func TestRotationKeepsBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	r, err := OpenRotating(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	line := strings.Repeat("x", 39) + "\n" // 40 bytes: two lines per file
	for i := range 10 {
		if _, err := fmt.Fprintf(r, "%d%s", i, line[1:]); err != nil {
			t.Fatal(err)
		}
	}
	files := Files(dir)
	if len(files) != 3 { // habakkuk.log.2, .1, habakkuk.log
		t.Fatalf("files %v", files)
	}
	for _, f := range files {
		st, _ := os.Stat(f)
		if st.Size() > 100 {
			t.Errorf("%s is %d bytes", f, st.Size())
		}
		if platform.HasUnixPermissions && st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %o", f, st.Mode().Perm())
		}
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "8") {
		t.Fatalf("current file starts %q", data)
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatal("kept more than 2 backups")
	}
}

// Two writers (two Habakkuk processes) share the file across a rotation.
func TestRotationAcrossWriters(t *testing.T) {
	testutil.UnixOnly(t, "renames a log file another writer holds open")
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	a, _ := OpenRotating(path, 200, 10)
	b, _ := OpenRotating(path, 200, 10)
	defer a.Close()
	defer b.Close()
	var wg sync.WaitGroup
	for _, w := range []*Rotating{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 20 {
				fmt.Fprintf(w, "line %02d %s\n", i, strings.Repeat("y", 20))
			}
		}()
	}
	wg.Wait()
	total := 0
	for _, f := range Files(dir) {
		data, _ := os.ReadFile(f)
		total += strings.Count(string(data), "\n")
	}
	// Ten backups of 200 bytes hold all 40 lines: none is lost or doubled.
	if total != 40 {
		t.Fatalf("%d lines survived, want 40", total)
	}
}

func TestRedaction(t *testing.T) {
	var out bytes.Buffer
	r := NewRedactor(&out)
	r.Add("hunter2-secret", `pa"ss\word`, "short", "")
	log := slog.New(slog.NewJSONHandler(r, nil))
	log.Info("calling", "key", "hunter2-secret", "pw", `pa"ss\word`, "word", "short",
		"auth", "Bearer abcdefghijklmnopqrstuvwx",
		"openai", "sk-proj-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"slack", "xoxb-123456789012-abcdefghijkl",
		"gh", "ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"aws", "AKIAABCDEFGHIJKLMNOP",
		"tg", "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw0")
	s := out.String()
	for _, leak := range []string{"hunter2", `pa\"ss`, "abcdefghijklmnopqrstuvwx", "sk-proj-", "xoxb-", "ghp_", "AKIAABCD", "AAHdqTcv"} {
		if strings.Contains(s, leak) {
			t.Errorf("leaked %q: %s", leak, s)
		}
	}
	if !strings.Contains(s, `"word":"short"`) || !strings.Contains(s, `"auth":"Bearer [redacted]"`) {
		t.Errorf("over-redacted: %s", s)
	}
	if Parse(strings.TrimSpace(s)).Msg != "calling" {
		t.Errorf("not valid JSON after redaction: %s", s)
	}
}

func TestDotEnvSecrets(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("# c\nOPENAI_API_KEY=sk-123456\nexport BOT_TOKEN=\"tok-abcdef\"\nTZ=America/Toronto\nDEBUG=1\n"), 0o600)
	got := DotEnvSecrets(p)
	if strings.Join(got, ",") != "sk-123456,tok-abcdef" {
		t.Fatalf("%v", got)
	}
}

func TestParseFilterFormat(t *testing.T) {
	line := `{"time":"2026-11-07T10:00:00.5Z","level":"WARN","msg":"channel down","channel":"tg","err":"dial tcp: refused","n":3}`
	r := Parse(line)
	if r.Level != slog.LevelWarn || r.Msg != "channel down" || len(r.Attrs) != 3 || r.Attrs[0].Key != "channel" {
		t.Fatalf("%+v", r)
	}
	r.Time = time.Time{}
	if got := Format(r, false); got != `WARN  channel down  channel=tg  err="dial tcp: refused"  n=3` {
		t.Fatalf("format %q", got)
	}
	r = Parse(line)
	since := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		f    Filter
		want bool
	}{
		{Filter{}, true},
		{Filter{MinLevel: slog.LevelError}, false},
		{Filter{Since: since}, true},
		{Filter{Since: since.Add(2 * time.Hour)}, false},
		{Filter{Grep: NewGrep("DIAL TCP")}, true},
		{Filter{Grep: NewGrep("telegram")}, false},
		{Filter{Grep: NewGrep("[unclosed")}, false}, // literal, not a broken pattern
	}
	for i, c := range cases {
		if c.f.Match(r) != c.want {
			t.Errorf("case %d: want %v", i, c.want)
		}
	}
	if plain := Parse("panic: boom"); plain.Msg != "panic: boom" || plain.Level != slog.LevelInfo {
		t.Fatalf("plain line %+v", plain)
	}
}

func TestLastAcrossFiles(t *testing.T) {
	home := t.TempDir()
	rec := func(i int, lvl string) string {
		return fmt.Sprintf(`{"time":"2026-11-07T10:00:%02dZ","level":"%s","msg":"m%d"}`+"\n", i, lvl, i)
	}
	os.WriteFile(filepath.Join(home, FileName+".1"), []byte(rec(1, "INFO")+rec(2, "ERROR")), 0o600)
	os.WriteFile(filepath.Join(home, FileName), []byte(rec(3, "ERROR")+rec(4, "INFO")+rec(5, "ERROR")), 0o600)
	var b strings.Builder
	if err := Last(home, Filter{MinLevel: slog.LevelError}, 2, &b, false); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, "m2") || !strings.Contains(out, "m3") || !strings.Contains(out, "m5") || strings.Count(out, "\n") != 2 {
		t.Fatalf("%s", out)
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestFollowAcrossRotation(t *testing.T) {
	testutil.UnixOnly(t, "renames a log file the follower holds open")

	home := t.TempDir()
	path := filepath.Join(home, FileName)
	w, _ := OpenRotating(path, 150, 2)
	defer w.Close()
	fmt.Fprintln(w, `{"level":"INFO","msg":"before"}`)
	ctx, cancel := context.WithCancel(context.Background())
	out := &lockedBuf{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Follow(ctx, home, Filter{}, out, false, 10*time.Millisecond)
	}()
	time.Sleep(50 * time.Millisecond)
	for i := range 6 { // rotates more than once
		fmt.Fprintf(w, `{"level":"INFO","msg":"after %d"}`+"\n", i)
		time.Sleep(30 * time.Millisecond)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(out.String(), "after 5") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	s := out.String()
	if strings.Contains(s, "before") {
		t.Errorf("printed a record from before following: %s", s)
	}
	for i := range 6 {
		if !strings.Contains(s, fmt.Sprintf("after %d", i)) {
			t.Errorf("missed record %d: %s", i, s)
		}
	}
}

func TestSettings(t *testing.T) {
	home := t.TempDir()
	if m, b := Settings(home); m != DefaultMaxMB || b != DefaultBackups {
		t.Fatal(m, b)
	}
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte("version: 1\nlogging:\n  max_mb: 3\n  backups: 9\nunknown: [\n"), 0o600)
	if m, b := Settings(home); m != DefaultMaxMB || b != DefaultBackups { // invalid YAML: defaults
		t.Fatal(m, b)
	}
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte("version: 1\nlogging:\n  max_mb: 3\n  backups: 9\n"), 0o600)
	if m, b := Settings(home); m != 3 || b != 9 {
		t.Fatal(m, b)
	}
}
