package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	plat "github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// fakeRelease builds a fake release archive holding a habakkuk binary.
func fakeRelease(t *testing.T, bin string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range map[string]string{"habakkuk": bin, "LICENSE": "MIT"} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func TestUpdateVerifiesAndReplaces(t *testing.T) {
	archive := fakeRelease(t, "#!/bin/sh\necho new\n")
	sum := sha256.Sum256(archive)
	sums := hex.EncodeToString(sum[:]) + "  habakkuk_linux_arm64.tar.gz\n" + strings.Repeat("0", 64) + "  habakkuk_darwin_amd64.tar.gz\n"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			w.Write([]byte(`{"tag_name":"v0.3.0","assets":[
				{"name":"habakkuk_linux_arm64.tar.gz","browser_download_url":"` + srv.URL + `/dl/a.tgz"},
				{"name":"habakkuk_darwin_amd64.tar.gz","browser_download_url":"` + srv.URL + `/dl/a.tgz"},
				{"name":"SHA256SUMS","browser_download_url":"` + srv.URL + `/dl/sums"}]}`))
		case "/dl/a.tgz":
			w.Write(archive)
		case "/dl/sums":
			w.Write([]byte(sums))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "habakkuk")
	os.WriteFile(exe, []byte("old"), 0o750)
	var out bytes.Buffer
	u := &updater{API: srv.URL, HTTP: srv.Client(), OS: "linux", Arch: "arm64", Exe: exe, Current: "0.2.0-dev", Out: &out, Home: t.TempDir()}
	ctx := context.Background()

	if err := u.update(ctx, updateOptions{checkOnly: true}); err != nil || !strings.Contains(out.String(), "update available on the stable channel: 0.2.0-dev -> v0.3.0") {
		t.Fatalf("check: %v %s", err, out.String())
	}
	if data, _ := os.ReadFile(exe); string(data) != "old" {
		t.Fatal("-check replaced the binary")
	}
	if err := u.update(ctx, updateOptions{}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(exe)
	info, _ := os.Stat(exe)
	if string(data) != "#!/bin/sh\necho new\n" || (plat.HasUnixPermissions && info.Mode().Perm() != 0o750) {
		t.Fatalf("binary = %q, mode %v", data, info.Mode().Perm())
	}

	u.Current = "v0.3.0"
	out.Reset()
	if err := u.update(ctx, updateOptions{}); err != nil || !strings.Contains(out.String(), "up to date") {
		t.Fatalf("same version: %v %s", err, out.String())
	}

	// A checksum mismatch installs nothing.
	os.WriteFile(exe, []byte("old"), 0o755)
	u = &updater{API: srv.URL, HTTP: srv.Client(), OS: "darwin", Arch: "amd64", Exe: exe, Current: "0.2.0", Out: io.Discard, Home: t.TempDir()}
	if err := u.update(ctx, updateOptions{}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatch: %v", err)
	}
	u.OS = "plan9"
	if err := u.update(ctx, updateOptions{}); err == nil || !strings.Contains(err.Error(), "no build") {
		t.Fatalf("missing platform: %v", err)
	}
	if data, _ := os.ReadFile(exe); string(data) != "old" {
		t.Fatal("a failed update touched the binary")
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"v0.3.0", "0.2.0-dev", 1}, {"v0.2.0", "0.2.0-dev", 1}, {"v0.2.0", "v0.2.0", 0},
		{"v0.2.0-rc1", "v0.2.0-rc2", -1}, {"v1.0.0", "v0.10.0", 1}, {"v0.9", "v0.10.0", -1},
		{"v0.1.0", "dev", 1}, {"main", "v0.1.0", -1},
	} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestBackupAndRestore(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(home, rel)
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(body), 0o600)
	}
	write("config.yaml", "version: 1\n")
	write(".env", "KEY=secret\n")
	write("agents/habakkuk/SOUL.md", "be kind")
	write("agents/habakkuk/memory/boat.md", "Heron")
	write("agents/habakkuk/workspace/big.bin", "lots")
	write("skills/deploy/SKILL.md", "ship")
	write("habakkuk.log", "noise")
	db, err := store.Open(ctx, filepath.Join(home, "control.db"), jobs.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	q := jobs.NewQueue(db)
	if _, err := q.Enqueue(ctx, jobs.NewJob{ID: "job_keep", Kind: "turn"}); err != nil {
		t.Fatal(err)
	}
	// The database stays open (as under habakkuk serve) while it is backed up.
	archive := filepath.Join(t.TempDir(), "b.tar.gz")
	if _, err := writeBackup(ctx, home, archive, false, false); err != nil {
		t.Fatal(err)
	}
	db.Close()
	names := tarNames(t, archive)
	for _, want := range []string{"config.yaml", "control.db", "agents/habakkuk/SOUL.md", "agents/habakkuk/memory/boat.md", "skills/deploy/SKILL.md"} {
		if !names[want] {
			t.Errorf("backup lacks %s: %v", want, names)
		}
	}
	for _, not := range []string{".env", "habakkuk.log", "agents/habakkuk/workspace/big.bin"} {
		if names[not] {
			t.Errorf("backup holds %s", not)
		}
	}

	dest := t.TempDir()
	if _, err := restoreBackup(archive, dest, false); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dest, "agents/habakkuk/memory/boat.md")); string(data) != "Heron" {
		t.Fatal("memory not restored")
	}
	rdb, err := store.Open(ctx, filepath.Join(dest, "control.db"), jobs.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	if j, err := jobs.NewQueue(rdb).Get(ctx, "job_keep"); err != nil || j.ID != "job_keep" {
		t.Fatalf("job not restored: %v", err)
	}
	rdb.Close()
	if _, err := restoreBackup(archive, dest, false); err == nil {
		t.Fatal("restored over an existing config without -force")
	}
	if _, err := restoreBackup(archive, dest, true); err != nil {
		t.Fatal(err)
	}

	// -env adds the secrets.
	if _, err := writeBackup(ctx, home, archive, true, true); err != nil {
		t.Fatal(err)
	}
	if names := tarNames(t, archive); !names[".env"] || !names["agents/habakkuk/workspace/big.bin"] {
		t.Fatalf("-env -workspaces backup = %v", names)
	}
}

func TestRestoreRefusesForeignPaths(t *testing.T) {
	for _, name := range []string{"../evil", "/etc/passwd", "agents/../../x", "bin/habakkuk", "agents"} {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: 1, Typeflag: tar.TypeReg})
		tw.Write([]byte("x"))
		tw.Close()
		zw.Close()
		archive := filepath.Join(t.TempDir(), "b.tar.gz")
		os.WriteFile(archive, buf.Bytes(), 0o600)
		if _, err := restoreBackup(archive, t.TempDir(), false); err == nil {
			t.Errorf("restored %q", name)
		}
	}
}

func tarNames(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	out := map[string]bool{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		out[h.Name] = true
	}
	return out
}
