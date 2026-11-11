package platform

import (
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
