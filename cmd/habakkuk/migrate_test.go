package main

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// sourceHome copies a fixture from internal/migrate/testdata into a fresh
// user home under the source's default folder name.
func sourceHome(t *testing.T, from string) (userHome, dir string) {
	t.Helper()
	userHome = t.TempDir()
	dir = filepath.Join(userHome, "."+from)
	if err := copyHome(dir, filepath.Join("..", "..", "internal", "migrate", "testdata", from)); err != nil {
		t.Fatal(err)
	}
	return userHome, dir
}

// copyHome copies a fixture home, turning its stored dotenv files (the
// repository ignores .env) into .env.
func copyHome(dst, src string) error {
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		return err
	}
	return filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == "dotenv" {
			return os.Rename(p, filepath.Join(filepath.Dir(p), ".env"))
		}
		return err
	})
}

func noEnv(string) string { return "" }

// fixtureSecrets are values in the fixtures that must never be printed.
var fixtureSecrets = []string{"sk-or-fixture-1111", "sk-ant-fixture-2222", "123456:fixture-telegram", "ntn-fixture-3333",
	"ghp_fixtureTokenValue123", "Ab3dE5fG7hJ9", "sk-ant-oc-fixture-6666", "789:fixture-tg", "studio-fixture-7777",
	"remote-fixture-8888", "gsk-fixture-4444", "discord-fixture-5555"}

func assertNoSecrets(t *testing.T, out string) {
	t.Helper()
	for _, s := range fixtureSecrets {
		if strings.Contains(out, s) {
			t.Errorf("output shows a secret value (%s...)", s[:4])
		}
	}
}

func TestMigrateDryRunWritesNothing(t *testing.T) {
	for _, from := range []string{"hermes", "openclaw"} {
		t.Run(from, func(t *testing.T) {
			user, _ := sourceHome(t, from)
			home := filepath.Join(t.TempDir(), "habakkuk")
			var out bytes.Buffer
			if err := runMigrate(context.Background(), home, []string{"--dry-run"}, strings.NewReader(""), &out, noEnv, user); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "Dry run: nothing was written.") {
				t.Errorf("output:\n%s", out.String())
			}
			if _, err := os.Stat(home); !os.IsNotExist(err) {
				t.Errorf("a dry run created %s", home)
			}
			assertNoSecrets(t, out.String())
		})
	}
}

func TestMigrateYes(t *testing.T) {
	cases := []struct {
		from, secrets string
		wantEnv       bool
		wantOut       []string
	}{
		{"hermes", "copy", true, []string{"Copied ANTHROPIC_API_KEY", "hk schedules resume sch_", "hk doctor", "hk serve", "hk whatsapp link"}},
		{"hermes", "skip", false, []string{"Set these in", "TELEGRAM_BOT_TOKEN", "Schedules are imported paused"}},
		{"openclaw", "", false, []string{"Set these in", "STUDIO_KEY", "the imessage channel"}},
	}
	for _, c := range cases {
		t.Run(c.from+"/"+c.secrets, func(t *testing.T) {
			user, _ := sourceHome(t, c.from)
			home := t.TempDir()
			args := []string{"--from", c.from, "-y"}
			if c.secrets != "" {
				args = append(args, "--secrets", c.secrets)
			}
			var out bytes.Buffer
			if err := runMigrate(context.Background(), home, args, strings.NewReader(""), &out, noEnv, user); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			for _, w := range c.wantOut {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, out.String())
				}
			}
			assertNoSecrets(t, out.String())
			info, err := os.Stat(filepath.Join(home, ".env"))
			if c.wantEnv != (err == nil) {
				t.Fatalf(".env exists: %v, want %v", err == nil, c.wantEnv)
			}
			if c.wantEnv && runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
				t.Errorf(".env mode %v", info.Mode().Perm())
			}
			if _, err := config.Load(home); err != nil {
				t.Fatalf("config.yaml does not load: %v", err)
			}
			// Again: nothing to do.
			out.Reset()
			if err := runMigrate(context.Background(), home, args, strings.NewReader(""), &out, noEnv, user); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "Nothing to import") {
				t.Errorf("second run:\n%s", out.String())
			}
		})
	}
}

func TestMigrateAsks(t *testing.T) {
	user, _ := sourceHome(t, "hermes")
	home := t.TempDir()
	var out bytes.Buffer
	if err := runMigrate(context.Background(), home, nil, strings.NewReader("n\n"), &out, noEnv, user); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Import now? (Y/n)") || !strings.Contains(out.String(), "Nothing was written.") {
		t.Errorf("output:\n%s", out.String())
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Errorf("declining wrote %d entries", len(entries))
	}
	out.Reset()
	if err := runMigrate(context.Background(), home, nil, strings.NewReader("\nn\n"), &out, noEnv, user); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Copy the 5 keys and tokens into") {
		t.Errorf("output:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".env")); err == nil {
		t.Error("keys were copied after answering no")
	}
	if _, err := os.Stat(filepath.Join(home, "agents", "habakkuk", "SOUL.md")); err != nil {
		t.Error("the import did not run after answering yes")
	}
}

func TestMigratePicksSource(t *testing.T) {
	user, _ := sourceHome(t, "openclaw")
	var out bytes.Buffer
	if err := runMigrate(context.Background(), t.TempDir(), []string{"--dry-run"}, nil, &out, noEnv, user); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Found OpenClaw at") {
		t.Errorf("output:\n%s", out.String())
	}
	if err := copyHome(filepath.Join(user, ".hermes"), filepath.Join("..", "..", "internal", "migrate", "testdata", "hermes")); err != nil {
		t.Fatal(err)
	}
	err := runMigrate(context.Background(), t.TempDir(), []string{"--dry-run"}, nil, &out, noEnv, user)
	if err == nil || !strings.Contains(err.Error(), "pass --from hermes or --from openclaw") {
		t.Errorf("both present: %v", err)
	}
	err = runMigrate(context.Background(), t.TempDir(), []string{"--dry-run"}, nil, &out, noEnv, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "found no setup to import") {
		t.Errorf("none present: %v", err)
	}
	for _, args := range [][]string{{"--from", "other"}, {"--secrets", "maybe"}, {"--agent", "a/b"}, {"extra"}} {
		if err := runMigrate(context.Background(), t.TempDir(), args, nil, &out, noEnv, user); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
}

// TestSetupOffersImport: a new home's setup imports only when asked (or,
// with -y, given -migrate), and then edits the config the import wrote.
func TestSetupOffersImport(t *testing.T) {
	user, _ := sourceHome(t, "hermes")
	cases := []struct {
		name  string
		args  []string
		stdin string
		want  bool
	}{
		{"yes without -migrate", []string{"-y"}, "", false},
		{"yes with -migrate", []string{"-y", "-migrate"}, "", true},
		{"interactive yes", nil, "y\n", true},
		{"interactive no", nil, "n\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			var out bytes.Buffer
			s, err := newSetup(context.Background(), home, c.args, strings.NewReader(c.stdin), &out)
			if err != nil {
				t.Fatal(err)
			}
			s.getenv, s.userHome = noEnv, user
			if err := s.load(); err != nil {
				t.Fatal(err)
			}
			if err := s.offerImport(); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			_, err = os.Stat(filepath.Join(home, "config.yaml"))
			if got := err == nil; got != c.want {
				t.Fatalf("imported: %v, want %v\n%s", got, c.want, out.String())
			}
			if c.want {
				if s.cfg == nil || s.cfg.Agents["habakkuk"] == nil {
					t.Error("setup did not reload the imported config")
				}
				if _, err := os.Stat(filepath.Join(home, ".env")); err != nil {
					t.Error("the keys were not copied")
				}
				assertNoSecrets(t, out.String())
			}
		})
	}
}
