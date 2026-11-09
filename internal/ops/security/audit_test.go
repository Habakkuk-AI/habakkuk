package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/testutil"
)

func parse(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`version: 1
providers:
  anthropic: {type: anthropic, keys_env: [ANTHROPIC_API_KEY]}
` + yaml))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func find(fs []Finding, check, text string) *Finding {
	for i := range fs {
		if fs[i].Check == check && strings.Contains(fs[i].Message, text) {
			return &fs[i]
		}
	}
	return nil
}

func plenty(string) (uint64, uint64, error) { return 500 << 30, 1000 << 30, nil }

func TestPermissionsFoundAndFixed(t *testing.T) {
	testutil.UnixOnly(t, "checks and fixes Unix permission bits")
	home := t.TempDir()
	os.Chmod(home, 0o755)
	write := func(rel string, mode os.FileMode) string {
		p := filepath.Join(home, rel)
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte("x"), mode)
		os.Chmod(p, mode)
		return p
	}
	env := write(".env", 0o644)
	db := write("control.db", 0o640)
	notes := write("agents/habakkuk/memory/n.md", 0o644)
	attachment := write("blobs/ab/abcd", 0o644)
	key := write("ssh/build.key", 0o640)
	skill := write("skills/demo/SKILL.md", 0o644)
	ws := write("agents/habakkuk/workspace/public.txt", 0o644) // the user's files: left alone
	os.Chmod(filepath.Join(home, "agents"), 0o755)

	fs := Audit(Options{Home: home, Disk: plenty, Now: time.Now()})
	h := find(fs, "permissions", "credential, chat, memory or database")
	if h == nil || h.Severity != High || !strings.HasPrefix(h.Message, "5 ") {
		t.Fatalf("findings: %+v", fs)
	}
	if o := find(fs, "permissions", "other file(s)"); o == nil || !strings.Contains(o.Message, "SKILL.md") || o.Severity != Low {
		t.Fatalf("findings: %+v", fs)
	}
	if find(fs, "permissions", "open to other users (mode 0755)") == nil || find(fs, "permissions", "folder(s)") == nil ||
		find(fs, "permissions", "other file(s)") == nil {
		t.Fatalf("findings: %+v", fs)
	}
	for _, f := range fs {
		if strings.Contains(f.Message, "public.txt") {
			t.Fatal("audited the workspace")
		}
		if f.Fixable() {
			if err := f.Apply(); err != nil {
				t.Fatal(err)
			}
		}
	}
	for p, want := range map[string]os.FileMode{home: 0o700, env: 0o600, db: 0o600, notes: 0o600, ws: 0o644,
		attachment: 0o600, key: 0o600, skill: 0o600,
		filepath.Join(home, "agents"): 0o700} {
		if st, _ := os.Stat(p); st.Mode().Perm() != want {
			t.Errorf("%s: %o, want %o", p, st.Mode().Perm(), want)
		}
	}
	for _, f := range Audit(Options{Home: home, Disk: plenty, Now: time.Now()}) {
		if f.Check == "permissions" {
			t.Errorf("after fix: %+v", f)
		}
	}
}

func TestExposureChecks(t *testing.T) {
	home := t.TempDir()
	cfg := parse(t, `agents:
  habakkuk: {model: anthropic/claude-opus-5-5, permission: full}
  helper: {model: anthropic/claude-opus-5-5, approvals: reviewer}
  quiet: {model: anthropic/claude-opus-5-5, permission: full}
channels:
  tg: {type: telegram, token_env: TG, allow_from: ["*"], agent: habakkuk, routes: {"42": helper}}
  ds: {type: discord, token_env: DS, allow_from: ["123"], agent: quiet}
http:
  listen: 0.0.0.0:8484
  token_env: HTTP_TOKEN
  public_url: http://habakkuk.example.com
  webhooks:
    alerts: {agent: helper, secret_env: ALERTS_SECRET}
    deploys: {agent: helper, secret_env: DEPLOY_SECRET}
metrics: {public: true}
mcp_servers:
  shell: {command: shell-mcp, risk: exec}
  remote: {url: "http://10.0.0.9:9000/mcp"}
tools: {allow_private_urls: true}
`)
	cfg.Agents["helper"].Toolsets = append(cfg.Agents["helper"].Toolsets, "mcp:shell")
	secrets := map[string]string{"HTTP_TOKEN": "short", "ALERTS_SECRET": "", "DEPLOY_SECRET": "abc"}
	fs := Audit(Options{Home: home, Config: cfg, Secret: func(n string) string { return secrets[n] }, Disk: plenty})
	for _, c := range []struct {
		check, text string
		sev         Severity
	}{
		{"agents", "agent habakkuk runs in full mode (no approvals) and outsiders reach it through channel tg", Critical},
		{"agents", "agent quiet runs in full mode", Medium},
		{"agents", "agent helper has no human approver", High},
		{"channels", `channel tg (telegram) has allow_from "*"`, High},
		{"http", "public_url http://habakkuk.example.com is plain http", High},
		{"http", "metrics.public", Low},
		{"http", "short", Medium},
		{"webhooks", "webhook alerts has no secret", High},
		{"webhooks", "webhook deploys has a short secret", Medium},
		{"mcp", "MCP server shell can run programs (risk: exec), used by helper; helper run in full mode or are reachable", High},
		{"mcp", "MCP server remote is reached over plain http", Medium},
		{"tools", "allow_private_urls", Low},
	} {
		f := find(fs, c.check, c.text)
		if f == nil {
			t.Errorf("no %s finding %q", c.check, c.text)
			continue
		}
		if f.Severity != c.sev {
			t.Errorf("%q: severity %s, want %s", c.text, f.Severity, c.sev)
		}
		if f.Fix == "" {
			t.Errorf("%q has no fix", c.text)
		}
	}
	if find(fs, "channels", "channel ds") != nil {
		t.Error("an allowlisted channel was flagged")
	}
	for i := 1; i < len(fs); i++ {
		if fs[i].Severity > fs[i-1].Severity {
			t.Fatal("findings not sorted by severity")
		}
	}
}

func TestQuietConfigHasNoExposureFindings(t *testing.T) {
	home := t.TempDir()
	os.Chmod(home, 0o700)
	cfg := parse(t, `agents:
  habakkuk: {model: anthropic/claude-opus-5-5}
http:
  token_env: HTTP_TOKEN
`)
	fs := Audit(Options{Home: home, Config: cfg, Secret: func(string) string { return strings.Repeat("k", 40) }, Disk: plenty})
	for _, f := range fs {
		if f.Severity > Info {
			t.Errorf("unexpected: %+v", f)
		}
	}
}

func TestSkillsHooksBackupsDisk(t *testing.T) {
	testutil.UnixOnly(t, "checks Unix permission bits and creates symlinks")
	home := t.TempDir()
	skill := filepath.Join(home, "skills", "deploy")
	os.MkdirAll(skill, 0o700)
	os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: deploy\n---\n"), 0o666)
	os.Chmod(filepath.Join(skill, "SKILL.md"), 0o666)
	os.WriteFile(filepath.Join(skill, "run.sh"), []byte("#!/bin/sh\n"), 0o700)
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(home, "skills", "linked"))

	hook := filepath.Join(home, "hooks", "guard.sh")
	os.MkdirAll(filepath.Dir(hook), 0o700)
	os.WriteFile(hook, []byte("#!/bin/sh\n"), 0o777)
	os.Chmod(hook, 0o777)

	os.MkdirAll(filepath.Join(home, "backups"), 0o700)
	old := filepath.Join(home, "backups", "habakkuk-backup-20250101.tar.gz")
	os.WriteFile(old, []byte("x"), 0o644)
	os.Chmod(old, 0o644)
	now := time.Now()
	os.Chtimes(old, now.Add(-40*24*time.Hour), now.Add(-40*24*time.Hour))
	cred := filepath.Join(home, "backups", "whatsapp-main.db.1")
	os.WriteFile(cred, []byte("x"), 0o600)

	cfg := parse(t, "agents:\n  habakkuk: {model: anthropic/claude-opus-5-5}\nhooks:\n  - {event: tool_before, command: \""+hook+" --strict\"}\n")
	low := func(string) (uint64, uint64, error) { return 500 << 20, 100 << 30, nil }
	fs := Audit(Options{Home: home, Config: cfg, Disk: low, Now: now})
	for _, c := range []struct {
		check, text string
		sev         Severity
	}{
		{"skills", "skill deploy can be changed by other users", High},
		{"skills", "skill deploy ships executable files", Info},
		{"skills", "is a link to", Medium},
		{"hooks", "can be changed by other users", High},
		{"backups", "backup archive(s) readable by other users", High},
		{"backups", "the newest backup archive is 40 days old", Low},
		{"backups", "stale credential backup(s)", Low},
		{"disk", "500 MB free", High},
	} {
		if f := find(fs, c.check, c.text); f == nil || f.Severity != c.sev {
			t.Errorf("want %s %q at %s; findings: %+v", c.check, c.text, c.sev, f)
		}
	}
	for _, f := range fs {
		if f.Fixable() {
			if err := f.Apply(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if st, _ := os.Stat(hook); st.Mode().Perm()&0o022 != 0 || st.Mode().Perm()&0o100 == 0 {
		t.Errorf("hook mode %o", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Join(skill, "SKILL.md")); st.Mode().Perm()&0o022 != 0 {
		t.Errorf("skill mode %o", st.Mode().Perm())
	}
	if st, _ := os.Stat(old); st.Mode().Perm() != 0o600 {
		t.Errorf("backup mode %o", st.Mode().Perm())
	}
}

func TestBrokenConfigStillChecksFiles(t *testing.T) {
	testutil.UnixOnly(t, "expects a Unix permission finding")

	home := t.TempDir()
	os.Chmod(home, 0o777)
	fs := Audit(Options{Home: home, ConfigErr: os.ErrInvalid, Disk: plenty})
	if find(fs, "config", "does not load") == nil || find(fs, "permissions", "open to other users") == nil {
		t.Fatalf("%+v", fs)
	}
}
