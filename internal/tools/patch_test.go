package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeWS(t *testing.T, env *Env, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(env.Workspace, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readWS(t *testing.T, env *Env, p string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(env.Workspace, p))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func TestBeginPatchFormat(t *testing.T) {
	r, env := setup(t, ModeWorkspace, nil)
	writeWS(t, env, map[string]string{
		"main.go":  "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n\nfunc other() {\n\tprintln(\"hi\")\n}\n",
		"old.txt":  "bye\n",
		"move.txt": "a\nb\nc\n",
	})
	patch := `*** Begin Patch
*** Update File: main.go
@@ func other() {
-	println("hi")
+	println("other")
*** Add File: docs/new.md
+# New
+
+text
*** Delete File: old.txt
*** Update File: move.txt
*** Move to: moved/move.txt
@@
 a
-b
+B
 c
*** End Patch`
	res := run(t, r, env, "apply_patch", map[string]any{"patch": patch})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := readWS(t, env, "main.go"); got != "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n\nfunc other() {\n\tprintln(\"other\")\n}\n" {
		t.Fatalf("main.go:\n%s", got)
	}
	if got := readWS(t, env, "docs/new.md"); got != "# New\n\ntext\n" {
		t.Fatalf("new.md: %q", got)
	}
	if readWS(t, env, "old.txt") != "<missing>" || readWS(t, env, "move.txt") != "<missing>" {
		t.Fatal("delete or move left the old file")
	}
	if got := readWS(t, env, "moved/move.txt"); got != "a\nB\nc\n" {
		t.Fatalf("moved: %q", got)
	}
	for _, want := range []string{"M main.go", "A docs/new.md", "D old.txt", "R move.txt -> moved/move.txt"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("report %q lacks %q", res.Text, want)
		}
	}
}

func TestUnifiedDiff(t *testing.T) {
	r, env := setup(t, ModeWorkspace, nil)
	writeWS(t, env, map[string]string{
		"a.txt": "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n",
		"b.txt": "keep\n",
	})
	patch := `diff --git a/a.txt b/a.txt
index 111..222 100644
--- a/a.txt
+++ b/a.txt
@@ -1,3 +1,3 @@
 one
-two
+TWO
 three
@@ -8,3 +8,4 @@ seven
 eight
 nine
 ten
+eleven
--- /dev/null
+++ b/c.txt
@@ -0,0 +1,2 @@
+new
+file
--- a/b.txt
+++ /dev/null
@@ -1 +0,0 @@
-keep
`
	res := run(t, r, env, "apply_patch", map[string]any{"patch": patch})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := readWS(t, env, "a.txt"); got != "one\nTWO\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\neleven\n" {
		t.Fatalf("a.txt: %q", got)
	}
	if got := readWS(t, env, "c.txt"); got != "new\nfile\n" {
		t.Fatalf("c.txt: %q", got)
	}
	if readWS(t, env, "b.txt") != "<missing>" {
		t.Fatal("b.txt not deleted")
	}
}

func TestPatchToleratesWhitespaceAndShiftedLines(t *testing.T) {
	r, env := setup(t, ModeWorkspace, nil)
	writeWS(t, env, map[string]string{"x.py": "# header\n# more\ndef f():   \n    return 1\n"})
	// Line numbers are off by two and the context lacks trailing spaces.
	patch := "--- a/x.py\n+++ b/x.py\n@@ -1,2 +1,2 @@\n def f():\n-    return 1\n+    return 2\n"
	res := run(t, r, env, "apply_patch", map[string]any{"patch": patch})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := readWS(t, env, "x.py"); got != "# header\n# more\ndef f():\n    return 2\n" {
		t.Fatalf("x.py: %q", got)
	}
}

func TestFailedHunkWritesNothing(t *testing.T) {
	r, env := setup(t, ModeWorkspace, nil)
	writeWS(t, env, map[string]string{"a.txt": "alpha\nbeta\ngamma\n", "b.txt": "one\ntwo\n"})
	patch := `*** Begin Patch
*** Update File: a.txt
 alpha
-beta
+BETA
*** Update File: b.txt
 one
-three
+THREE
*** End Patch`
	res := run(t, r, env, "apply_patch", map[string]any{"patch": patch})
	if !res.IsError {
		t.Fatal("bad hunk applied")
	}
	for _, want := range []string{"b.txt", "hunk 1 does not apply", `line 2 is "two" where the patch expects "three"`, "Nothing was written"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("error %q lacks %q", res.Text, want)
		}
	}
	if readWS(t, env, "a.txt") != "alpha\nbeta\ngamma\n" {
		t.Fatal("a.txt changed although the patch failed")
	}
}

func TestPatchKeepsCRLFAndMissingNewline(t *testing.T) {
	r, env := setup(t, ModeWorkspace, nil)
	writeWS(t, env, map[string]string{"w.txt": "a\r\nb\r\nc\r\n", "n.txt": "x\ny"})
	res := run(t, r, env, "apply_patch", map[string]any{"patch": "*** Begin Patch\n*** Update File: w.txt\n a\n-b\n+B\n*** Update File: n.txt\n x\n-y\n+z\n*** End of File\n*** End Patch\n"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := readWS(t, env, "w.txt"); got != "a\r\nB\r\nc\r\n" {
		t.Fatalf("crlf: %q", got)
	}
	if got := readWS(t, env, "n.txt"); got != "x\nz" {
		t.Fatalf("no final newline: %q", got)
	}
	// A unified diff can add the missing newline.
	res = run(t, r, env, "apply_patch", map[string]any{"patch": "--- a/n.txt\n+++ b/n.txt\n@@ -1,2 +1,2 @@\n x\n-z\n\\ No newline at end of file\n+z\n"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := readWS(t, env, "n.txt"); got != "x\nz\n" {
		t.Fatalf("newline: %q", got)
	}
}

func TestPatchPermissions(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.txt")
	patch := "*** Begin Patch\n*** Add File: in.txt\n+x\n*** Add File: " + outside + "\n+y\n*** End Patch"
	r, env := setup(t, ModeWorkspace, nil)
	if res := run(t, r, env, "apply_patch", map[string]any{"patch": patch}); !res.IsError || !strings.Contains(res.Text, "denied") {
		t.Fatalf("patch outside the workspace ran unattended: %+v", res)
	}
	if res := run(t, r, env, "apply_patch", map[string]any{"patch": "*** Begin Patch\n*** Add File: in.txt\n+x\n*** End Patch"}); res.IsError {
		t.Fatalf("patch inside the workspace needed approval: %s", res.Text)
	}
	r, env = setup(t, ModeWorkspace, answer(true))
	if res := run(t, r, env, "apply_patch", map[string]any{"patch": patch}); res.IsError {
		t.Fatalf("approved patch failed: %s", res.Text)
	}
	if b, _ := os.ReadFile(outside); string(b) != "y\n" {
		t.Fatalf("outside file: %q", b)
	}
}

func TestPatchErrors(t *testing.T) {
	r, env := setup(t, ModeWorkspace, nil)
	writeWS(t, env, map[string]string{"a.txt": "a\n"})
	cases := map[string]string{
		"just some text": "unrecognised patch",
		"*** Begin Patch\n*** Add File: a.txt\n+x\n*** End Patch":           "already exists",
		"*** Begin Patch\n*** Delete File: nope.txt\n*** End Patch":         "does not exist",
		"*** Begin Patch\n*** Update File: nope.txt\n-a\n+b\n*** End Patch": "cannot update nope.txt",
		"*** Begin Patch\n*** Update File: a.txt\n*** End Patch":            "has no hunks",
		"*** Begin Patch\n*** Add File: b.txt\nno plus\n*** End Patch":      "start with +",
	}
	for patch, want := range cases {
		res := run(t, r, env, "apply_patch", map[string]any{"patch": patch})
		if !res.IsError || !strings.Contains(res.Text, want) {
			t.Errorf("patch %q: got %q, want error containing %q", patch, res.Text, want)
		}
	}
}

func TestPatchPicksTheMatchNearestTheLineNumber(t *testing.T) {
	r, env := setup(t, ModeWorkspace, nil)
	writeWS(t, env, map[string]string{"r.txt": "x\nsame\nx\nx\nx\nx\nsame\nx\n"})
	res := run(t, r, env, "apply_patch", map[string]any{"patch": "--- a/r.txt\n+++ b/r.txt\n@@ -7,1 +7,1 @@\n-same\n+changed\n"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if got := readWS(t, env, "r.txt"); got != "x\nsame\nx\nx\nx\nx\nchanged\nx\n" {
		t.Fatalf("r.txt: %q", got)
	}
}
