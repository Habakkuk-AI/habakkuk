package projtrust

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestTrustCheckListUntrust(t *testing.T) {
	home, proj := t.TempDir(), t.TempDir()
	file := filepath.Join(proj, "AGENTS.md")
	os.WriteFile(file, []byte("Use tabs."), 0o644)
	st := &Store{Home: home}
	real, _ := Real(file)
	sum, _, _ := Hash(real)
	if ok, known := st.Check(real, sum); ok || known {
		t.Fatal("trusted before anyone trusted it")
	}
	e, _, err := st.Trust(file)
	if err != nil || e.Path != real || e.SHA256 != sum {
		t.Fatalf("trust: %+v %v", e, err)
	}
	if ok, _ := st.Check(real, sum); !ok {
		t.Fatal("not trusted after Trust")
	}
	if runtime.GOOS != "windows" {
		for _, p := range []string{st.Folder(), st.file()} {
			info, _ := os.Stat(p)
			if info.Mode().Perm()&0o077 != 0 {
				t.Fatalf("%s is open to other users: %o", p, info.Mode().Perm())
			}
		}
	}
	// One changed byte.
	os.WriteFile(file, []byte("Use tabs!"), 0o644)
	sum2, _, _ := Hash(real)
	if ok, known := st.Check(real, sum2); ok || !known {
		t.Fatalf("changed file: ok=%v known=%v", ok, known)
	}
	_, status, _ := st.List()
	if status[0] != StatusChanged {
		t.Fatalf("status %q", status[0])
	}
	removed, err := st.Untrust(proj) // a folder untrusts what is under it
	if err != nil || len(removed) != 1 {
		t.Fatalf("untrust: %v %v", removed, err)
	}
	if entries, _, _ := st.List(); len(entries) != 0 {
		t.Fatal("still listed")
	}
}

func TestTrustRefusesOddFiles(t *testing.T) {
	st := &Store{Home: t.TempDir()}
	dir := t.TempDir()
	big := filepath.Join(dir, "AGENTS.md")
	os.WriteFile(big, make([]byte, MaxBytes+1), 0o644)
	if _, _, err := st.Trust(big); err == nil {
		t.Fatal("trusted a file too large to hash whole")
	}
	if _, _, err := st.Trust(dir); err == nil {
		t.Fatal("trusted a folder as a file")
	}
}

// A record anyone else could have written trusts nothing: open to others,
// a link, or in a folder open to others.
func TestCheckRefusesARecordThatIsNotPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes")
	}
	home, proj := t.TempDir(), t.TempDir()
	file := filepath.Join(proj, "AGENTS.md")
	os.WriteFile(file, []byte("Use tabs."), 0o644)
	st := &Store{Home: home}
	e, _, err := st.Trust(file)
	if err != nil {
		t.Fatal(err)
	}
	check := func() bool { ok, _ := st.Check(e.Path, e.SHA256); return ok }
	if !check() {
		t.Fatal("not trusted")
	}
	os.Chmod(st.file(), 0o644)
	if check() {
		t.Fatal("trusted through a record others can read and write")
	}
	os.Chmod(st.file(), 0o600)
	os.Chmod(st.Folder(), 0o755)
	if check() {
		t.Fatal("trusted through a folder open to others")
	}
	os.Chmod(st.Folder(), 0o700)
	// A link in the record's place.
	data, _ := os.ReadFile(st.file())
	elsewhere := filepath.Join(t.TempDir(), "x.json")
	os.WriteFile(elsewhere, data, 0o600)
	os.Remove(st.file())
	os.Symlink(elsewhere, st.file())
	if check() {
		t.Fatal("trusted through a link")
	}
}

// The same file reached another way, and the same place in a worktree of
// the same repository, are matched; another repository is not.
func TestTrustFollowsTheFileNotTheSpelling(t *testing.T) {
	home, base := t.TempDir(), t.TempDir()
	repo := filepath.Join(base, "app")
	os.MkdirAll(filepath.Join(repo, ".git", "worktrees", "task1"), 0o755)
	os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("Use tabs."), 0o644)
	st := &Store{Home: home}
	e, _, err := st.Trust(filepath.Join(repo, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if e.Repo == "" || e.Rel != "AGENTS.md" {
		t.Fatalf("no repository place recorded: %+v", e)
	}
	// Through a link to the repository.
	link := filepath.Join(base, "link")
	os.Symlink(repo, link)
	if ok, _ := st.Check(filepath.Join(link, "AGENTS.md"), e.SHA256); !ok {
		t.Fatal("the same file through a link is not trusted")
	}
	// Another letter case, on a disk that ignores case.
	if _, err := os.Stat(filepath.Join(base, "APP")); err == nil {
		if ok, _ := st.Check(filepath.Join(base, "APP", "AGENTS.md"), e.SHA256); !ok {
			t.Fatal("the same file in another case is not trusted")
		}
	}
	// A worktree of the repository, as delegated tasks use.
	wt := filepath.Join(base, "wt")
	os.MkdirAll(wt, 0o755)
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(repo, ".git", "worktrees", "task1")+"\n"), 0o644)
	os.WriteFile(filepath.Join(repo, ".git", "worktrees", "task1", "commondir"), []byte("../..\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "AGENTS.md"), []byte("Use tabs."), 0o644)
	if ok, _ := st.Check(filepath.Join(wt, "AGENTS.md"), e.SHA256); !ok {
		t.Fatal("the same file in a worktree of the trusted repository is not trusted")
	}
	// Another repository with the same content is not.
	other := filepath.Join(base, "other")
	os.MkdirAll(filepath.Join(other, ".git"), 0o755)
	os.WriteFile(filepath.Join(other, "AGENTS.md"), []byte("Use tabs."), 0o644)
	if ok, known := st.Check(filepath.Join(other, "AGENTS.md"), e.SHA256); ok || known {
		t.Fatal("another repository's file is trusted")
	}
}

func TestReadRegularRefusesFIFOsAndLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFOs")
	}
	dir := t.TempDir()
	fifo := filepath.Join(dir, "AGENTS.md")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := ReadRegular(fifo, 100); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read a FIFO")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("hung on a FIFO")
	}
	os.WriteFile(filepath.Join(dir, "real.md"), []byte("x"), 0o644)
	os.Symlink(filepath.Join(dir, "real.md"), filepath.Join(dir, "CLAUDE.md"))
	if _, err := ReadRegular(filepath.Join(dir, "CLAUDE.md"), 100); err == nil {
		t.Fatal("followed a final link")
	}
}
