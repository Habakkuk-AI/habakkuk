package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// homeWith makes a Habakkuk home holding the fixture as config.yaml.
func homeWith(t *testing.T, fixture string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "migrate", fixture))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestLoadCurrentFormat(t *testing.T) {
	for _, src := range []string{"v1.yaml"} {
		cfg, err := Load(homeWith(t, src))
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if cfg.Version != CurrentVersion || cfg.Report.NeedsMigration() || len(cfg.Report.Unknown) > 0 {
			t.Fatalf("%s: version %d report %+v", src, cfg.Version, cfg.Report)
		}
		if cfg.Timezone != "America/Toronto" || cfg.Workers.Concurrency != 2 || cfg.Retention.Days != 30 {
			t.Fatalf("%s: values not loaded: %+v", src, cfg)
		}
	}
	// The starter config is current too.
	cfg, err := Parse([]byte(Template))
	if err != nil || cfg.Report.NeedsMigration() {
		t.Fatalf("template: %v %+v", err, cfg.Report)
	}
}

func TestLoadMigratesOldConfigInMemory(t *testing.T) {
	home := homeWith(t, "v0.yaml")
	before, _ := os.ReadFile(filepath.Join(home, "config.yaml"))
	cfg, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != CurrentVersion || cfg.Report.FileVersion != 0 || !cfg.Report.NeedsMigration() {
		t.Fatalf("version %d report %+v", cfg.Version, cfg.Report)
	}
	if s := cfg.Report.Summary(); !strings.Contains(s, "version 0") || !strings.Contains(s, "habakkuk config migrate") {
		t.Fatalf("summary %q", s)
	}
	if cfg.Retention.Days != 30 || cfg.Agents["habakkuk"].Permission != "workspace" {
		t.Fatalf("values lost in migration: %+v", cfg)
	}
	after, _ := os.ReadFile(filepath.Join(home, "config.yaml"))
	if string(after) != string(before) {
		t.Fatal("loading rewrote config.yaml")
	}
}

func TestMigrateFile(t *testing.T) {
	home := homeWith(t, "v0.yaml")
	path := filepath.Join(home, "config.yaml")
	old, _ := os.ReadFile(path)
	now := time.Date(2026, 11, 9, 8, 30, 0, 0, time.UTC)

	dry, err := MigrateFile(home, now, true)
	if err != nil || dry.From != 0 || dry.To != CurrentVersion || dry.Backup != "" {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	if cur, _ := os.ReadFile(path); string(cur) != string(old) {
		t.Fatal("dry run wrote the file")
	}

	res, err := MigrateFile(home, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := path + ".20261109-083000.bak"; res.Backup != want {
		t.Fatalf("backup %q, want %q", res.Backup, want)
	}
	if bak, _ := os.ReadFile(res.Backup); string(bak) != string(old) {
		t.Fatal("the backup is not the old file")
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "version: 1\n") {
		t.Fatalf("no version key written:\n%s", data)
	}
	if !strings.Contains(string(data), "# keep this comment") {
		t.Fatalf("comments lost:\n%s", data)
	}
	cfg, err := Load(home)
	if err != nil || cfg.Report.NeedsMigration() {
		t.Fatalf("after migrate: %v %+v", err, cfg.Report)
	}

	again, err := MigrateFile(home, now.Add(time.Minute), false)
	if err != nil || again.Backup != "" || again.From != CurrentVersion {
		t.Fatalf("second migrate should do nothing: %+v %v", again, err)
	}
}

func TestUnknownKeysWarnOnLoad(t *testing.T) {
	home := homeWith(t, "unknown-keys.yaml")
	cfg, err := Load(home)
	if err != nil {
		t.Fatalf("unknown keys should not stop Habakkuk: %v", err)
	}
	got := strings.Join(cfg.Report.Unknown, "\n")
	if !strings.Contains(got, `"budget_per_day" in Agent (line 14)`) || !strings.Contains(got, `"future_feature" (line 16)`) {
		t.Fatalf("unknown keys reported: %q", got)
	}
	if cfg.Agents["habakkuk"].Model != "anthropic/claude-opus-5-5" {
		t.Fatal("known keys around the unknown ones were not loaded")
	}
	data, _ := os.ReadFile(filepath.Join(home, "config.yaml"))
	if _, err := Parse(data); err == nil {
		t.Fatal("Parse (used to check edits) should still refuse unknown keys")
	}
}

func TestNewerConfigIsRefusedClearly(t *testing.T) {
	_, err := Load(homeWith(t, "future.yaml"))
	if !errors.Is(err, ErrConfigTooNew) || !strings.Contains(err.Error(), "version 99") || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("err = %v", err)
	}
	if _, err := MigrateFile(homeWith(t, "future.yaml"), time.Now(), false); !errors.Is(err, ErrConfigTooNew) {
		t.Fatalf("migrate: %v", err)
	}
}

// The step machinery, with made-up steps: each runs once, in order, and the
// version key follows.
func TestApplyStepsChain(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("old_name: 3\nkeep: x\n"), &doc); err != nil {
		t.Fatal(err)
	}
	root := docRoot(&doc)
	steps := []configStep{
		{from: 0, note: "add version", fn: func(*yaml.Node) ([]string, error) { return nil, nil }},
		{from: 1, note: "rename old_name", fn: func(r *yaml.Node) ([]string, error) {
			for i := 0; i < len(r.Content); i += 2 {
				if r.Content[i].Value == "old_name" {
					r.Content[i].Value = "new_name"
					return []string{"old_name is now new_name"}, nil
				}
			}
			return nil, nil
		}},
	}
	notes, err := applySteps(root, 0, 2, steps)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := yaml.Marshal(&doc)
	if !strings.HasPrefix(string(out), "version: 2\n") || !strings.Contains(string(out), "new_name: 3") || !strings.Contains(string(out), "keep: x") {
		t.Fatalf("migrated:\n%s", out)
	}
	if len(notes) != 3 || !strings.Contains(notes[1], "v1->v2") {
		t.Fatalf("notes %q", notes)
	}
	if _, err := applySteps(root, 2, 3, steps); err == nil {
		t.Fatal("a missing step should be an error")
	}
}
