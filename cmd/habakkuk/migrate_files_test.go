package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// filesSource is a Hermes Agent home whose imported text names the user's
// own files in it (workspaces, scripts, state) and parts of the agent
// itself. It returns the user's home, the source home and where the
// Habakkuk home will be.
func filesSource(t *testing.T) (user, src, home string) {
	t.Helper()
	user, src = sourceHome(t, "hermes")
	write := func(rel, text string, mode os.FileMode) {
		t.Helper()
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("workspaces/briefs/monday.md", "Brief for Monday.\n", 0o644)
	write("state/inbox.json", `{"last_uid": 4411}`+"\n", 0o600)
	write("hermes-agent/venv/bin/hermes", "#!/bin/sh\n", 0o755)
	write("scripts/inbox_sync.py", "#!/usr/bin/env python3\nSTATE = \""+filepath.Join(src, "state", "inbox.json")+"\"\nENV = \"~/.hermes/.env\"\n", 0o755)
	soul, err := os.ReadFile(filepath.Join(src, "SOUL.md"))
	if err != nil {
		t.Fatal(err)
	}
	write("SOUL.md", string(soul)+"\nYour briefs live in ~/.hermes/workspaces/briefs. Never edit ~/.hermes/config.yaml.\n", 0o644)
	write("cron/jobs.json", `{"jobs": [{"id": "a2", "name": "Inbox sync", "prompt": "", "script": "inbox_sync.py", "no_agent": true,
		"schedule": {"kind": "interval", "minutes": 30}, "deliver": "local", "enabled": true}]}`, 0o644)
	return user, src, filepath.Join(user, ".habakkuk")
}

func migrateOut(t *testing.T, home, user, stdin string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := runMigrate(context.Background(), home, args, strings.NewReader(stdin), &out, noEnv, user); err != nil {
		t.Fatalf("habakkuk migrate %v: %v\n%s", args, err, out.String())
	}
	assertNoSecrets(t, out.String())
	return out.String()
}

func wantAll(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func TestMigrateFilesCopy(t *testing.T) {
	user, src, home := filesSource(t)
	files := filepath.Join(home, "files")

	// A dry run shows the whole plan and writes nothing.
	out := migrateOut(t, home, user, "", "--dry-run", "--files", "copy")
	wantAll(t, out,
		"Files in "+src+" that the imported text names",
		"+ scripts: ", "+ state: ", "+ workspaces: ", "-> "+filepath.Join(files, "workspaces"),
		"Inside the copied files, 1 reference in 1 file will name the copies too.",
		"Not moved ("+src+" is still needed for these):",
		"- .env: named once (scripts/inbox_sync.py): Hermes Agent's keys and sign-ins",
		"- config.yaml: named once (habakkuk: SOUL.md): Hermes Agent's config",
		"They will be copied (", "is only read.", "Dry run: nothing was written.")
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("a dry run created %s", home)
	}

	out = migrateOut(t, home, user, "", "--files", "copy", "--secrets", "copy", "-y")
	wantAll(t, out,
		"Copying ", "Copied 4 files (", "out of "+src+" into "+files+": scripts, state, workspaces.",
		"Rewrote 3 references to name the copies: 1 in persona files, 1 in schedules, 1 inside 1 copied file.",
		"Still points into "+src+" (that folder is needed while these remain):",
		"- .env: named once (scripts/inbox_sync.py)",
		"habakkuk migrate --dry-run       see what still points into "+src+" before you remove it")
	if strings.Contains(out, "hermes-agent") {
		t.Errorf("hermes-agent is not named by anything imported:\n%s", out)
	}
	for _, rel := range []string{"scripts/inbox_sync.py", "scripts/digest.py", "state/inbox.json", "workspaces/briefs/monday.md"} {
		if _, err := os.Stat(filepath.Join(files, filepath.FromSlash(rel))); err != nil {
			t.Errorf("not copied: %v", err)
		}
	}
	for _, rel := range []string{"hermes-agent", ".env", "config.yaml", "skills", "sessions"} {
		if _, err := os.Stat(filepath.Join(files, rel)); err == nil {
			t.Errorf("%s is part of the old agent and was copied", rel)
		}
	}
	soul, _ := os.ReadFile(filepath.Join(home, "agents", "habakkuk", "SOUL.md"))
	if !strings.Contains(string(soul), "~/.habakkuk/files/workspaces/briefs") {
		t.Errorf("SOUL.md:\n%s", soul)
	}
	script, _ := os.ReadFile(filepath.Join(files, "scripts", "inbox_sync.py"))
	if !strings.Contains(string(script), filepath.Join(files, "state", "inbox.json")) || !strings.Contains(string(script), "~/.hermes/.env") {
		t.Errorf("the copied script:\n%s", script)
	}

	// Again, in any mode: nothing to do.
	for _, args := range [][]string{{"--files", "copy", "-y"}, {"-y"}, {"--files", "skip", "-y"}} {
		out = migrateOut(t, home, user, "", args...)
		wantAll(t, out, "Nothing to import: everything is already there.", "= scripts: ", "already in "+filepath.Join(files, "scripts"))
		if strings.Contains(out, "--files copy to bring them over") || strings.Contains(out, "\n  ! ") {
			t.Errorf("%v after the copy:\n%s", args, out)
		}
	}
}

// -y alone leaves the files and says how to bring them over later; doing
// that later rewrites what was imported.
func TestMigrateFilesSkippedThenCopied(t *testing.T) {
	for _, args := range [][]string{{"-y"}, {"-y", "--files", "skip"}} {
		user, src, home := filesSource(t)
		out := migrateOut(t, home, user, "", args...)
		wantAll(t, out,
			"- workspaces: ", "(it would go to "+filepath.Join(home, "files", "workspaces")+")",
			"Still points into "+src,
			"- scripts: named once (schedule \"Inbox sync\"): your own files, not copied",
			"habakkuk migrate --files copy copies your own files into "+filepath.Join(home, "files")+" and rewrites the references.",
			"habakkuk migrate --files copy    bring your files over from "+src)
		if len(args) == 1 {
			wantAll(t, out, "They are not copied unless you say so: habakkuk migrate --files copy brings them over.")
		} else {
			wantAll(t, out, "They will not be copied (--files skip)")
		}
		if _, err := os.Stat(filepath.Join(home, "files")); err == nil {
			t.Errorf("%v copied files", args)
		}
		out = migrateOut(t, home, user, "", args...)
		wantAll(t, out, "Nothing to import: everything is already there.", "run again with --files copy to bring them over")

		// The user has changed one imported file since; it still names the
		// old folder.
		own := filepath.Join(home, "agents", "coder", "SOUL.md")
		if err := os.WriteFile(own, []byte("My own words. Briefs: ~/.hermes/workspaces/briefs\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out = migrateOut(t, home, user, "", "-y", "--files", "copy")
		wantAll(t, out, "~ habakkuk: SOUL.md: it will name the copied files", "~ habakkuk: Inbox sync: it will name the copied files",
			"! coder: SOUL.md: the agent already has a different SOUL.md; kept, with its references to the copied files rewritten",
			"Copied 4 files (", "Rewrote 4 references to name the copies: 2 in persona files, 1 in schedules, 1 inside 1 copied file.",
			"Left as they were (1 conflicts)", "In 1 of them only the references to the copied files were rewritten.")
		if strings.Contains(out, "Schedules are imported paused") {
			t.Errorf("a schedule changed in place is not a new, paused one:\n%s", out)
		}
		if text, _ := os.ReadFile(own); string(text) != "My own words. Briefs: ~/.habakkuk/files/workspaces/briefs\n" {
			t.Errorf("the user's file is now %q", text)
		}
		out = migrateOut(t, home, user, "", "-y", "--files", "copy")
		wantAll(t, out, "Nothing to import: everything is already there.", "! coder: SOUL.md: the agent already has a different SOUL.md\n")
	}
}

func TestMigrateFilesAsks(t *testing.T) {
	// Import, keys, then the files: no.
	user, src, home := filesSource(t)
	out := migrateOut(t, home, user, "y\ny\nn\n")
	wantAll(t, out, "You will be asked whether to copy them (",
		"Copy your files (scripts, state, workspaces: ", "into "+filepath.Join(home, "files")+" and point the imported text at the copies? (Y/n)",
		"Still points into "+src, "your own files, not copied")
	if _, err := os.Stat(filepath.Join(home, "files")); err == nil {
		t.Error("files were copied after answering no")
	}
	soul, _ := os.ReadFile(filepath.Join(home, "agents", "habakkuk", "SOUL.md"))
	if !strings.Contains(string(soul), "~/.hermes/workspaces/briefs") {
		t.Errorf("SOUL.md was rewritten after answering no:\n%s", soul)
	}
	// Later: yes (Enter).
	out = migrateOut(t, home, user, "\n\n")
	wantAll(t, out, "Copied 4 files (", "Rewrote 3 references")
	// Nothing left to ask about.
	out = migrateOut(t, home, user, "")
	if strings.Contains(out, "(Y/n)") || !strings.Contains(out, "Nothing to import") {
		t.Errorf("a third run asked again:\n%s", out)
	}

	// Yes the first time.
	user, _, home = filesSource(t)
	out = migrateOut(t, home, user, "\n\n\n")
	wantAll(t, out, "Copied 4 files (", "Rewrote 3 references")
}

func TestMigrateFilesFlags(t *testing.T) {
	user, _, home := filesSource(t)
	var out bytes.Buffer
	for _, args := range [][]string{{"--files", "move"}, {"--files-max", "huge"}, {"--files-max", "-1"}} {
		if err := runMigrate(context.Background(), home, args, strings.NewReader(""), &out, noEnv, user); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("bad flags wrote something")
	}
	// --files-dir and --files-max.
	dir := filepath.Join(user, "moved")
	text := migrateOut(t, home, user, "", "-y", "--files", "copy", "--files-dir", dir, "--files-max", "64")
	wantAll(t, text, "-> "+filepath.Join(dir, "workspaces"), "leaves out 1 file over 64 bytes (--files-max raises the limit): inbox_sync.py",
		"Left out of scripts, over the --files-max limit: inbox_sync.py.")
	if _, err := os.Stat(filepath.Join(dir, "workspaces", "briefs", "monday.md")); err != nil {
		t.Errorf("--files-dir: %v", err)
	}
	// Only the script that was left out names the state folder, so that
	// folder is not wanted yet.
	if _, err := os.Stat(filepath.Join(dir, "state")); err == nil {
		t.Error("state was copied although nothing that was imported names it")
	}
	if _, err := os.Stat(filepath.Join(dir, "scripts", "inbox_sync.py")); err == nil {
		t.Error("a file over --files-max was copied")
	}
	soul, _ := os.ReadFile(filepath.Join(home, "agents", "habakkuk", "SOUL.md"))
	if !strings.Contains(string(soul), "~/moved/workspaces/briefs") {
		t.Errorf("SOUL.md:\n%s", soul)
	}
	text = migrateOut(t, home, user, "", "-y", "--files", "copy", "--files-dir", dir, "--files-max", "0")
	wantAll(t, text, "+ state: ", "Copied 2 files (", "scripts, state, workspaces.")
}

func TestMigrateFilesNoRoom(t *testing.T) {
	user, _, home := filesSource(t)
	none := func(string) (uint64, error) { return 4096, nil }
	run := func(stdin string, fl migrateFlags) (string, error) {
		var out bytes.Buffer
		fl.userHome, fl.freeSpace = user, none
		m := &migration{fl: fl, home: home, in: bufio.NewReader(strings.NewReader(stdin)), out: &out, getenv: noEnv}
		err := m.run(context.Background())
		return out.String(), err
	}
	// Asked for: refused before anything is written.
	out, err := run("", migrateFlags{files: "copy", yes: true})
	if err == nil || !strings.Contains(err.Error(), "nothing was written") {
		t.Errorf("err = %v", err)
	}
	wantAll(t, out, "Problem: not enough room in "+filepath.Join(home, "files"), "use --files skip")
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("a refused copy created %s", home)
	}
	// Not asked for: the import goes on without the copy and says so.
	out, err = run("y\nn\n", migrateFlags{})
	if err != nil {
		t.Fatal(err)
	}
	wantAll(t, out, "Your files cannot be copied now, so they stay where they are", "Imported 2 agents", "your own files, not copied")
	if strings.Contains(out, "Copy your files") {
		t.Errorf("it asked about a copy it cannot make:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, "files")); err == nil {
		t.Error("files were copied")
	}
}
