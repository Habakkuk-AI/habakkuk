package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func searchTree(t *testing.T, env *Env) {
	t.Helper()
	writeWS(t, env, map[string]string{
		".gitignore":          "*.log\nbuild/\n/root-only.txt\n!keep.log\n",
		"main.go":             "package main\n// TODO: first\n",
		"pkg/util.go":         "package pkg\n// todo: second\n",
		"pkg/root-only.txt":   "TODO not anchored here\n",
		"root-only.txt":       "TODO ignored at the root\n",
		"debug.log":           "TODO ignored log\n",
		"keep.log":            "TODO kept by negation\n",
		"build/out.go":        "TODO ignored dir\n",
		".hidden/x.go":        "TODO hidden\n",
		"web/.gitignore":      "*.js\n",
		"web/app.js":          "TODO ignored by nested gitignore\n",
		"web/app.ts":          "TODO in ts\n",
		"bin.dat":             "TODO\x00binary\n",
		"src/deep/nested.ts":  "TODO deep\n",
		"pkg/long.go":         "// TODO " + strings.Repeat("x", 500) + "\n",
		"other/notes.md":      "nothing here\n",
		"other/more/TODO.txt": "the file name matches, the text does not\n",
	})
}

func search(t *testing.T, s searchFiles, env *Env, input map[string]any) (string, error) {
	t.Helper()
	b, _ := json.Marshal(input)
	return s.Run(context.Background(), env, b)
}

func matchedFiles(out string) []string {
	var files []string
	for _, l := range strings.Split(out, "\n") {
		if f, _, ok := strings.Cut(l, ":"); ok && !strings.HasPrefix(l, "[") {
			files = append(files, filepath.ToSlash(f)) // Windows shows pkg\long.go
		}
	}
	slices.Sort(files)
	return slices.Compact(files)
}

func TestSearchWalkerHonorsGitignore(t *testing.T) {
	_, env := setup(t, ModeWorkspace, nil)
	searchTree(t, env)
	out, err := search(t, searchFiles{rg: "-"}, env, map[string]any{"pattern": "TODO"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"keep.log", "main.go", "pkg/long.go", "pkg/root-only.txt", "src/deep/nested.ts", "web/app.ts"}
	if got := matchedFiles(out); !slices.Equal(got, want) {
		t.Fatalf("files %v, want %v\n%s", got, want, out)
	}
	if !strings.Contains(out, "main.go:2: // TODO: first") {
		t.Fatalf("line format: %s", out)
	}
	if strings.Contains(out, strings.Repeat("x", 400)) {
		t.Fatal("long line not cut")
	}
}

func TestSearchOptions(t *testing.T) {
	_, env := setup(t, ModeWorkspace, nil)
	searchTree(t, env)
	s := searchFiles{rg: "-"}
	out, _ := search(t, s, env, map[string]any{"pattern": "todo", "ignore_case": true, "glob": "*.go"})
	if got := matchedFiles(out); !slices.Equal(got, []string{"main.go", "pkg/long.go", "pkg/util.go"}) {
		t.Fatalf("ignore_case+glob: %v", got)
	}
	out, _ = search(t, s, env, map[string]any{"pattern": "TODO", "glob": "src/**/*.ts", "files_only": true})
	if out != "src/deep/nested.ts" {
		t.Fatalf("anchored glob, files only: %q", out)
	}
	out, _ = search(t, s, env, map[string]any{"pattern": "TODO", "path": "pkg"})
	if got := matchedFiles(out); !slices.Equal(got, []string{"pkg/long.go", "pkg/root-only.txt"}) {
		t.Fatalf("path: %v", got)
	}
	out, _ = search(t, s, env, map[string]any{"pattern": "TODO", "max_results": 2})
	if !strings.Contains(out, "[stopped at 2 results") {
		t.Fatalf("cap: %s", out)
	}
	out, _ = search(t, s, env, map[string]any{"pattern": "no such text"})
	if out != "no matches" {
		t.Fatalf("no match: %q", out)
	}
	if _, err := search(t, s, env, map[string]any{"pattern": "("}); err == nil {
		t.Fatal("bad regexp accepted")
	}
}

func TestSearchWithRipgrep(t *testing.T) {
	rg, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("ripgrep is not installed")
	}
	_, env := setup(t, ModeWorkspace, nil)
	searchTree(t, env)
	s := searchFiles{rg: rg}
	out, err := search(t, s, env, map[string]any{"pattern": "TODO"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"keep.log", "main.go", "pkg/long.go", "pkg/root-only.txt", "src/deep/nested.ts", "web/app.ts"}
	if got := matchedFiles(out); !slices.Equal(got, want) {
		t.Fatalf("files %v, want %v\n%s", got, want, out)
	}
	out, _ = search(t, s, env, map[string]any{"pattern": "TODO", "glob": "*.ts", "files_only": true})
	if got := strings.Split(out, "\n"); len(got) != 2 {
		t.Fatalf("files only: %q", out)
	}
}

func TestGlobRegexp(t *testing.T) {
	cases := []struct {
		glob     string
		anchored bool
		path     string
		want     bool
	}{
		{"*.go", false, "a/b/c.go", true},
		{"*.go", false, "c.go.txt", false},
		{"build", false, "x/build", true},
		{"src/*.ts", true, "src/a.ts", true},
		{"src/*.ts", true, "src/x/a.ts", false},
		{"src/**/*.ts", true, "src/x/y/a.ts", true},
		{"src/**/*.ts", true, "src/a.ts", true},
		{"**/vendor", true, "a/vendor", true},
		{"logs/**", true, "logs/a/b", true},
		{"file[0-9].txt", false, "file7.txt", true},
		{"a?c", false, "abc", true},
	}
	for _, c := range cases {
		re, err := globRegexp(c.glob, c.anchored)
		if err != nil {
			t.Fatal(err)
		}
		if got := re.MatchString(c.path); got != c.want {
			t.Errorf("glob %q on %q = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}
