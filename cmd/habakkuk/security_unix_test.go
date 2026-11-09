//go:build unix

package main

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
)

// Everything Habakkuk creates under $HABAKKUK_HOME (databases, the agent's
// folder, identity, logs, workspace) is owner-only, whatever the umask
// lets through.
func TestHomeFilesArePrivate(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	home := writeHome(t, "")
	ctx := context.Background()
	a, closeApp, err := open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	spec := a.Runner.Agents["habakkuk"]
	if err := agent.WriteIdentity(spec.Dir, agent.Identity{Name: "Hab"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Runner.NewSession(ctx, "habakkuk"); err != nil {
		t.Fatal(err)
	}
	a.Log.Info("hello")
	closeApp()

	n := 0
	_ = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == home {
			return nil
		}
		info, err := d.Info()
		if err != nil || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		n++
		if info.Mode().Perm()&0o077 != 0 {
			rel, _ := filepath.Rel(home, p)
			t.Errorf("%s is mode %04o", rel, info.Mode().Perm())
		}
		return nil
	})
	if n < 5 {
		t.Fatalf("only %d files created; the test checks nothing", n)
	}
	var out strings.Builder
	if got := permissionsDoctor(home, nil, &out); got != 0 {
		t.Fatalf("doctor found %d permission problems:\n%s", got, out.String())
	}
}
