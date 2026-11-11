package nodes

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// Files a node returns are saved only where nodes/<node>/ really is inside
// the workspace: a link named nodes cannot lead them into a locked folder.
func TestNodeFilesDoNotSaveThroughALink(t *testing.T) {
	ws, home := t.TempDir(), t.TempDir()
	locked := filepath.Join(home, "projects")
	os.MkdirAll(locked, 0o700)
	os.Symlink(home, filepath.Join(ws, "nodes"))
	env := &tools.Env{Workspace: ws, Policy: &tools.Policy{Locked: []string{locked}}}
	res := &Result{Text: "ok", Files: []File{{Name: "trusted.json", Data: []byte("[]")}}}
	out := invokeTool{}.files(context.Background(), env, "projects", res)
	if !strings.Contains(out, "could not save") {
		t.Fatalf("saved through the link: %s", out)
	}
	if ents, _ := os.ReadDir(locked); len(ents) > 0 {
		t.Fatal("a node file landed in the locked folder")
	}
	os.Remove(filepath.Join(ws, "nodes"))
	if out := (invokeTool{}).files(context.Background(), env, "phone", res); !strings.Contains(out, "saved ") {
		t.Fatalf("did not save in the workspace: %s", out)
	}
}

// files.put on a node running on the same machine never writes into a
// Habakkuk home, even when the shared folder holds it or links to it.
func TestFilesPutRefusesTheHabakkukHome(t *testing.T) {
	shared, home := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(home, "projects"), 0o700)
	os.Symlink(home, filepath.Join(shared, "hk"))
	caps := LocalCaps(LocalOptions{Dir: shared, Protected: []string{home}})
	put := caps[CmdFilesPut]
	for _, p := range []string{"hk/projects/trusted.json", "hk/config.yaml"} {
		if _, err := put.Run(context.Background(), []byte(`{"path":"`+p+`","text":"[]"}`)); err == nil {
			t.Errorf("files.put wrote %s", p)
		}
	}
	if ents, _ := os.ReadDir(filepath.Join(home, "projects")); len(ents) > 0 {
		t.Fatal("a file landed in the trust folder")
	}
	// A shared folder that is the home itself.
	inHome := LocalCaps(LocalOptions{Dir: home, Protected: []string{home}})[CmdFilesPut]
	if _, err := inHome.Run(context.Background(), []byte(`{"path":"projects/trusted.json","text":"[]"}`)); err == nil {
		t.Error("files.put wrote into the home it shares")
	}
	if _, err := put.Run(context.Background(), []byte(`{"path":"notes.txt","text":"hi"}`)); err != nil {
		t.Errorf("an ordinary put failed: %v", err)
	}
}

// A node does not know where the gateway keeps its home: any folder that
// is laid out as a Habakkuk home is refused, wherever it is.
func TestFilesPutRefusesAnyHabakkukHome(t *testing.T) {
	shared := t.TempDir()
	gw := filepath.Join(shared, "srv", "hk") // the gateway's HABAKKUK_HOME, not the node's default
	os.MkdirAll(filepath.Join(gw, "agents", "habakkuk"), 0o700)
	os.WriteFile(filepath.Join(gw, "config.yaml"), []byte("version: 1\n"), 0o600)
	put := LocalCaps(LocalOptions{Dir: shared})[CmdFilesPut]
	if _, err := put.Run(context.Background(), []byte(`{"path":"srv/hk/projects/trusted.json","text":"[]"}`)); err == nil {
		t.Fatal("files.put wrote into a Habakkuk home it was not told about")
	}
	if _, err := os.Stat(filepath.Join(gw, "projects")); !os.IsNotExist(err) {
		t.Fatal("projects/ was created in the home")
	}
	if _, err := put.Run(context.Background(), []byte(`{"path":"srv/notes.txt","text":"hi"}`)); err != nil {
		t.Fatalf("an ordinary put failed: %v", err)
	}
}
