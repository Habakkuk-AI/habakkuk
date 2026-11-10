package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestTrustedWritePaths(t *testing.T) {
	agent := func(paths string) string {
		return strings.Replace(mediaBase, "{model: p/m}", "{model: p/m, trusted_write_paths: "+paths+"}", 1)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home folder:", err)
	}
	cfg, err := Parse([]byte(agent(`["~/notes", "~/Documents/agent notes"]`)))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Home = filepath.Join(userHome, ".habakkuk")
	dirs, ignored := cfg.TrustedWriteDirs("habakkuk")
	if want := []string{filepath.Join(userHome, "notes"), filepath.Join(userHome, "Documents", "agent notes")}; !slices.Equal(dirs, want) || len(ignored) != 0 {
		t.Fatalf("dirs %q, ignored %q; want %q", dirs, ignored, want)
	}
	if d, _ := cfg.TrustedWriteDirs("nobody"); d != nil {
		t.Fatalf("an unknown agent has trusted folders: %q", d)
	}

	// Too broad, or not a path that says where it is: refused when the
	// config loads.
	for _, bad := range []string{`["~"]`, `["~/"]`, `["notes"]`, `["./notes"]`, `[""]`, `["` + filepath.ToSlash(filepath.Dir(userHome)) + `"]`,
		`["` + filepath.ToSlash(filepath.VolumeName(userHome)) + `/"]`} {
		if _, err := Parse([]byte(agent(bad))); err == nil || !strings.Contains(err.Error(), "trusted_write_paths") {
			t.Errorf("%s: want a trusted_write_paths error, got %v", bad, err)
		}
	}

	// Habakkuk's own folders are never trusted: such entries are ignored
	// and reported, the rest still count.
	home := filepath.Join(t.TempDir(), "hk")
	notes := filepath.Join(t.TempDir(), "notes")
	list := []string{home, filepath.Dir(home), filepath.Join(home, "agents", "habakkuk"), filepath.Join(home, "skills"),
		filepath.Join(home, "hooks", "sub"), filepath.Join(home, "workspaces", "shared"), notes}
	quoted := make([]string, len(list))
	for i, p := range list {
		quoted[i] = `"` + filepath.ToSlash(p) + `"`
	}
	cfg, err = Parse([]byte(agent("[" + strings.Join(quoted, ", ") + "]")))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Home = home
	dirs, ignored = cfg.TrustedWriteDirs("habakkuk")
	if want := []string{filepath.Join(home, "workspaces", "shared"), notes}; !slices.Equal(dirs, want) {
		t.Fatalf("dirs %q, want %q", dirs, want)
	}
	if len(ignored) != 5 {
		t.Fatalf("ignored %q, want the five entries in Habakkuk's own folders", ignored)
	}
}

func TestProtectedDirs(t *testing.T) {
	home := filepath.Join(t.TempDir(), "hk")
	scripts := filepath.Join(t.TempDir(), "my-hooks")
	workdir := filepath.Join(t.TempDir(), "jobs")
	os.MkdirAll(workdir, 0o755)
	cfg := &Config{Home: home, Hooks: []*Hook{
		{Event: "tool_before", Command: filepath.Join(scripts, "check-exec.sh") + " --strict"},
		{Event: "turn_end", Command: "cd '" + workdir + "' && ./notify.sh"},
		{Event: "turn_end", Command: "echo done"},
		nil,
	}}
	got := cfg.ProtectedDirs()
	for _, want := range []string{filepath.Join(home, "agents"), filepath.Join(home, "skills"), filepath.Join(home, "hooks"),
		filepath.Join(home, "plugins"), scripts, workdir} {
		if !slices.Contains(got, want) {
			t.Errorf("%s is not protected: %q", want, got)
		}
	}
	// A command that names the home folder itself does not put everything
	// under it out of reach.
	if dirs := CommandDirs("cd ~/ && ls ~/x.sh"); len(dirs) != 0 {
		t.Errorf("CommandDirs protects the whole home: %q", dirs)
	}
}
