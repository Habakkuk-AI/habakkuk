package tools

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/hub"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// Trusted folders for notes (agents.<name>.trusted_write_paths). A turn
// that came from a chat, or that has read outside content, needs a human
// for every file write outside the workspace. That is right for most of
// the disk and tiresome for the one or two folders where the agent keeps
// notes for its owner. A trusted folder lifts the approval for the file
// tools there, and only for them, and only for files that are not programs:
//
//   - only write_file, edit_file and apply_patch; the shell is unaffected,
//     so nothing written there can be run without the usual approval;
//   - every path the call touches must be in the workspace or under a
//     trusted folder, after symlinks are resolved, so a link inside the
//     folder cannot point the write somewhere else;
//   - never under a protected folder (Policy.Protected: the agent homes,
//     config, hooks, skills, plugins and the folders scheduled scripts run
//     from), whatever the setting says;
//   - never a file that could run as a program (an executable bit, a script
//     extension, a #! line, a program binary: the test skills use for
//     "ships scripts"). In a tainted turn such a write is refused outright;
//     in a clean one it goes through the usual approval;
//   - never a hidden file or anything under a hidden folder (.git, .envrc):
//     those are settings other programs act on;
//   - never for a guest's turn.

// trustedWrite reports whether a call is covered by the turn's trusted
// folders. A non-nil error means the call is refused.
func trustedWrite(env *Env, t Tool, call llm.Block) (bool, error) {
	p := env.Policy
	if p == nil || len(p.TrustedWrite) == 0 || p.Guest || env.Files != nil || env.remote() {
		return false, nil
	}
	var targets []string
	switch tt := t.(type) {
	case writeFile:
		path, err := tt.TargetPath(call.Input)
		if err != nil {
			return false, nil
		}
		targets = []string{path}
	case editFile:
		path, err := tt.TargetPath(call.Input)
		if err != nil {
			return false, nil
		}
		targets = []string{path}
	case applyPatch:
		paths, err := tt.TargetPaths(call.Input)
		if err != nil {
			return false, nil
		}
		targets = paths
	default:
		return false, nil
	}
	roots := realPaths(p.TrustedWrite)
	protected := realPaths(p.Protected)
	var trusted []string // the targets outside the workspace, as written
	for _, target := range targets {
		abs := env.Resolve(target)
		if env.InWorkspace(abs) {
			continue
		}
		real, ok := realPath(abs)
		if !ok || underAny(protected, real) || underAny(protected, abs) || !inTrusted(roots, real) {
			return false, nil
		}
		trusted = append(trusted, target)
	}
	if len(trusted) == 0 {
		return false, nil
	}
	if name := scriptLikeWrite(env, t, call.Input, trusted); name != "" {
		if env.Taint.Marked() {
			return false, fmt.Errorf("%w: %s would be a script or program in a trusted notes folder (trusted_write_paths). After reading outside content those folders take notes and data only; tell the user, who can put it there themselves", ErrDenied, name)
		}
		return false, nil
	}
	return true, nil
}

// realPaths resolves each folder's symlinks, dropping any that cannot be
// resolved safely.
func realPaths(dirs []string) []string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if d = strings.TrimSpace(d); d == "" {
			continue
		}
		if real, ok := realPath(d); ok {
			out = append(out, real)
		}
	}
	return out
}

// realPath resolves the symlinks in p as far as p exists: the longest
// existing part is resolved and the rest appended, so the path of a file
// that is not there yet still says where it would really land. ok is false
// when a link on the way cannot be followed (it dangles), since nothing can
// then be said about where a write would go.
func realPath(p string) (string, bool) {
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(real, rest), true
		}
		if fi, lerr := os.Lstat(cur); lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return "", false
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, true
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// inTrusted reports whether p is inside one of the trusted folders, with
// no hidden file or folder on the way: .git, .envrc and their like are
// settings other programs act on, not notes.
func inTrusted(roots []string, p string) bool {
	for _, root := range roots {
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		hidden := false
		for _, el := range strings.Split(rel, string(filepath.Separator)) {
			hidden = hidden || strings.HasPrefix(el, ".")
		}
		if !hidden {
			return true
		}
	}
	return false
}

// underAny reports whether p is one of the folders or inside one.
func underAny(dirs []string, p string) bool {
	for _, d := range dirs {
		rel, err := filepath.Rel(d, p)
		if err == nil && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// scriptLikeWrite names the first of the given targets that the call would
// leave as something that can run, or "".
func scriptLikeWrite(env *Env, t Tool, input json.RawMessage, targets []string) string {
	want := map[string]bool{}
	for _, target := range targets {
		want[target] = true
	}
	// existing is the file's mode and first bytes now; zero and nil when it
	// is not there.
	existing := func(abs string) (fs.FileMode, []byte) {
		st, err := os.Stat(abs)
		if err != nil || st.IsDir() {
			return 0, nil
		}
		return st.Mode(), fileHead(abs)
	}
	check := func(target string, after []byte) bool {
		abs := env.Resolve(target)
		mode, before := existing(abs)
		return hub.ScriptLike(abs, mode, before) || hub.ScriptLike(abs, 0, after)
	}
	switch t.(type) {
	case writeFile:
		in, err := decode[struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}](input)
		if err != nil || check(in.Path, head([]byte(in.Content))) {
			return in.Path
		}
	case editFile:
		in, err := decode[struct {
			Path       string `json:"path"`
			OldString  string `json:"old_string"`
			NewString  string `json:"new_string"`
			ReplaceAll bool   `json:"replace_all"`
		}](input)
		if err != nil {
			return in.Path
		}
		after := []byte(nil)
		if data, rerr := os.ReadFile(env.Resolve(in.Path)); rerr == nil && in.OldString != "" {
			limit := 1
			if in.ReplaceAll {
				limit = -1
			}
			after = head([]byte(strings.Replace(string(data), in.OldString, in.NewString, limit)))
		}
		if check(in.Path, after) {
			return in.Path
		}
	case applyPatch:
		fps, err := patchInput(input)
		if err != nil {
			return "the patch"
		}
		for _, fp := range fps {
			var after []byte
			switch fp.Op {
			case opAdd:
				after = head([]byte(fp.Content))
			case opUpdate:
				// The result is not computed here; a patch that adds a #!
				// line anywhere is treated as writing a script.
				for _, h := range fp.Hunks {
					for _, l := range h.New {
						if strings.HasPrefix(l, "#!") {
							after = []byte(l)
						}
					}
				}
			}
			for _, target := range []string{fp.Path, fp.MoveTo} {
				if target != "" && want[target] && check(target, after) {
					return target
				}
			}
		}
	}
	return ""
}

// head is the part of a file's content the script test looks at.
func head(data []byte) []byte { return data[:min(len(data), 512)] }

func fileHead(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := io.ReadFull(f, buf)
	return buf[:n]
}
