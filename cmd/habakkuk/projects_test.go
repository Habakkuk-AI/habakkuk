package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/agent/projtrust"
)

func TestProjectsTrustListUntrust(t *testing.T) {
	home := writeHome(t, "")
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, ".git"), 0o755)
	os.WriteFile(filepath.Join(proj, "AGENTS.md"), []byte("Run make test."), 0o644)
	os.WriteFile(filepath.Join(proj, "CLAUDE.md"), []byte("Small commits."), 0o644)

	// Shown first; one yes, one no.
	var out bytes.Buffer
	if err := projectsCmd(home, []string{"trust", proj}, strings.NewReader("y\nn\n"), &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "Run make test.") || !strings.Contains(s, "Trust it? [y/N]") ||
		!strings.Contains(s, "trusted: ") || !strings.Contains(s, "not trusted: ") {
		t.Fatalf("output:\n%s", s)
	}
	st := &projtrust.Store{Home: home}
	entries, status, _ := st.List()
	if len(entries) != 1 || filepath.Base(entries[0].Path) != "AGENTS.md" || status[0] != projtrust.StatusTrusted {
		t.Fatalf("entries %+v %v", entries, status)
	}

	// A change shows in the list.
	os.WriteFile(filepath.Join(proj, "AGENTS.md"), []byte("Run make test!"), 0o644)
	out.Reset()
	projectsCmd(home, []string{"list"}, nil, &out)
	if !strings.Contains(out.String(), "changed since it was trusted") {
		t.Fatalf("list:\n%s", out.String())
	}

	// -y trusts without asking; untrust removes.
	out.Reset()
	if err := projectsCmd(home, []string{"trust", "-y", filepath.Join(proj, "CLAUDE.md")}, nil, &out); err != nil || !strings.Contains(out.String(), "trusted: ") {
		t.Fatalf("trust -y: %v\n%s", err, out.String())
	}
	out.Reset()
	projectsCmd(home, []string{"untrust", proj}, nil, &out)
	if strings.Count(out.String(), "no longer trusted") != 2 {
		t.Fatalf("untrust:\n%s", out.String())
	}
	out.Reset()
	if err := projectsCmd(home, []string{"trust", t.TempDir()}, nil, &out); err != nil || !strings.Contains(out.String(), "No project instruction files") {
		t.Fatalf("empty folder: %v\n%s", err, out.String())
	}
	os.WriteFile(filepath.Join(proj, "README.md"), []byte("x"), 0o644)
	if err := projectsCmd(home, []string{"trust", "-y", filepath.Join(proj, "README.md")}, nil, &out); err == nil {
		t.Fatal("trusted a file that is not a project instruction file")
	}
}
