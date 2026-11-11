package browser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// The browser's save tools follow links before they check the workspace: a
// link in the workspace cannot lead a PDF, screenshot or download into the
// Habakkuk home, and its locked folder stays out whatever the spelling.
func TestBrowserSavesStayInTheRealWorkspace(t *testing.T) {
	ws, home := t.TempDir(), t.TempDir()
	locked := filepath.Join(home, "projects")
	os.Symlink(home, filepath.Join(ws, "vendor"))
	env := &tools.Env{Workspace: ws, Policy: &tools.Policy{Locked: []string{locked}}}
	if p, err := outPath(env, "vendor/projects/page.pdf", "pdfs", "page", ".pdf"); err == nil {
		t.Fatalf("browser_pdf saved through a workspace link: %s", p)
	}
	if _, err := os.Stat(locked); !os.IsNotExist(err) {
		t.Fatal("projects/ was created through the link")
	}
	if d, err := outDir(env, "vendor/projects", "downloads"); err == nil {
		t.Fatalf("browser_download dir through a workspace link: %s", d)
	}
	// Inside the workspace works, and comes back as the real path.
	p, err := outPath(env, "", "pdfs", "page", ".pdf")
	if err != nil {
		t.Fatal(err)
	}
	if real, _ := filepath.EvalSymlinks(ws); filepath.Dir(filepath.Dir(p)) != real {
		t.Fatalf("saved at %s, not in the real workspace %s", p, real)
	}
	if _, err := outDir(env, "", "downloads"); err != nil {
		t.Fatal(err)
	}
}
