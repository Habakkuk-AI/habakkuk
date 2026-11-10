package hub

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ErrHighRisk means the scan found a high-risk pattern (a download piped
// into a shell, a decoded payload run, credentials or the environment sent
// out, a program binary) and the caller did not pass Force; nothing was
// installed.
var ErrHighRisk = errors.New("the security scan found high-risk patterns: read the findings; install anyway only with -force")

// highRisk are the scan rules whose findings need Force rather than Yes.
var highRisk = map[string]bool{
	"pipe-to-shell": true, "obfuscation": true, "env-exfiltration": true, "credential-files": true, "binary": true,
}

// High reports whether the finding is high-risk.
func (f Finding) High() bool { return highRisk[f.Rule] }

// anyHigh reports whether any finding is high-risk.
func anyHigh(fs []Finding) bool { return slices.ContainsFunc(fs, Finding.High) }

// scriptExt are extensions of files that run as programs.
var scriptExt = map[string]bool{
	".sh": true, ".bash": true, ".zsh": true, ".fish": true, ".ksh": true, ".py": true, ".rb": true, ".pl": true,
	".js": true, ".mjs": true, ".cjs": true, ".ts": true, ".php": true, ".ps1": true, ".psm1": true, ".bat": true,
	".cmd": true, ".lua": true, ".tcl": true, ".applescript": true, ".scpt": true, ".exe": true, ".jar": true,
}

// Files lists the regular files under dir as slash paths, sorted, skipping
// .git.
func Files(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	slices.Sort(out)
	return out, err
}

// ScriptLike reports whether a file could run as a program: an executable
// bit, a script extension, a #! line or a program binary. name is its path,
// mode its permission bits (0 when unknown) and head its first bytes (nil
// when unknown). It is the one test for "this is a script": skills use it
// to decide a package ships scripts, and the file tools to keep programs
// out of trusted notes folders.
func ScriptLike(name string, mode fs.FileMode, head []byte) bool {
	if mode.Perm()&0o111 != 0 || scriptExt[strings.ToLower(filepath.Ext(name))] {
		return true
	}
	return bytes.HasPrefix(head, []byte("#!")) || isExecutable(head)
}

// Scripts lists the files under dir an agent could run as a program: an
// executable bit, a #! line, a script extension or a program binary.
func Scripts(dir string) []string {
	files, _ := Files(dir)
	var out []string
	for _, rel := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if ScriptLike(rel, info.Mode(), nil) {
			out = append(out, rel)
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		head := make([]byte, 512)
		n, _ := f.Read(head)
		f.Close()
		if ScriptLike(rel, 0, head[:n]) {
			out = append(out, rel)
		}
	}
	return out
}

// Change is one file that differs between two versions of a package.
type Change struct {
	Path  string
	Kind  string // added, removed, changed
	Patch string // a line diff for text files; "" when too large or binary
}

const (
	maxDiffBytes = 256 << 10
	maxDiffLines = 3000
)

// Diff compares the installed package in oldDir with the fetched one in
// newDir.
func Diff(oldDir, newDir string) ([]Change, error) {
	oldFiles, err := Files(oldDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	newFiles, err := Files(newDir)
	if err != nil {
		return nil, err
	}
	var out []Change
	for _, rel := range newFiles {
		nb, err := os.ReadFile(filepath.Join(newDir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		if !slices.Contains(oldFiles, rel) {
			out = append(out, Change{Path: rel, Kind: "added", Patch: patch(nil, nb)})
			continue
		}
		ob, err := os.ReadFile(filepath.Join(oldDir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(ob, nb) {
			out = append(out, Change{Path: rel, Kind: "changed", Patch: patch(ob, nb)})
		}
	}
	for _, rel := range oldFiles {
		if !slices.Contains(newFiles, rel) {
			out = append(out, Change{Path: rel, Kind: "removed"})
		}
	}
	slices.SortFunc(out, func(a, b Change) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// patch is a minimal line diff ("-" old, "+" new, " " context lines
// dropped) of two text files, or "" for binary or very large ones.
func patch(a, b []byte) string {
	if len(a) > maxDiffBytes || len(b) > maxDiffBytes || bytes.IndexByte(a, 0) >= 0 || bytes.IndexByte(b, 0) >= 0 {
		return ""
	}
	al, bl := lines(a), lines(b)
	if len(al) > maxDiffLines || len(bl) > maxDiffLines {
		return ""
	}
	// Longest common subsequence, then walk it.
	lcs := make([][]int32, len(al)+1)
	for i := range lcs {
		lcs[i] = make([]int32, len(bl)+1)
	}
	for i := len(al) - 1; i >= 0; i-- {
		for j := len(bl) - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var sb strings.Builder
	i, j := 0, 0
	for i < len(al) || j < len(bl) {
		switch {
		case i < len(al) && j < len(bl) && al[i] == bl[j]:
			i++
			j++
		case j < len(bl) && (i == len(al) || lcs[i][j+1] > lcs[i+1][j]):
			fmt.Fprintf(&sb, "+%s\n", bl[j])
			j++
		default:
			fmt.Fprintf(&sb, "-%s\n", al[i])
			i++
		}
	}
	return sb.String()
}

func lines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// AuditEntry is what `hk skills audit` reports for one installed
// package folder.
type AuditEntry struct {
	Name     string
	Dir      string
	Entry    *Entry // nil for a folder put there by hand
	Hash     string // TreeHash of the files now
	Modified bool   // the files differ from what was installed (lock sha256)
	Scripts  []string
	Findings []Finding
	Err      error
}

// Audit rescans every package folder in m.Dir: its scripts, the scan's
// findings, and whether its files still match the hash recorded at
// install.
func (m *Manager) Audit() ([]AuditEntry, error) {
	entries, err := m.Entries()
	if err != nil {
		return nil, err
	}
	dirs, err := os.ReadDir(m.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []AuditEntry
	for _, d := range dirs {
		if !d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			continue
		}
		dir := filepath.Join(m.Dir, d.Name())
		if _, err := os.Stat(filepath.Join(dir, m.Marker)); err != nil {
			continue
		}
		a := AuditEntry{Name: d.Name(), Dir: dir, Entry: entries[d.Name()], Scripts: Scripts(dir)}
		if a.Hash, a.Err = TreeHash(dir); a.Err == nil {
			a.Findings, a.Err = Scan(dir)
		}
		a.Modified = a.Entry != nil && a.Hash != "" && a.Hash != a.Entry.SHA256
		out = append(out, a)
	}
	return out, nil
}
