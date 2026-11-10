package projtrust

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
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
	e, err := st.Trust(file)
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
	if _, err := st.Trust(big); err == nil {
		t.Fatal("trusted a file too large to hash whole")
	}
	if _, err := st.Trust(dir); err == nil {
		t.Fatal("trusted a folder as a file")
	}
}
