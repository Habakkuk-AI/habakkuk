package platform

import (
	"path/filepath"
	"runtime"
	"testing"
)

// HABAKKUK_SHELL set to the path of a shell this doesn't know runs that
// program, under its own name: never labelled bash.
func TestUnknownShellPathKeepsItsName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shells")
	}
	t.Setenv(ShellEnv, "/opt/homebrew/bin/fish")
	c := ScriptCommand("echo hi")
	if name := ScriptShellName(); name != "fish" || c.Path != "/opt/homebrew/bin/fish" || c.Args[1] != "-c" {
		t.Fatalf("name %q runs %q %q", name, c.Path, c.Args)
	}
	t.Setenv(ShellEnv, "")
	if name := ScriptShellName(); name != "bash" && name != "sh" {
		t.Fatalf("default: %q", name)
	}
}

// The name is the program that runs: only Windows drops an extension, so
// /x/bash.sh is not bash.
func TestShellNameMatchesProgram(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shells")
	}
	for _, sh := range []string{"/tmp/x/bash.sh", "/tmp/x/sh.zsh", "/tmp/x/dash.fish", "/bin/zsh", "/opt/homebrew/bin/fish"} {
		t.Setenv(ShellEnv, sh)
		c := ScriptCommand("true")
		name := ScriptShellName()
		base := filepath.Base(c.Path)
		if (name == "bash" || name == "sh" || name == "dash") && base != name {
			t.Errorf("%s runs %s but is named %q", sh, c.Path, name)
		}
	}
	if k := shellFor("windows", "C:/Git/bin/bash.exe", "", true, func(string) (string, error) { return "", nil }).kind; k != "bash" {
		t.Errorf("windows bash.exe: %q", k)
	}
}
