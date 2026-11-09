package local

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolve(t *testing.T) {
	home := t.TempDir()
	now := time.Now()

	if _, err := Resolve(home, "", now); err == nil {
		t.Fatal("no config.yaml: want an error")
	}
	write(t, filepath.Join(home, "config.yaml"), "agents:\n  habakkuk: {}\n")
	if _, err := Resolve(home, "", now); !errors.Is(err, ErrNoHTTP) {
		t.Fatalf("no http section: %v", err)
	}

	write(t, filepath.Join(home, "config.yaml"), "http:\n  listen: 0.0.0.0:9000\n  token_env: HABAKKUK_TEST_TOKEN\n")
	t.Setenv("HABAKKUK_TEST_TOKEN", "")
	if _, err := Resolve(home, "", now); err == nil || !strings.Contains(err.Error(), "HABAKKUK_TEST_TOKEN") {
		t.Fatalf("token missing: %v", err)
	}
	write(t, filepath.Join(home, ".env"), "# keys\nexport HABAKKUK_TEST_TOKEN=\"from-file\"\nOTHER=x\n")
	ep, err := Resolve(home, "", now)
	if err != nil || ep.URL != "http://127.0.0.1:9000" || ep.Token != "from-file" {
		t.Fatalf("from config = %+v, %v", ep, err)
	}
	t.Setenv("HABAKKUK_TEST_TOKEN", "from-env")
	if ep, _ := Resolve(home, "", now); ep.Token != "from-env" {
		t.Fatalf("environment must win over .env: %+v", ep)
	}

	// A live serve.json wins over config; a stale one does not.
	hb := Heartbeat{PID: 42, Beat: now.Add(-10 * time.Second), Listen: "127.0.0.1:9100"}
	data, _ := json.Marshal(hb)
	write(t, filepath.Join(home, "serve.json"), string(data))
	if ep, _ := Resolve(home, "", now); ep.URL != "http://127.0.0.1:9100" {
		t.Fatalf("live heartbeat: %+v", ep)
	}
	if ep, _ := Resolve(home, "", now.Add(time.Minute)); ep.URL != "http://127.0.0.1:9000" {
		t.Fatalf("stale heartbeat: %+v", ep)
	}
	// The address a running serve printed wins over both.
	if ep, _ := Resolve(home, "[::]:9200", now); ep.URL != "http://[::1]:9200" {
		t.Fatalf("printed address: %+v", ep)
	}

	write(t, filepath.Join(home, "config.yaml"), "http: {}\n")
	os.Remove(filepath.Join(home, "serve.json"))
	if ep, err := Resolve(home, "", now); err != nil || ep.URL != "http://"+DefaultListen || ep.Token != "" {
		t.Fatalf("defaults: %+v, %v", ep, err)
	}
}

func TestHome(t *testing.T) {
	t.Setenv("HABAKKUK_HOME", "/srv/habakkuk")
	if Home("") != "/srv/habakkuk" {
		t.Fatal("HABAKKUK_HOME ignored")
	}
	if Home("/x") != "/x" {
		t.Fatal("explicit home ignored")
	}
	if h := Home("~/f"); strings.HasPrefix(h, "~") {
		t.Fatalf("~ not expanded: %s", h)
	}
}

func TestFindBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec bits")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "habakkuk")
	write(t, bin, "#!/bin/sh\n")
	if _, err := FindBinary(bin); err == nil {
		t.Fatal("a non-executable file was accepted")
	}
	os.Chmod(bin, 0o755)
	if p, err := FindBinary(bin); err != nil || p != bin {
		t.Fatalf("explicit = %q, %v", p, err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("HOME", t.TempDir())
	if p, err := FindBinary(""); err != nil || p != bin {
		// a bundled or system habakkuk may be found first on a dev machine
		if err != nil {
			t.Fatalf("on PATH: %v", err)
		}
	}
}

func TestSearchPath(t *testing.T) {
	t.Setenv("PATH", "/a"+string(os.PathListSeparator)+"/a")
	p := SearchPath()
	if !strings.HasPrefix(p, "/a") || strings.Count(p, "/a"+string(os.PathListSeparator)) > 1 {
		t.Fatalf("SearchPath = %q", p)
	}
}
