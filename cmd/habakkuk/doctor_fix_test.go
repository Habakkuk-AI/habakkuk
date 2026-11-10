package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

func fixEnv(t *testing.T, home string) repairEnv {
	t.Helper()
	return repairEnv{home: home, userHome: t.TempDir(), goos: "linux", exe: "/usr/local/bin/habakkuk", path: "/usr/bin", now: time.Now()}
}

func TestDoctorFixFoldersPermissionsAndConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes")
	}
	home := writeHome(t, "")
	// An older config: no version key.
	path := filepath.Join(home, "config.yaml")
	data, _ := os.ReadFile(path)
	os.WriteFile(path, bytes.Replace(data, []byte("version: 1\n"), nil, 1), 0o644)
	os.Chmod(home, 0o755)
	e := fixEnv(t, home)

	repairs := planRepairs(e)
	var whats []string
	for _, r := range repairs {
		whats = append(whats, r.what)
	}
	all := strings.Join(whats, "\n")
	for _, want := range []string{"format version 0", filepath.Join(home, "ws") + " is missing", "open to other users"} {
		if !strings.Contains(all, want) {
			t.Errorf("plan misses %q:\n%s", want, all)
		}
	}

	var out bytes.Buffer
	if failed := applyRepairs(&out, repairs); failed != 0 {
		t.Fatalf("%d failed:\n%s", failed, out.String())
	}
	if _, err := os.Stat(filepath.Join(home, "ws")); err != nil {
		t.Fatal("workspace not created")
	}
	if st, _ := os.Stat(home); st.Mode().Perm() != 0o700 {
		t.Fatalf("home mode %o", st.Mode().Perm())
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %o", st.Mode().Perm())
	}
	cfg, err := config.Load(home)
	if err != nil || cfg.Report.NeedsMigration() {
		t.Fatalf("config not migrated: %v %+v", err, cfg.Report)
	}
	baks, _ := filepath.Glob(path + ".*.bak")
	if len(baks) != 1 {
		t.Fatalf("old config not kept: %v", baks)
	}
	if !strings.Contains(out.String(), "fixed: migrated config.yaml from version 0 to 1") {
		t.Fatalf("report:\n%s", out.String())
	}

	// A second run finds nothing left.
	if left := planRepairs(e); len(left) != 0 {
		t.Fatalf("still to fix: %+v", left[0].what)
	}
	out.Reset()
	applyRepairs(&out, nil)
	if !strings.Contains(out.String(), "Nothing to fix") {
		t.Fatal(out.String())
	}
}

func TestDoctorFixLeavesDecisionsAlone(t *testing.T) {
	// An unknown key and a missing provider key need a person: no repair.
	home := writeHome(t, "\nnot_a_setting: 1\n")
	os.MkdirAll(filepath.Join(home, "ws"), 0o700)
	os.MkdirAll(filepath.Join(home, "agents", "habakkuk"), 0o700)
	for _, r := range planRepairs(fixEnv(t, home)) {
		if strings.Contains(r.what, "not_a_setting") || strings.Contains(r.what, "key") {
			t.Fatalf("doctor --fix must not touch %q", r.what)
		}
	}
}

func TestDoctorFixServiceFile(t *testing.T) {
	home := writeHome(t, "")
	e := fixEnv(t, home)
	unit := filepath.Join(e.userHome, ".config", "systemd", "user", "habakkuk.service")
	os.MkdirAll(filepath.Dir(unit), 0o700)
	gone := filepath.Join(t.TempDir(), "old", "habakkuk")
	os.WriteFile(unit, []byte(systemdUnit(gone, home, "/opt/bin:/usr/bin")), 0o600)

	var out bytes.Buffer
	if serviceDoctor(e, &out) != 1 || !strings.Contains(out.String(), "no longer exists") {
		t.Fatalf("doctor did not see the stale service: %q", out.String())
	}
	r, ok := serviceRepair(e)
	if !ok {
		t.Fatal("no repair offered")
	}
	done, err := r.apply()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(done, "systemctl --user daemon-reload") {
		t.Fatalf("no reload advice: %q", done)
	}
	data, _ := os.ReadFile(unit)
	if !strings.Contains(string(data), `ExecStart="/usr/local/bin/habakkuk" serve`) || !strings.Contains(string(data), "PATH=/opt/bin:/usr/bin") {
		t.Fatalf("unit not rewritten with this binary and the old PATH:\n%s", data)
	}
	if old, _ := os.ReadFile(unit + ".bak"); !strings.Contains(string(old), gone) {
		t.Fatal("old unit not kept")
	}

	// A service for another home is left alone.
	os.WriteFile(unit, []byte(systemdUnit(gone, "/srv/other-home", "")), 0o600)
	if _, ok := serviceRepair(e); ok {
		t.Fatal("touched another home's service")
	}
}

func TestReadLaunchdService(t *testing.T) {
	home := writeHome(t, "")
	e := fixEnv(t, home)
	e.goos = "darwin"
	plist := filepath.Join(e.userHome, "Library", "LaunchAgents", launchdLabel+".plist")
	os.MkdirAll(filepath.Dir(plist), 0o700)
	os.WriteFile(plist, []byte(launchdPlist("/Users/sam/old & new/habakkuk", home, "/usr/bin")), 0o600)
	s, ok := readService(e)
	if !ok || s.program != "/Users/sam/old & new/habakkuk" || s.home != home {
		t.Fatalf("read %+v", s)
	}
	if got := servicePath("darwin", launchdPlist("x", home, "/usr/bin:/bin")); got != "/usr/bin:/bin" {
		t.Fatalf("path %q", got)
	}
}
