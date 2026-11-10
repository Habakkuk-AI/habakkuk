package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/app"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/testutil"
)

// world is a user's home with a source home in it and a place for the
// Habakkuk home, so "~/" references resolve the way they do for a user.
type world struct {
	t    *testing.T
	user string // the user's home directory
	src  string // the source home
	home string // the Habakkuk home (not created)
	from string
}

// newWorld copies a fixture into <user>/.hermes or <user>/.openclaw.
func newWorld(t *testing.T, from string) *world {
	t.Helper()
	user := filepath.Join(t.TempDir(), "ada")
	w := &world{t: t, user: user, src: filepath.Join(user, "."+from), home: filepath.Join(user, ".habakkuk"), from: from}
	if err := copyHome(w.src, filepath.Join("testdata", from)); err != nil {
		t.Fatal(err)
	}
	return w
}

// write puts a file in the source home.
func (w *world) write(rel, text string, mode os.FileMode) string {
	w.t.Helper()
	p := filepath.Join(w.src, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), mode); err != nil {
		w.t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // past the umask
		w.t.Fatal(err)
	}
	return p
}

// add appends to a file in the source home.
func (w *world) add(rel, text string) {
	w.t.Helper()
	p := filepath.Join(w.src, filepath.FromSlash(rel))
	old, err := os.ReadFile(p)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(p, append(old, text...), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) options(files string) Options {
	return Options{From: w.from, Source: w.src, UserHome: w.user, Files: files,
		Getenv: func(string) string { return "" }, Now: func() time.Time { return testNow }}
}

func (w *world) plan(o Options) *Plan {
	w.t.Helper()
	f, err := Read(o)
	if err != nil {
		w.t.Fatal(err)
	}
	p, err := NewPlan(f, w.home, o)
	if err != nil {
		w.t.Fatal(err)
	}
	return p
}

// run plans and applies, failing the test on any error.
func (w *world) run(files string) (*Plan, *Result) {
	w.t.Helper()
	p := w.plan(w.options(files))
	res, err := p.Apply(context.Background(), true)
	if err != nil {
		w.t.Fatal(err)
	}
	for it, err := range res.Failed {
		w.t.Errorf("%s %s failed: %v", it.Kind, it.Name, err)
	}
	return p, res
}

// hermesWorld is a Hermes Agent home whose persona, a skill, schedules and
// scripts name the user's files in it by every form of its path, and name
// parts of the agent itself too.
func hermesWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t, Hermes)
	w.write("workspaces/briefs/monday.md", "Brief for Monday.\n", 0o644)
	w.write("workspaces/reports/q3.md", "Quarterly numbers.\n", 0o644)
	w.write("state/inbox.json", `{"last_uid": 4411}`+"\n", 0o600)
	w.write("state/gateway.lifecycle.json", `{"pid": 4242}`+"\n", 0o600) // the agent's own, in the user's folder
	w.write("notes.txt", "Call the plumber.\n", 0o644)
	w.write("scripts/inbox_sync.py", `#!/usr/bin/env python3
import json, os
STATE = "`+filepath.Join(w.src, "state", "inbox.json")+`"
OUT = os.path.expanduser("~/.hermes/workspaces/briefs")
ENV = os.path.expanduser("~/.hermes/.env")
print(json.load(open(STATE)))
`, 0o755)
	w.write("scripts/report.sh", `#!/bin/sh
. "$HOME/.hermes/.env"
"$HOME/.hermes/hermes-agent/venv/bin/hermes" chat -q "weekly report" > "${HOME}/.hermes/workspaces/reports/weekly.md"
`, 0o755)
	// The agent's own parts.
	w.write("hermes-agent/venv/bin/hermes", "#!/bin/sh\n", 0o755)
	w.write("logs/gateway.log", "old log\n", 0o644)
	w.write("state.db", "sqlite", 0o600)
	w.write("platforms/whatsapp/session/creds.json", "{}", 0o600)
	w.write("gateway.pid", "4242\n", 0o644)

	w.add("SOUL.md", `
Your briefs live in ~/.hermes/workspaces/briefs and reports go to ~/.hermes/workspaces/reports.
Scripts you may run are in ~/.hermes/scripts. My notes are in ~/.hermes/notes.txt.
Never edit ~/.hermes/config.yaml, and leave ~/.hermes/logs alone. Write drafts to ~/.hermes/drafts.
`)
	w.add("skills/research/notes-helper/SKILL.md", `
Save each summary under `+filepath.Join(w.src, "workspaces", "briefs")+`, and read the last position from ~/.hermes/state/inbox.json.
`)
	w.write("cron/jobs.json", `{"jobs": [
  {"id": "a1", "name": "Morning brief", "prompt": "Write the brief to ~/.hermes/workspaces/briefs. The session database is ~/.hermes/state.db.",
   "schedule": {"kind": "cron", "expr": "0 8 * * 1-5"}, "deliver": "telegram:4242", "enabled": true},
  {"id": "a2", "name": "Inbox sync", "prompt": "", "script": "inbox_sync.py", "no_agent": true,
   "schedule": {"kind": "interval", "minutes": 30}, "deliver": "local", "enabled": true},
  {"id": "a3", "name": "Weekly report", "prompt": "Check the report in ${HOME}/.hermes/workspaces/reports.", "script": "report.sh",
   "schedule": {"kind": "cron", "expr": "0 17 * * 5"}, "deliver": "telegram", "enabled": true}
]}`, 0o644)
	return w
}

func entryNames(list []*FileEntry) []string {
	return names(list, func(e *FileEntry) string { return e.Name })
}

func entry(p *Plan, name string) *FileEntry {
	if p.Files == nil {
		return nil
	}
	for _, e := range append(slices.Clone(p.Files.Moves), p.Files.Stays...) {
		if e.Name == name {
			return e
		}
	}
	return nil
}

// tree lists a folder: path -> "mode size mtime" and, for small files,
// their contents.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		line := info.Mode().String()
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			to, _ := os.Readlink(p)
			line += " -> " + to
		case info.Mode().IsRegular():
			data, _ := os.ReadFile(p)
			line += " " + info.ModTime().UTC().Format(time.RFC3339Nano) + " " + string(data)
		}
		out[filepath.ToSlash(rel)] = line
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return out
}

func sameTree(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s: %s changed:\n  was %q\n  now %q", what, k, v, after[k])
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			t.Errorf("%s: %s appeared", what, k)
		}
	}
}

func TestFilesPlanSortsEntries(t *testing.T) {
	w := hermesWorld(t)
	p := w.plan(w.options(FilesCopy))
	if p.Files == nil {
		t.Fatal("the plan has no Files section")
	}
	// The user's own data moves; "drafts" is named but not there yet.
	if got, want := entryNames(p.Files.Moves), []string{"drafts", "notes.txt", "scripts", "state", "workspaces"}; !slices.Equal(got, want) {
		t.Errorf("moves %v, want %v", got, want)
	}
	// Parts of the agent stay, each with a reason. hermes-agent and .env
	// are only named inside the scripts that get copied.
	wantStay := map[string]string{
		".env":         "keys and sign-ins",
		"config.yaml":  "config",
		"hermes-agent": "own program",
		"logs":         "logs",
		"state.db":     "sessions and databases",
	}
	if got := entryNames(p.Files.Stays); len(got) != len(wantStay) {
		t.Errorf("stays %v, want %v", got, sortedKeys(wantStay))
	}
	for name, why := range wantStay {
		if e := entry(p, name); e == nil || !strings.Contains(e.Why, why) || e.Dest != "" {
			t.Errorf("%s: %+v, want it to stay because of its %s", name, e, why)
		}
	}
	for name, want := range map[string]struct {
		kind  string
		refs  int
		files int
	}{
		"workspaces": {"folder", 7, 2}, // SOUL 2, skill 1, two prompts 2, two scripts 2
		"scripts":    {"folder", 3, 3}, // SOUL 1, two schedule commands (digest.py from the fixture is the third file)
		"state":      {"folder", 2, 1}, // skill 1, inbox_sync.py 1
		"notes.txt":  {"file", 1, 1},
		"drafts":     {"missing", 1, 0},
	} {
		e := entry(p, name)
		if e == nil || e.Kind != want.kind || e.Refs != want.refs || e.Stats.Files != want.files {
			t.Errorf("%s: %+v, want a %s named %d times holding %d files", name, e, want.kind, want.refs, want.files)
			continue
		}
		if e.Dest != filepath.Join(w.home, "files", name) {
			t.Errorf("%s goes to %s", name, e.Dest)
		}
	}
	if e := entry(p, "scripts"); e.InsideFiles != 2 || e.InsideRefs != 3 {
		t.Errorf("scripts: %d references in %d files to rewrite, want 3 in 2", e.InsideRefs, e.InsideFiles)
	}
	if e := entry(p, "hermes-agent"); !slices.Equal(e.Where, []string{"scripts/report.sh"}) {
		t.Errorf("hermes-agent is named by %v", e.Where)
	}

	var out bytes.Buffer
	p.Write(&out, "")
	for _, want := range []string{
		"Files in " + w.src + " that the imported text names",
		" in 2 files, named 7 times -> " + filepath.Join(w.home, "files", "workspaces"),
		"+ drafts: not there yet, so only the references move, named once",
		"Inside the copied files, 3 references in 2 files will name the copies too.",
		"Not moved (" + w.src + " is still needed for these):",
		"- hermes-agent: named once (scripts/report.sh): Hermes Agent's own program",
		"is only read.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the plan lacks %q:\n%s", want, out.String())
		}
	}
	// Planning reads; it writes nothing.
	if _, err := os.Stat(w.home); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("planning created %s", w.home)
	}
}

func TestFilesCopyRewritesReferences(t *testing.T) {
	w := hermesWorld(t)
	before := tree(t, w.src)
	p, res := w.run(FilesCopy)
	sameTree(t, "the source home", before, tree(t, w.src))

	files := filepath.Join(w.home, "files")
	// Only the user's own entries were copied.
	got, _ := os.ReadDir(files)
	if names := names(got, func(d fs.DirEntry) string { return d.Name() }); !slices.Equal(names, []string{"notes.txt", "scripts", "state", "workspaces"}) {
		t.Errorf("files/ holds %v", names)
	}
	if res.Files == nil || res.Files.Totals().Copy != 7 {
		t.Errorf("copied %+v, want 7 files", res.Files)
	}
	// The gateway's own file in the state folder stays behind.
	if _, err := os.Stat(filepath.Join(files, "state", "gateway.lifecycle.json")); err == nil || res.Files.Stats["state"].Internal != 1 {
		t.Errorf("state: %+v; the agent's own file must not be copied", res.Files.Stats["state"])
	}

	has := func(what, text string, want []string, not []string) {
		t.Helper()
		for _, s := range want {
			if !strings.Contains(text, s) {
				t.Errorf("%s lacks %q:\n%s", what, s, text)
			}
		}
		for _, s := range not {
			if strings.Contains(text, s) {
				t.Errorf("%s still has %q:\n%s", what, s, text)
			}
		}
	}
	// A persona file: "~/" stays "~/", and what did not move is untouched.
	has("SOUL.md", readFile(t, filepath.Join(w.home, "agents", "habakkuk", "SOUL.md")),
		[]string{"~/.habakkuk/files/workspaces/briefs", "~/.habakkuk/files/workspaces/reports", "~/.habakkuk/files/scripts.",
			"~/.habakkuk/files/notes.txt", "~/.habakkuk/files/drafts", "~/.hermes/config.yaml", "~/.hermes/logs"},
		[]string{"~/.hermes/workspaces", "~/.hermes/scripts", "~/.hermes/notes.txt", "~/.hermes/drafts"})
	// A skill file: the full path becomes the new full path.
	has("the skill", readFile(t, filepath.Join(w.home, "skills", "notes-helper", "SKILL.md")),
		[]string{filepath.Join(files, "workspaces", "briefs") + ",", "~/.habakkuk/files/state/inbox.json"},
		[]string{w.src, "~/.hermes"})
	// Schedules: the command and the prompt.
	byName := map[string]string{}
	for _, sc := range listSchedules(t, w.home) {
		byName[sc.Name] = sc.Command + "\n" + sc.Prompt
	}
	has("the Inbox sync schedule", byName["Inbox sync"], []string{"python3 " + shellQuote(filepath.Join(files, "scripts", "inbox_sync.py"))}, []string{w.src})
	has("the Morning brief schedule", byName["Morning brief"], []string{"~/.habakkuk/files/workspaces/briefs", "~/.hermes/state.db"}, []string{"~/.hermes/workspaces"})
	has("the Weekly report schedule", byName["Weekly report"],
		[]string{"bash " + shellQuote(filepath.Join(files, "scripts", "report.sh")), "${HOME}/.habakkuk/files/workspaces/reports"}, []string{w.src, ".hermes/"})
	// Inside a copied script: its own home path, in each form it used.
	has("the copied inbox_sync.py", readFile(t, filepath.Join(files, "scripts", "inbox_sync.py")),
		[]string{`STATE = "` + filepath.Join(files, "state", "inbox.json") + `"`, `"~/.habakkuk/files/workspaces/briefs"`, `"~/.hermes/.env"`},
		[]string{filepath.Join(w.src, "state"), "~/.hermes/workspaces"})
	has("the copied report.sh", readFile(t, filepath.Join(files, "scripts", "report.sh")),
		[]string{`"${HOME}/.habakkuk/files/workspaces/reports/weekly.md"`, `. "$HOME/.hermes/.env"`, `"$HOME/.hermes/hermes-agent/venv/bin/hermes"`}, nil)

	if st := res.Files.Stats["scripts"]; st.Rewritten != 2 || st.References != 3 {
		t.Errorf("rewrote %d references in %d copied files, want 3 in 2", st.References, st.Rewritten)
	}
	rewrote := map[string]int{}
	for _, it := range res.Done {
		rewrote[it.Kind] += it.Rewritten()
	}
	if want := map[string]int{"file": 5, "skill": 2, "schedule": 4}; rewrote["file"] != want["file"] || rewrote["skill"] != want["skill"] || rewrote["schedule"] != want["schedule"] {
		t.Errorf("references rewritten by kind: %v, want %v", rewrote, want)
	}
	// What still needs the old home, by entry.
	var still []string
	for _, s := range p.StillPoints() {
		still = append(still, s.Name)
	}
	if want := []string{".env", "config.yaml", "hermes-agent", "logs", "state.db"}; !slices.Equal(still, want) {
		t.Errorf("still points at %v, want %v", still, want)
	}

	// A second run finds nothing to do and changes nothing.
	homeBefore, filesBefore := tree(t, filepath.Join(w.home, "agents")), tree(t, files)
	cfg := readFile(t, filepath.Join(w.home, "config.yaml"))
	p2 := w.plan(w.options(FilesCopy))
	for _, it := range p2.Items {
		if it.Changes() {
			t.Errorf("second run would change %s %s (%s: %s)", it.Kind, it.Name, it.Status, it.Detail)
		}
	}
	if p2.Files.Pending() || p2.Files.Need != 0 || p2.Config != nil {
		t.Errorf("second run has files to copy (%d bytes) or a config to write", p2.Files.Need)
	}
	res2, err := p2.Apply(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if n := res2.Files.Totals().Copy; n != 0 || len(res2.Done) != 0 {
		t.Errorf("second run copied %d files and wrote %d items", n, len(res2.Done))
	}
	sameTree(t, "agents/", homeBefore, tree(t, filepath.Join(w.home, "agents")))
	sameTree(t, "files/", filesBefore, tree(t, files))
	if readFile(t, filepath.Join(w.home, "config.yaml")) != cfg {
		t.Error("config.yaml changed on the second run")
	}
	// So does a later run that does not ask for a copy.
	for _, mode := range []string{"", FilesSkip} {
		p3 := w.plan(w.options(mode))
		for _, it := range p3.Items {
			if it.Changes() || it.Status == Conflict {
				t.Errorf("a later run with Files %q: %s %s is %s (%s)", mode, it.Kind, it.Name, it.Status, it.Detail)
			}
		}
	}
	sameTree(t, "the source home", before, tree(t, w.src))
}

func TestFilesSkipCopiesNothing(t *testing.T) {
	for _, mode := range []string{FilesSkip, ""} {
		w := hermesWorld(t)
		p, res := w.run(mode)
		if res.Files != nil {
			t.Errorf("Files %q copied %+v", mode, res.Files)
		}
		for _, gone := range []string{"files", filesManifest} {
			if _, err := os.Stat(filepath.Join(w.home, gone)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("Files %q wrote %s", mode, gone)
			}
		}
		// The text is imported as it was.
		if soul := readFile(t, filepath.Join(w.home, "agents", "habakkuk", "SOUL.md")); !strings.Contains(soul, "~/.hermes/workspaces/briefs") || strings.Contains(soul, ".habakkuk") {
			t.Errorf("SOUL.md was rewritten:\n%s", soul)
		}
		for _, it := range res.Done {
			if it.Rewritten() != 0 {
				t.Errorf("%s %s reports %d rewritten references", it.Kind, it.Name, it.Rewritten())
			}
		}
		// And the report says what still needs the old home, the user's own
		// files included.
		still := map[string]string{}
		for _, s := range p.StillPoints() {
			still[s.Name] = s.Why
		}
		for _, name := range []string{"workspaces", "scripts", "state", "notes.txt"} {
			if still[name] != "your own files, not copied" {
				t.Errorf("Files %q: %s is reported as %q", mode, name, still[name])
			}
		}
		if still["config.yaml"] == "" || len(p.Files.Waiting()) != 5 {
			t.Errorf("Files %q: still %v, waiting %v", mode, still, entryNames(p.Files.Waiting()))
		}
		var out bytes.Buffer
		p.Write(&out, "")
		want := "habakkuk migrate --files copy brings them over"
		if mode == FilesSkip {
			want = "They will not be copied (--files skip)"
		}
		if !strings.Contains(out.String(), want) {
			t.Errorf("Files %q: the plan lacks %q:\n%s", mode, want, out.String())
		}
	}
}

// An import made without the copy still names the old home. A later run
// with the copy rewrites what it imported, where it is.
func TestFilesCopyAfterAnEarlierImport(t *testing.T) {
	w := hermesWorld(t)
	w.run("")
	ctx := context.Background()
	db, err := app.OpenControl(ctx, w.home)
	if err != nil {
		t.Fatal(err)
	}
	// The user has resumed a schedule since.
	if _, err := db.ExecContext(ctx, `UPDATE schedules SET enabled = 1 WHERE name = 'Inbox sync'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	ids := map[string]string{}
	for _, sc := range listSchedules(t, w.home) {
		ids[sc.Name] = sc.ID
	}

	p := w.plan(w.options(FilesCopy))
	for kind, name := range map[string]string{"file": "SOUL.md", "skill": "notes-helper", "schedule": "Inbox sync"} {
		if it := item(p, kind, name); it == nil || it.Status != Update {
			t.Errorf("%s %s: %+v, want it updated in place", kind, name, it)
		}
	}
	if n := len(p.Conflicts()); n != 0 {
		t.Errorf("%d conflicts; an unchanged earlier import is not a conflict", n)
	}
	res, err := p.Apply(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failed) != 0 || len(res.Paused) != 0 {
		t.Errorf("failed %v, newly paused %v", res.Failed, res.Paused)
	}
	files := filepath.Join(w.home, "files")
	for _, sc := range listSchedules(t, w.home) {
		if sc.ID != ids[sc.Name] {
			t.Errorf("schedule %q got a new ID", sc.Name)
		}
		if sc.Enabled != (sc.Name == "Inbox sync") {
			t.Errorf("schedule %q enabled = %v", sc.Name, sc.Enabled)
		}
		if strings.Contains(sc.Command, w.src) || sc.Name == "Inbox sync" && !strings.Contains(sc.Command, filepath.Join(files, "scripts")) {
			t.Errorf("schedule %q runs %q", sc.Name, sc.Command)
		}
	}
	if soul := readFile(t, filepath.Join(w.home, "agents", "habakkuk", "SOUL.md")); strings.Contains(soul, "~/.hermes/workspaces") || !strings.Contains(soul, "~/.habakkuk/files/scripts") {
		t.Errorf("SOUL.md:\n%s", soul)
	}
	if _, err := os.Stat(filepath.Join(w.home, "skills", "notes-helper.bak")); err == nil {
		t.Error("the rewritten skill left a .bak copy")
	}
	if skill := readFile(t, filepath.Join(w.home, "skills", "notes-helper", "SKILL.md")); strings.Contains(skill, w.src) {
		t.Errorf("the skill still names the old home:\n%s", skill)
	}
	// Text the user changed since is theirs: a conflict, left alone.
	soul := filepath.Join(w.home, "agents", "coder", "SOUL.md")
	if err := os.WriteFile(soul, []byte("My own words.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := status(w.plan(w.options(FilesCopy)), "file", "SOUL.md"); st != Same {
		t.Errorf("habakkuk's SOUL.md is %s after the copy", st)
	}
	p3 := w.plan(w.options(FilesCopy))
	if c := p3.Conflicts(); len(c) != 1 || c[0].Agent != "coder" {
		t.Errorf("conflicts: %+v", c)
	}
}

// What the user changed after an earlier import is theirs and is not
// replaced. But where it still names a folder that is copied now, that
// reference is rewritten, or the new agent would go on using the old home.
func TestFilesCopyFixesReferencesInConflicts(t *testing.T) {
	w := hermesWorld(t)
	w.write("scripts/local_mcp.py", "print('mcp')\n", 0o755)
	w.add("memories/MEMORY.md", "\n§\nReports are kept in ~/.hermes/workspaces/reports, one per quarter.\n")
	cfgPath := filepath.Join(w.src, "config.yaml")
	w.write("config.yaml", strings.Replace(readFile(t, cfgPath), `args: ["mcp-server-time"]`,
		`args: ["run", "~/.hermes/scripts/local_mcp.py"]`, 1), 0o644)
	w.run("")

	// The user's own changes, each still naming the old home.
	ctx := context.Background()
	edit := func(rel, old, new string) {
		t.Helper()
		path := filepath.Join(w.home, filepath.FromSlash(rel))
		text := readFile(t, path)
		if !strings.Contains(text, old) {
			t.Fatalf("%s lacks %q:\n%s", rel, old, text)
		}
		if err := os.WriteFile(path, []byte(strings.Replace(text, old, new, 1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	edit("agents/habakkuk/SOUL.md", "You keep answers short.", "You keep answers very short.")
	edit("skills/notes-helper/SKILL.md", "tidy summary", "tidy, dated summary")
	edit("agents/habakkuk/memory/reports-are-kept-in.md", "one per quarter.", "one per quarter. Drafts go to ~/.hermes/workspaces/drafts.")
	edit("config.yaml", "local_mcp.py", "local_mcp.py, --verbose")
	db, err := app.OpenControl(ctx, w.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE schedules SET enabled = 1, prompt = prompt || ' Then run ~/.hermes/scripts/report.sh.' WHERE name = 'Morning brief'`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	p := w.plan(w.options(FilesCopy))
	conflicts := map[string]string{"file": "SOUL.md", "skill": "notes-helper", "note": "Reports are kept in ~/", "mcp": "time", "schedule": "Morning brief"}
	for kind, name := range conflicts {
		if it := item(p, kind, name); it == nil || !it.Fixes() || it.Changes() || !strings.Contains(it.Detail, "references to the copied files rewritten") {
			t.Errorf("%s %s: %+v, want a conflict whose references are rewritten", kind, name, it)
		}
	}
	res, err := p.Apply(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failed) != 0 || len(res.Fixed) != len(conflicts) {
		t.Errorf("failed %v, fixed %d of %d", res.Failed, len(res.Fixed), len(conflicts))
	}
	// Each keeps the user's change and names the copies.
	for rel, want := range map[string][]string{
		"agents/habakkuk/SOUL.md":                       {"You keep answers very short.", "~/.habakkuk/files/workspaces/briefs", "~/.hermes/config.yaml"},
		"skills/notes-helper/SKILL.md":                  {"tidy, dated summary", "~/.habakkuk/files/state/inbox.json"},
		"agents/habakkuk/memory/reports-are-kept-in.md": {"kept in ~/.habakkuk/files/workspaces/reports,", "Drafts go to ~/.habakkuk/files/workspaces/drafts.", "imported"},
	} {
		text := readFile(t, filepath.Join(w.home, filepath.FromSlash(rel)))
		for _, s := range want {
			if !strings.Contains(text, s) {
				t.Errorf("%s lacks %q:\n%s", rel, s, text)
			}
		}
		for _, gone := range []string{"~/.hermes/workspaces", "~/.hermes/scripts", "~/.hermes/state/", w.src + string(filepath.Separator)} {
			if strings.Contains(text, gone) {
				t.Errorf("%s still names %q:\n%s", rel, gone, text)
			}
		}
	}
	cfg, err := config.Load(w.home)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.MCP["time"].Args, []string{"run", "~/.habakkuk/files/scripts/local_mcp.py", "--verbose"}; !slices.Equal(got, want) {
		t.Errorf("the MCP server's args are %v, want %v", got, want)
	}
	for _, sc := range listSchedules(t, w.home) {
		if sc.Name == "Morning brief" && (!sc.Enabled || !strings.HasSuffix(sc.Prompt, "Then run ~/.habakkuk/files/scripts/report.sh.") || strings.Contains(sc.Prompt, "~/.hermes/workspaces")) {
			t.Errorf("Morning brief: enabled %v, prompt %q", sc.Enabled, sc.Prompt)
		}
	}
	// They stay conflicts, with nothing more to do.
	p2 := w.plan(w.options(FilesCopy))
	if n := len(p2.Conflicts()); n != len(conflicts) {
		t.Errorf("%d conflicts on the next run, want %d", n, len(conflicts))
	}
	for _, it := range p2.Items {
		if it.Changes() || it.Fixes() {
			t.Errorf("the next run would change %s %s (%s: %s)", it.Kind, it.Name, it.Status, it.Detail)
		}
	}
}

// Memory notes, the profile and MCP servers are rewritten too, both when
// they are first imported and when they were imported before the copy.
func TestFilesRewriteNotesProfileAndMCP(t *testing.T) {
	for _, earlier := range []bool{false, true} {
		w := newWorld(t, Hermes)
		w.write("workspaces/reports/q3.md", "Quarterly numbers.\n", 0o644)
		w.write("scripts/local_mcp.py", "print('mcp')\n", 0o755)
		w.add("memories/MEMORY.md", "\n§\nReports are kept in ~/.hermes/workspaces/reports, one per quarter.\n")
		w.add("memories/USER.md", "\n§\nReads the briefs in ~/.hermes/workspaces every morning.\n")
		cfgPath := filepath.Join(w.src, "config.yaml")
		w.write("config.yaml", strings.Replace(readFile(t, cfgPath), `args: ["mcp-server-time"]`,
			`args: ["run", "~/.hermes/scripts/local_mcp.py", "--log", "~/.hermes/logs/mcp.log"]`, 1), 0o644)
		if earlier {
			w.run("")
			if cfg, err := config.Load(w.home); err != nil || !slices.Contains(cfg.MCP["time"].Args, "~/.hermes/scripts/local_mcp.py") {
				t.Fatalf("the earlier import: %v", err)
			}
		}
		p, res := w.run(FilesCopy)
		want := New
		if earlier {
			want = Update
		}
		for kind, name := range map[string]string{"note": "Reports are kept in ~/", "profile": "USER.md", "mcp": "time"} {
			if it := item(p, kind, name); it == nil || it.Status != want || it.Rewritten() == 0 {
				t.Errorf("earlier %v: %s %s is %+v, want %s with references rewritten", earlier, kind, name, it, want)
			}
		}
		if len(res.Failed) != 0 || len(p.Conflicts()) != 0 {
			t.Errorf("earlier %v: failed %v, conflicts %v", earlier, res.Failed, p.Conflicts())
		}
		note := readFile(t, filepath.Join(w.home, "agents", "habakkuk", "memory", "reports-are-kept-in.md"))
		if !strings.Contains(note, "kept in ~/.habakkuk/files/workspaces/reports,") || strings.Contains(note, "kept in ~/.hermes") {
			t.Errorf("earlier %v: the note:\n%s", earlier, note)
		}
		user := readFile(t, filepath.Join(w.home, "agents", "habakkuk", "USER.md"))
		if !strings.Contains(user, "briefs in ~/.habakkuk/files/workspaces every") || strings.Contains(user, "~/.hermes") || strings.Count(user, "Reads the briefs") != 1 {
			t.Errorf("earlier %v: USER.md:\n%s", earlier, user)
		}
		cfg, err := config.Load(w.home)
		if err != nil {
			t.Fatal(err)
		}
		// The script moved; the old agent's log folder did not.
		if got, want := cfg.MCP["time"].Args, []string{"run", "~/.habakkuk/files/scripts/local_mcp.py", "--log", "~/.hermes/logs/mcp.log"}; !slices.Equal(got, want) {
			t.Errorf("earlier %v: the MCP server's args are %v", earlier, got)
		}
		for _, it := range w.plan(w.options(FilesCopy)).Items {
			if it.Changes() {
				t.Errorf("earlier %v: the next run would change %s %s (%s: %s)", earlier, it.Kind, it.Name, it.Status, it.Detail)
			}
		}
	}
}

func TestFilesCopyKeepsModesTimesAndLinks(t *testing.T) {
	testutil.UnixOnly(t, "permission bits, symlinks and named pipes")
	w := hermesWorld(t)
	old := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	script := w.write("scripts/tool.sh", "#!/bin/sh\necho hi\n", 0o750)
	secret := w.write("state/token.json", "{}", 0o400)
	big := w.write("workspaces/reports/data.bin", strings.Repeat("\x00\x01", 4096), 0o644)
	for _, p := range []string{script, secret, big} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	// A user's repository is kept whole.
	w.write("workspaces/repo/.git/config", "[core]\n\tworktree = "+filepath.Join(w.src, "workspaces", "repo")+"\n", 0o644)
	w.write("workspaces/repo/.git/objects/ab/cdef", "x\x00y", 0o444)
	dir := filepath.Join(w.src, "workspaces", "private")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	w.write("workspaces/private/plan.md", "Plan.\n", 0o640)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	// Links: one inside the entry, one to another entry that moves, one out
	// of the source home (to a file that must not be copied through it).
	outside := filepath.Join(w.user, "outside.txt")
	if err := os.WriteFile(outside, []byte("not yours to copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		"workspaces/reports/latest.md": "../briefs/monday.md",
		"workspaces/state-link":        filepath.Join(w.src, "state", "inbox.json"),
		"workspaces/out-link":          outside,
		"workspaces/dir-link":          "briefs",
	}
	for rel, to := range links {
		if err := os.Symlink(to, filepath.Join(w.src, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
	}
	if err := mkfifo(filepath.Join(w.src, "workspaces", "queue.fifo")); err != nil {
		t.Fatal(err)
	}
	// An entry that is itself a link to a folder kept elsewhere: the link
	// is copied, what it points to is not.
	shared := filepath.Join(w.user, "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "big.txt"), []byte("kept elsewhere"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Join(w.src, "shared")); err != nil {
		t.Fatal(err)
	}
	w.add("SOUL.md", "Shared files are in ~/.hermes/shared.\n")
	before := tree(t, w.src)

	p, res := w.run(FilesCopy)
	sameTree(t, "the source home", before, tree(t, w.src))
	files := filepath.Join(w.home, "files")
	for rel, want := range map[string]os.FileMode{
		"scripts/tool.sh": 0o750, "state/token.json": 0o400, "scripts/inbox_sync.py": 0o755,
		"state/inbox.json": 0o600, "workspaces/private/plan.md": 0o640, "workspaces/repo/.git/objects/ab/cdef": 0o444,
	} {
		info, err := os.Stat(filepath.Join(files, rel))
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: mode %v (%v), want %v", rel, info.Mode().Perm(), err, want)
		}
	}
	for _, rel := range []string{"scripts/tool.sh", "state/token.json", "workspaces/reports/data.bin", "workspaces/private"} {
		info, err := os.Stat(filepath.Join(files, rel))
		if err != nil || !info.ModTime().Equal(old) {
			t.Errorf("%s: modified %v (%v), want %v", rel, info.ModTime(), err, old)
		}
	}
	if info, _ := os.Stat(filepath.Join(files, "workspaces", "private")); info.Mode().Perm() != 0o750 {
		t.Errorf("the folder's mode is %v", info.Mode().Perm())
	}
	// A rewritten file keeps its time too.
	src, _ := os.Stat(filepath.Join(w.src, "scripts", "inbox_sync.py"))
	if dst, _ := os.Stat(filepath.Join(files, "scripts", "inbox_sync.py")); !dst.ModTime().Equal(src.ModTime()) || dst.Size() == src.Size() {
		t.Errorf("inbox_sync.py: time %v size %d, source %v size %d", dst.ModTime(), dst.Size(), src.ModTime(), src.Size())
	}
	for rel, want := range map[string]string{
		"workspaces/reports/latest.md": "../briefs/monday.md",
		"workspaces/state-link":        filepath.Join(files, "state", "inbox.json"), // it follows the entry that moved
		"workspaces/out-link":          outside,                                     // kept as it is
		"workspaces/dir-link":          "briefs",
	} {
		p := filepath.Join(files, filepath.FromSlash(rel))
		info, err := os.Lstat(p)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			t.Errorf("%s is not a link (%v)", rel, err)
			continue
		}
		if to, _ := os.Readlink(p); to != want {
			t.Errorf("%s points to %q, want %q", rel, to, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(files, "workspaces", "queue.fifo")); err == nil {
		t.Error("the named pipe was copied")
	}
	if e := entry(p, "shared"); e == nil || e.Kind != "link" || e.LinkTo != shared {
		t.Errorf("shared: %+v", e)
	}
	if info, err := os.Lstat(filepath.Join(files, "shared")); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("files/shared is not a link (%v)", err)
	} else if to, _ := os.Readlink(filepath.Join(files, "shared")); to != shared {
		t.Errorf("files/shared points to %q", to)
	}
	if soul := readFile(t, filepath.Join(w.home, "agents", "habakkuk", "SOUL.md")); !strings.Contains(soul, "~/.habakkuk/files/shared.") {
		t.Errorf("SOUL.md:\n%s", soul)
	}
	if st := res.Files.Stats["workspaces"]; st.Special != 1 || st.Links != 4 || st.MakeLinks != 4 {
		t.Errorf("workspaces: %+v", st)
	}
	// The repository's text config follows the move; its objects are bytes.
	if cfg := readFile(t, filepath.Join(files, "workspaces", "repo", ".git", "config")); !strings.Contains(cfg, "worktree = "+filepath.Join(files, "workspaces", "repo")) {
		t.Errorf(".git/config:\n%s", cfg)
	}
	if obj := readFile(t, filepath.Join(files, "workspaces", "repo", ".git", "objects", "ab", "cdef")); obj != "x\x00y" {
		t.Errorf("the git object changed: %q", obj)
	}

	// Running again copies nothing and touches nothing.
	after := tree(t, files)
	_, res2 := w.run(FilesCopy)
	if tot := res2.Files.Totals(); tot.Copy != 0 || tot.MakeLinks != 0 {
		t.Errorf("second run copied %d files and %d links", tot.Copy, tot.MakeLinks)
	}
	sameTree(t, "files/", after, tree(t, files))

	// The new agent now works in the copy. A file it changed is kept, a
	// file it added is never deleted, and a file that changed in the source
	// since comes over.
	live := filepath.Join(files, "state", "inbox.json")
	if err := os.WriteFile(live, []byte(`{"last_uid": 9000}`), 0o600); err != nil {
		t.Fatal(err)
	}
	added := filepath.Join(files, "workspaces", "briefs", "tuesday.md")
	if err := os.WriteFile(added, []byte("New.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	w.write("workspaces/reports/q3.md", "Quarterly numbers, corrected.\n", 0o644)
	if err := os.Chtimes(filepath.Join(w.src, "workspaces", "reports", "q3.md"), later, later); err != nil {
		t.Fatal(err)
	}
	w.write("workspaces/reports/q4.md", "Next quarter.\n", 0o644)
	p3 := w.plan(w.options(FilesCopy))
	if e := entry(p3, "workspaces"); e.Stats.Copy != 2 || !p3.Files.Pending() {
		t.Errorf("workspaces: %d files to copy, want the 2 that changed", e.Stats.Copy)
	}
	if e := entry(p3, "state"); e.Stats.Newer != 1 || e.Stats.Copy != 0 {
		t.Errorf("state: %+v, want the changed copy kept", e.Stats)
	}
	res3, err := p3.Apply(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if n := res3.Files.Totals().Copy; n != 2 {
		t.Errorf("third run copied %d files, want 2", n)
	}
	if got := readFile(t, live); got != `{"last_uid": 9000}` {
		t.Errorf("the changed copy was replaced: %s", got)
	}
	if got := readFile(t, added); got != "New.\n" {
		t.Errorf("the added file is %q", got)
	}
	if got := readFile(t, filepath.Join(files, "workspaces", "reports", "q3.md")); !strings.Contains(got, "corrected") {
		t.Errorf("the changed source file did not come over: %q", got)
	}
}

// A copy that was stopped part way goes on from where it stopped.
func TestFilesCopyResumes(t *testing.T) {
	w := hermesWorld(t)
	for i := range 20 {
		w.write(filepath.ToSlash(filepath.Join("workspaces", "bulk", string(rune('a'+i))+".md")), "x\n", 0o644)
	}
	p := w.plan(w.options(FilesCopy))
	total := entry(p, "workspaces").Stats.Copy
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Apply(ctx, true); err == nil || !strings.Contains(err.Error(), "goes on from there") {
		t.Fatalf("a stopped copy returned %v", err)
	}
	// Nothing else was written: no config, no agents, no record of a copy.
	for _, name := range []string{"config.yaml", "agents", filesManifest} {
		if _, err := os.Stat(filepath.Join(w.home, name)); err == nil {
			t.Errorf("a stopped copy wrote %s", name)
		}
	}
	// Half of one folder is there, as an interrupted run would leave it.
	half := filepath.Join(w.home, "files", "workspaces", "bulk")
	if err := os.MkdirAll(half, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		src := filepath.Join(w.src, "workspaces", "bulk", name)
		info, _ := os.Stat(src)
		dst := filepath.Join(half, name)
		if err := os.WriteFile(dst, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dst, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
	}
	// And one file was cut short while it was being written.
	if err := os.WriteFile(filepath.Join(half, "d.md"+partSuffix), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p2, res := w.run(FilesCopy)
	if got := entry(p2, "workspaces").Stats.Copy; got != total-3 {
		t.Errorf("the second run planned %d files, want %d (3 were there)", got, total-3)
	}
	if got := res.Files.Stats["workspaces"].Copy; got != total-3 {
		t.Errorf("the second run copied %d files, want %d", got, total-3)
	}
	if got := readFile(t, filepath.Join(half, "d.md")); got != "x\n" {
		t.Errorf("d.md = %q", got)
	}
	if _, err := os.Stat(filepath.Join(half, "d.md"+partSuffix)); err == nil {
		t.Error("the part file was left behind")
	}
}

func TestFilesCopyRefusedWithoutRoom(t *testing.T) {
	w := hermesWorld(t)
	o := w.options(FilesCopy)
	var asked string
	o.FreeSpace = func(dir string) (uint64, error) {
		asked = dir
		return 1 << 20, nil // less than the margin a copy keeps free
	}
	p := w.plan(o)
	if p.Files.Err == nil || !strings.Contains(p.Files.Err.Error(), "not enough room") {
		t.Fatalf("Files.Err = %v", p.Files.Err)
	}
	if asked != filepath.Join(w.home, "files") {
		t.Errorf("asked for the room at %q", asked)
	}
	var out bytes.Buffer
	p.Write(&out, "")
	if !strings.Contains(out.String(), "Problem: not enough room in "+filepath.Join(w.home, "files")) {
		t.Errorf("the plan does not say so:\n%s", out.String())
	}
	if _, err := p.Apply(context.Background(), true); err == nil || !strings.Contains(err.Error(), "not enough room") {
		t.Errorf("Apply returned %v", err)
	}
	if _, err := os.Stat(w.home); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused copy created %s", w.home)
	}
	// With room, or without the copy, it goes through.
	o.FreeSpace = func(string) (uint64, error) { return 1 << 40, nil }
	if p := w.plan(o); p.Files.Err != nil {
		t.Errorf("with room: %v", p.Files.Err)
	}
	if p := w.plan(w.options(FilesSkip)); p.Files.Err != nil {
		t.Errorf("without the copy: %v", p.Files.Err)
	}
	// The real check answers for a folder that does not exist yet.
	if free, err := freeSpace(filepath.Join(w.home, "files", "deep")); err != nil || free == 0 {
		t.Errorf("freeSpace = %d, %v", free, err)
	}
}

// Folder names with spaces move like any other. A reference that hides
// part of such a name (an escaped space) is left for the user: moving the
// half that can be read would break it.
func TestFilesNamesWithSpaces(t *testing.T) {
	w := newWorld(t, Hermes)
	w.write("my notes/todo.md", "Todo.\n", 0o644)
	w.write("old reports/q1.md", "Q1.\n", 0o644)
	w.add("SOUL.md", "\nYour notes are in \"~/.hermes/my notes/todo.md\". Old ones: ~/.hermes/old\\ reports/q1.md\n")
	p, _ := w.run(FilesCopy)
	if got := entryNames(p.Files.Moves); !slices.Equal(got, []string{"my notes", "scripts"}) {
		t.Errorf("moves %v", got)
	}
	if e := entry(p, "old"); e == nil || !strings.Contains(e.Why, `did the text mean "old reports"?`) {
		t.Errorf("old: %+v", e)
	}
	if got := readFile(t, filepath.Join(w.home, "files", "my notes", "todo.md")); got != "Todo.\n" {
		t.Errorf("the copy holds %q", got)
	}
	soul := readFile(t, filepath.Join(w.home, "agents", "habakkuk", "SOUL.md"))
	if !strings.Contains(soul, `"~/.habakkuk/files/my notes/todo.md"`) || !strings.Contains(soul, `~/.hermes/old\ reports/q1.md`) {
		t.Errorf("SOUL.md:\n%s", soul)
	}
}

// On Windows a reference may spell a folder in any letter case; the entry
// and its copy are named the way the folder is.
func TestRealName(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "Profiles", "Coder", "Scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"profiles/coder/scripts": "Profiles/Coder/Scripts",
		"profiles/coder/state":   "Profiles/Coder/state", // not there: kept as written
		"workspaces":             "workspaces",
	} {
		if got := realName(dir, key); got != want {
			t.Errorf("realName(%q) = %q, want %q", key, got, want)
		}
	}
}

// A copy made before another entry moved is brought up to date: a script
// that was copied as it was is rewritten once the file it names moves.
func TestFilesCopyCatchesUpWhenMoreMoves(t *testing.T) {
	w := newWorld(t, Hermes)
	w.write("scripts/push.sh", "#!/bin/sh\ncat ~/.hermes/all.csv\n", 0o755)
	w.write("all.csv", strings.Repeat("1,2,3\n", 200), 0o644)
	w.add("SOUL.md", "\nRun ~/.hermes/scripts/push.sh when asked.\n")
	o := w.options(FilesCopy)
	o.FilesMax = 512 // all.csv is over the limit, so it stays
	p := w.plan(o)
	if e := entry(p, "all.csv"); e == nil || !strings.Contains(e.Why, "--files-max") {
		t.Fatalf("all.csv: %+v", e)
	}
	if _, err := p.Apply(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(w.home, "files", "scripts", "push.sh")
	if got := readFile(t, copied); !strings.Contains(got, "cat ~/.hermes/all.csv") {
		t.Fatalf("push.sh was rewritten to name a file that did not move:\n%s", got)
	}
	// With the limit raised the file moves, and the script that names it
	// is copied again although its size and time did not change.
	o.FilesMax = -1
	p2 := w.plan(o)
	if e := entry(p2, "scripts"); e.Stats.Copy != 1 || e.InsideRefs != 1 {
		t.Errorf("scripts: %d files to copy and %d references inside, want 1 and 1", e.Stats.Copy, e.InsideRefs)
	}
	if _, err := p2.Apply(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, copied); !strings.Contains(got, "cat ~/.habakkuk/files/all.csv") {
		t.Errorf("push.sh:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(w.home, "files", "all.csv")); err != nil {
		t.Error(err)
	}
	if p3 := w.plan(o); p3.Files.Pending() {
		t.Error("a third run has files to copy")
	}
}

func TestFilesMaxAndFilesDir(t *testing.T) {
	w := hermesWorld(t)
	w.write("workspaces/reports/video.bin", strings.Repeat("v", 5000), 0o644)
	w.write("dump.bin", strings.Repeat("d", 5000), 0o644)
	w.add("SOUL.md", "The dump is ~/.hermes/dump.bin.\n")
	dir := filepath.Join(w.user, "elsewhere")
	o := w.options(FilesCopy)
	o.FilesMax, o.FilesDir = 4096, dir
	p := w.plan(o)
	if e := entry(p, "dump.bin"); e == nil || !strings.Contains(e.Why, "--files-max") {
		t.Errorf("dump.bin: %+v, want it to stay as too large", e)
	}
	ws := entry(p, "workspaces")
	if !slices.Equal(ws.Stats.TooBig, []string{"reports/video.bin"}) || ws.Dest != filepath.Join(dir, "workspaces") {
		t.Errorf("workspaces: too big %v, dest %s", ws.Stats.TooBig, ws.Dest)
	}
	var out bytes.Buffer
	p.Write(&out, "")
	if !strings.Contains(out.String(), "leaves out 1 file over 4 KB (--files-max raises the limit): reports/video.bin") {
		t.Errorf("the plan does not name the file left out:\n%s", out.String())
	}
	if _, err := p.Apply(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "workspaces", "reports", "video.bin")); err == nil {
		t.Error("the file over the limit was copied")
	}
	if _, err := os.Stat(filepath.Join(w.home, "files")); err == nil {
		t.Error("files/ was created although --files-dir points elsewhere")
	}
	// "~/" is kept: the new place is still under the user's home.
	if soul := readFile(t, filepath.Join(w.home, "agents", "habakkuk", "SOUL.md")); !strings.Contains(soul, "~/elsewhere/workspaces/briefs") || !strings.Contains(soul, "~/.hermes/dump.bin") {
		t.Errorf("SOUL.md:\n%s", soul)
	}
	// The record of where things went is kept, so a later run without the
	// flags agrees with this one.
	var m manifest
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(w.home, filesManifest))), &m); err != nil || m.Sources[w.src]["workspaces"] != filepath.Join(dir, "workspaces") {
		t.Errorf("manifest %+v (%v)", m, err)
	}
	p2 := w.plan(w.options(""))
	for _, it := range p2.Items {
		if it.Changes() || it.Status == Conflict {
			t.Errorf("a later plain run: %s %s is %s (%s)", it.Kind, it.Name, it.Status, it.Detail)
		}
	}
	// No limit takes the large file too.
	o.FilesMax = -1
	if _, err := w.plan(o).Apply(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "workspaces", "reports", "video.bin")); err != nil {
		t.Errorf("with no limit: %v", err)
	}

	// A destination inside the source home is refused.
	w2 := hermesWorld(t)
	o2 := w2.options(FilesCopy)
	o2.FilesDir = filepath.Join(w2.src, "copies")
	if p := w2.plan(o2); p.Files.Err == nil || !strings.Contains(p.Files.Err.Error(), "overlaps") {
		t.Errorf("a destination inside the source: %v", p.Files.Err)
	}
}

// A profile is a home inside the home: its scripts are its own entry.
func TestFilesInAProfileHome(t *testing.T) {
	w := newWorld(t, Hermes)
	w.write("profiles/coder/scripts/lint.sh", "#!/bin/sh\ncat ~/.hermes/profiles/coder/state/last\n", 0o755)
	w.write("profiles/coder/state/last", "ok\n", 0o644)
	w.write("profiles/coder/state.db", "sqlite", 0o600)
	w.write("profiles/coder/cron/jobs.json", `{"jobs": [{"id": "c1", "name": "Lint", "prompt": "", "script": "lint.sh", "no_agent": true,
		"schedule": {"kind": "interval", "minutes": 60}, "enabled": true}]}`, 0o644)
	w.add("profiles/coder/SOUL.md", "Your session database is ~/.hermes/profiles/coder/state.db. Other agents live in ~/.hermes/profiles.\n")
	p, _ := w.run(FilesCopy)
	if got := entryNames(p.Files.Moves); !slices.Contains(got, "profiles/coder/scripts") || !slices.Contains(got, "profiles/coder/state") {
		t.Errorf("moves %v", got)
	}
	for name, why := range map[string]string{"profiles/coder/state.db": "sessions and databases", "profiles": "other agents"} {
		if e := entry(p, name); e == nil || !strings.Contains(e.Why, why) {
			t.Errorf("%s: %+v", name, e)
		}
	}
	dest := filepath.Join(w.home, "files", "profiles", "coder")
	if got := readFile(t, filepath.Join(dest, "scripts", "lint.sh")); !strings.Contains(got, "cat ~/.habakkuk/files/profiles/coder/state/last") {
		t.Errorf("lint.sh:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "state.db")); err == nil {
		t.Error("the profile's session database was copied")
	}
	for _, sc := range listSchedules(t, w.home) {
		if sc.Name == "Lint" && sc.Command != "bash "+shellQuote(filepath.Join(dest, "scripts", "lint.sh")) {
			t.Errorf("the Lint schedule runs %q", sc.Command)
		}
	}
}

// OpenClaw keeps each agent's files in a workspace folder that Habakkuk
// goes on using where it is, so that folder is never copied; and its
// "state" folder is its own, where a Hermes Agent home's is the user's.
func TestFilesOpenClaw(t *testing.T) {
	w := newWorld(t, OpenClaw)
	w.write("reports/q3.md", "Quarterly numbers.\n", 0o644)
	w.write("bin-scripts/push.sh", "#!/bin/sh\ncp ~/.openclaw/reports/q3.md /tmp/\n", 0o755)
	w.write("state/openclaw.sqlite-wal", "wal", 0o600)
	w.add("workspace/AGENTS.md", `
Reports are in ~/.openclaw/reports. Push them with `+filepath.Join(w.src, "bin-scripts", "push.sh")+`.
Your own files are in ~/.openclaw/workspace, and the job database is ~/.openclaw/state/openclaw.sqlite.
`)
	p, res := w.run(FilesCopy)
	if got, want := entryNames(p.Files.Moves), []string{"bin-scripts", "reports"}; !slices.Equal(got, want) {
		t.Errorf("moves %v, want %v", got, want)
	}
	ws := entry(p, "workspace")
	if ws == nil || !strings.Contains(ws.Why, "agent habakkuk's workspace") || !slices.Contains(ws.Where, "agent habakkuk's workspace setting") {
		t.Errorf("workspace: %+v", ws)
	}
	if e := entry(p, "state"); e == nil || !strings.Contains(e.Why, "sessions and databases") {
		t.Errorf("state: %+v", e)
	}
	files := filepath.Join(w.home, "files")
	got, _ := os.ReadDir(files)
	if names := names(got, func(d fs.DirEntry) string { return d.Name() }); !slices.Equal(names, []string{"bin-scripts", "reports"}) {
		t.Errorf("files/ holds %v: the workspace must not be copied a second time", names)
	}
	if res.Files.Totals().Copy != 2 {
		t.Errorf("copied %d files, want 2", res.Files.Totals().Copy)
	}
	agents := readFile(t, filepath.Join(w.home, "agents", "habakkuk", "AGENTS.md"))
	for _, want := range []string{"~/.habakkuk/files/reports.", filepath.Join(files, "bin-scripts", "push.sh") + ".", "~/.openclaw/workspace,", "~/.openclaw/state/openclaw.sqlite"} {
		if !strings.Contains(agents, want) {
			t.Errorf("AGENTS.md lacks %q:\n%s", want, agents)
		}
	}
	if got := readFile(t, filepath.Join(files, "bin-scripts", "push.sh")); !strings.Contains(got, "cp ~/.habakkuk/files/reports/q3.md") {
		t.Errorf("push.sh:\n%s", got)
	}
	// The agent's workspace is still the folder in the source home.
	cfg, err := config.Load(w.home)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Agents["habakkuk"].Workspace; got != "~/.openclaw/workspace" {
		t.Errorf("the workspace became %q", got)
	}
	p2 := w.plan(w.options(FilesCopy))
	for _, it := range p2.Items {
		if it.Changes() {
			t.Errorf("second run would change %s %s (%s: %s)", it.Kind, it.Name, it.Status, it.Detail)
		}
	}
	if p2.Files.Pending() {
		t.Error("second run has files to copy")
	}
}

// A Hermes Agent home may keep the agent's working folder inside itself;
// that is the user's data, and the agent's workspace setting follows it.
func TestFilesWorkspaceSettingFollows(t *testing.T) {
	w := newWorld(t, Hermes)
	w.write("workspaces/main/todo.md", "Todo.\n", 0o644)
	cfgPath := filepath.Join(w.src, "config.yaml")
	w.write("config.yaml", strings.Replace(readFile(t, cfgPath), `cwd: "."`, "cwd: "+filepath.Join(w.src, "workspaces", "main"), 1), 0o644)
	w.run("")
	cfg, err := config.Load(w.home)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Agents["habakkuk"].Workspace; got != "~/.hermes/workspaces/main" {
		t.Fatalf("the workspace was imported as %q", got)
	}
	p, res := w.run(FilesCopy)
	if it := item(p, "agent", "habakkuk"); it.Status != Update || !strings.Contains(it.Detail, "~/.habakkuk/files/workspaces/main") {
		t.Errorf("agent: %s (%s)", it.Status, it.Detail)
	}
	if !res.WroteCfg {
		t.Error("config.yaml was not written")
	}
	if cfg, err = config.Load(w.home); err != nil || cfg.Agents["habakkuk"].Workspace != "~/.habakkuk/files/workspaces/main" {
		t.Errorf("the workspace is %q (%v)", cfg.Agents["habakkuk"].Workspace, err)
	}
	if st := status(w.plan(w.options(FilesCopy)), "agent", "habakkuk"); st != Same {
		t.Errorf("the agent is %s on the next run", st)
	}
}

func TestParseSize(t *testing.T) {
	good := map[string]int64{"500M": 500 << 20, "2G": 2 << 30, "1.5GB": 3 << 29, "64k": 64 << 10, "4096": 4096,
		"1GiB": 1 << 30, "0": -1, "none": -1}
	for in, want := range good {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "big", "-1G", "12abc", "1e9", "G"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) should fail", in)
		}
	}
}

func TestInternalsAreNeverUserData(t *testing.T) {
	for from, names := range map[string][]string{
		// The Hermes Agent names are the top level of a real home.
		Hermes: {".env", ".skills_prompt_snapshot.json", ".update_check", "SOUL.md", "assets", "audio_cache", "auth.json", "auth.lock",
			"bin", "bootstrap-cache", "cache", "config.yaml", "context_length_cache.yaml", "cron", "desktop", "desktop-plugins",
			"gateway-starts.log", "gateway_state.json", "hermes-agent", "hermes-setup", "hooks", "image_cache", "logs", "memories",
			"models_dev_cache.etag", "models_dev_cache.json", "node", "ollama_cloud_models_cache.json", "pairing", "pending_messages",
			"pets", "platforms", "plugins", "profile.yaml", "profiles", "projects.db", "provider_models_cache.json", "sessions",
			"skills", "state.db", "state.db-wal", "node_modules", "gateway.pid", "agent.sock", "tools", "local-patches", "locales"},
		OpenClaw: {".env", "openclaw.json", "openclaw.json.bak", "credentials", "agents", "state", "logs", "skills", "cron",
			"extensions", "node_modules", "gateway.sock", "cache"},
	} {
		for _, name := range names {
			if internalWhy(from, name) == "" {
				t.Errorf("%s: %s would be copied as user data", from, name)
			}
		}
	}
	for from, names := range map[string][]string{
		Hermes:   {"workspaces", "scripts", "state", "notes", "data.db", "run.log", "todo.md"},
		OpenClaw: {"reports", "scripts", "notes.md"},
	} {
		for _, name := range names {
			if why := internalWhy(from, name); why != "" {
				t.Errorf("%s: %s is held back (%s)", from, name, why)
			}
		}
	}
	if runtime.GOOS != "windows" && internalWhy(Hermes, "LOGS") != "" {
		t.Error("names are matched exactly where the file system is")
	}
	if !innerInternal(Hermes, "state", "gateway.lifecycle.json") || innerInternal(Hermes, "state", "inbox.json") ||
		innerInternal(Hermes, "state", "sub/gateway.json") || innerInternal(Hermes, "scripts", "gateway.py") || innerInternal(OpenClaw, "state", "gateway.x") {
		t.Error("only the gateway's files directly in a Hermes Agent state folder are the agent's own")
	}
}
