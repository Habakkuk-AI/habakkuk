package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// apply_patch

type applyPatch struct{}

func (applyPatch) Risk() Risk        { return RiskWrite }
func (applyPatch) WritesLocalFiles() {}
func (applyPatch) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "apply_patch",
		Description: "Add, change, move and delete files with one patch. Either a unified diff (as git diff prints it) or this format:\n" +
			"*** Begin Patch\n*** Add File: path\n+line\n*** Update File: path\n*** Move to: new/path (optional)\n@@ optional line the hunk follows, e.g. a function signature\n context line\n-removed line\n+added line\n*** Delete File: path\n*** End Patch\n" +
			"Hunks are found by their context and removed lines (give about 3 lines of context), not line numbers. " +
			"Every hunk must apply or nothing is written. Paths are relative to the workspace unless absolute.",
		Schema: schema(`{"type":"object","properties":{"patch":{"type":"string"}},"required":["patch"]}`),
	}
}

func patchInput(input json.RawMessage) ([]filePatch, error) {
	in, err := decode[struct{ Patch string }](input)
	if err != nil {
		return nil, err
	}
	return parsePatch(in.Patch)
}

func (applyPatch) TargetPaths(input json.RawMessage) ([]string, error) {
	fps, err := patchInput(input)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, fp := range fps {
		paths = append(paths, fp.Path)
		if fp.MoveTo != "" {
			paths = append(paths, fp.MoveTo)
		}
	}
	return paths, nil
}

func (a applyPatch) Mutates(env *Env, input json.RawMessage) bool {
	paths, err := a.TargetPaths(input)
	if err != nil {
		return false
	}
	for _, p := range paths {
		if env.InWorkspace(env.Resolve(p)) {
			return true
		}
	}
	return false
}

func (applyPatch) Run(ctx context.Context, env *Env, input json.RawMessage) (string, error) {
	fps, err := patchInput(input)
	if err != nil {
		return "", err
	}
	type change struct {
		path    string
		content *string // nil deletes
		perm    os.FileMode
	}
	var changes []change
	var report []string
	pending := map[string]*string{} // earlier results in this patch, by path
	read := func(p string) (string, os.FileMode, error) {
		if c, ok := pending[p]; ok {
			if c == nil {
				return "", 0, fmt.Errorf("%s: deleted earlier in this patch", p)
			}
			return *c, 0o644, nil
		}
		st, err := os.Stat(p)
		if err != nil {
			return "", 0, err
		}
		if st.IsDir() {
			return "", 0, fmt.Errorf("%s is a directory", p)
		}
		data, err := os.ReadFile(p)
		return string(data), st.Mode().Perm(), err
	}
	exists := func(p string) bool {
		if c, ok := pending[p]; ok {
			return c != nil
		}
		_, err := os.Lstat(p)
		return err == nil
	}
	for _, fp := range fps {
		p := env.Resolve(fp.Path)
		switch fp.Op {
		case opAdd:
			if exists(p) {
				return "", fmt.Errorf("cannot add %s: it already exists (update it instead)", fp.Path)
			}
			c := fp.Content
			changes = append(changes, change{path: p, content: &c, perm: 0o644})
			pending[p] = &c
			report = append(report, "A "+fp.Path)
		case opDelete:
			if !exists(p) {
				return "", fmt.Errorf("cannot delete %s: it does not exist", fp.Path)
			}
			changes = append(changes, change{path: p})
			pending[p] = nil
			report = append(report, "D "+fp.Path)
		case opUpdate:
			old, perm, err := read(p)
			if err != nil {
				return "", fmt.Errorf("cannot update %s: %w", fp.Path, err)
			}
			out, err := applyHunks(old, fp.Hunks)
			if err != nil {
				return "", fmt.Errorf("%s: %w", fp.Path, err)
			}
			dest, line := p, fmt.Sprintf("M %s (%d hunk%s)", fp.Path, len(fp.Hunks), plural(len(fp.Hunks)))
			if fp.MoveTo != "" {
				dest = env.Resolve(fp.MoveTo)
				if dest != p && exists(dest) {
					return "", fmt.Errorf("cannot move %s to %s: the destination exists", fp.Path, fp.MoveTo)
				}
				line = fmt.Sprintf("R %s -> %s (%d hunk%s)", fp.Path, fp.MoveTo, len(fp.Hunks), plural(len(fp.Hunks)))
			}
			changes = append(changes, change{path: dest, content: &out, perm: perm})
			pending[dest] = &out
			if dest != p {
				changes = append(changes, change{path: p})
				pending[p] = nil
			}
			report = append(report, line)
		}
	}
	// Every hunk applied in memory; now write.
	for _, c := range changes {
		if c.content == nil {
			if err := os.Remove(c.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return "", err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
			return "", err
		}
		if err := store.WriteFileAtomic(c.path, []byte(*c.content), c.perm); err != nil {
			return "", err
		}
	}
	return "applied patch:\n" + strings.Join(report, "\n"), nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

type patchOp int

const (
	opAdd patchOp = iota + 1
	opUpdate
	opDelete
)

type filePatch struct {
	Op      patchOp
	Path    string
	MoveTo  string
	Content string // opAdd
	Hunks   []hunk // opUpdate
}

type hunk struct {
	Anchor   string   // a line the hunk comes after ("@@ func main()")
	OldStart int      // 1-based line from a unified header; 0 unknown
	Old      []string // context and removed lines, in order
	New      []string // context and added lines, in order
	AtEOF    bool     // the hunk ends at the end of the file
	OldNoEOL bool     // the old text has no final newline
	NewNoEOL bool     // the new text has no final newline
}

// parsePatch reads either the "*** Begin Patch" format or a unified diff.
func parsePatch(text string) ([]filePatch, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "*** Begin Patch") {
			return parseBeginPatch(lines)
		}
		break
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "diff --git ") {
			return parseUnified(lines)
		}
	}
	return nil, errors.New(`unrecognised patch: start with "*** Begin Patch" or give a unified diff with ---/+++ file headers`)
}

func parseBeginPatch(lines []string) ([]filePatch, error) {
	var out []filePatch
	var cur *filePatch
	var h *hunk
	ended := false
	flush := func() {
		if h != nil && cur != nil {
			if len(h.Old) > 0 || len(h.New) > 0 {
				cur.Hunks = append(cur.Hunks, *h)
			}
			h = nil
		}
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "*** Begin Patch"):
			continue
		case strings.HasPrefix(l, "*** End Patch"):
			flush()
			ended = true
		case strings.HasPrefix(l, "*** Add File:"):
			flush()
			cur = &filePatch{Op: opAdd, Path: strings.TrimSpace(strings.TrimPrefix(l, "*** Add File:"))}
		case strings.HasPrefix(l, "*** Delete File:"):
			flush()
			cur = &filePatch{Op: opDelete, Path: strings.TrimSpace(strings.TrimPrefix(l, "*** Delete File:"))}
		case strings.HasPrefix(l, "*** Update File:"):
			flush()
			cur = &filePatch{Op: opUpdate, Path: strings.TrimSpace(strings.TrimPrefix(l, "*** Update File:"))}
		case strings.HasPrefix(l, "*** Move to:"):
			if cur == nil || cur.Op != opUpdate {
				return nil, fmt.Errorf("line %d: *** Move to must follow *** Update File", i+1)
			}
			cur.MoveTo = strings.TrimSpace(strings.TrimPrefix(l, "*** Move to:"))
		case strings.HasPrefix(l, "*** End of File"):
			if h != nil {
				h.AtEOF = true
			}
		case cur == nil:
			if strings.TrimSpace(l) != "" && !ended {
				return nil, fmt.Errorf("line %d: expected *** Add File, *** Update File or *** Delete File, got %q", i+1, l)
			}
		case cur.Op == opAdd:
			if !strings.HasPrefix(l, "+") {
				return nil, fmt.Errorf("line %d: lines of an added file start with +, got %q", i+1, l)
			}
			cur.Content += l[1:] + "\n"
		case cur.Op == opDelete:
			if strings.TrimSpace(l) != "" {
				return nil, fmt.Errorf("line %d: *** Delete File takes no content", i+1)
			}
		case strings.HasPrefix(l, "@@"):
			if h != nil && (len(h.Old) > 0 || len(h.New) > 0) {
				cur.Hunks = append(cur.Hunks, *h)
			}
			h = &hunk{Anchor: strings.TrimSpace(strings.TrimPrefix(l, "@@"))}
		default:
			if h == nil {
				h = &hunk{}
			}
			if err := hunkLine(h, l); err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
		}
	}
	flush()
	return validatePatch(out)
}

// hunkLine adds one " ", "-" or "+" line. A bare empty line is an empty
// context line; models often drop the space.
func hunkLine(h *hunk, l string) error {
	switch {
	case l == "":
		h.Old, h.New = append(h.Old, ""), append(h.New, "")
	case l[0] == ' ':
		h.Old, h.New = append(h.Old, l[1:]), append(h.New, l[1:])
	case l[0] == '-':
		h.Old = append(h.Old, l[1:])
	case l[0] == '+':
		h.New = append(h.New, l[1:])
	case strings.HasPrefix(l, `\ No newline`):
		// Applies to the line before it, set by the caller.
	default:
		return fmt.Errorf("hunk lines start with space, - or +, got %q", l)
	}
	return nil
}

func parseUnified(lines []string) ([]filePatch, error) {
	var out []filePatch
	for i := 0; i < len(lines); {
		l := lines[i]
		if !strings.HasPrefix(l, "--- ") || i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "+++ ") {
			i++ // diff --git, index, mode lines, commentary
			continue
		}
		oldPath, newPath := diffPath(l[4:]), diffPath(lines[i+1][4:])
		i += 2
		fp := filePatch{Op: opUpdate, Path: oldPath}
		switch {
		case oldPath == "" && newPath == "":
			return nil, fmt.Errorf("line %d: both file headers are /dev/null", i-1)
		case oldPath == "":
			fp.Op, fp.Path = opAdd, newPath
		case newPath == "":
			fp.Op = opDelete
		case newPath != oldPath:
			fp.MoveTo = newPath
		}
		for i < len(lines) && strings.HasPrefix(lines[i], "@@") {
			h, next, err := unifiedHunk(lines, i)
			if err != nil {
				return nil, err
			}
			fp.Hunks = append(fp.Hunks, h)
			i = next
		}
		if fp.Op == opAdd {
			var b strings.Builder
			for _, h := range fp.Hunks {
				for _, s := range h.New {
					b.WriteString(s + "\n")
				}
			}
			fp.Content = b.String()
			if n := len(fp.Hunks); n > 0 && fp.Hunks[n-1].NewNoEOL {
				fp.Content = strings.TrimSuffix(fp.Content, "\n")
			}
			fp.Hunks = nil
		}
		if fp.Op == opDelete {
			fp.Hunks = nil
		}
		out = append(out, fp)
	}
	return validatePatch(out)
}

// unifiedHunk reads the hunk whose "@@ -a,b +c,d @@" header is lines[i]. It
// trusts the line counts, but stops early at another hunk or file header, so
// a hand-written hunk with wrong counts still parses.
func unifiedHunk(lines []string, i int) (hunk, int, error) {
	h := hunk{}
	oldN, newN := -1, -1
	if f := strings.Fields(lines[i]); len(f) >= 3 && strings.HasPrefix(f[1], "-") && strings.HasPrefix(f[2], "+") {
		h.OldStart, oldN = hunkRange(f[1][1:])
		_, newN = hunkRange(f[2][1:])
	}
	i++
	var last byte
	for i < len(lines) {
		l := lines[i]
		if oldN >= 0 && len(h.Old) >= oldN && len(h.New) >= newN && !strings.HasPrefix(l, `\`) {
			break
		}
		if strings.HasPrefix(l, "@@") || strings.HasPrefix(l, "diff --git ") ||
			(strings.HasPrefix(l, "--- ") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ ")) {
			break
		}
		if strings.HasPrefix(l, `\`) {
			switch last {
			case '-':
				h.OldNoEOL = true
			case '+':
				h.NewNoEOL = true
			default:
				h.OldNoEOL, h.NewNoEOL = true, true
			}
			i++
			continue
		}
		if l != "" && l[0] != ' ' && l[0] != '-' && l[0] != '+' {
			break
		}
		if err := hunkLine(&h, l); err != nil {
			return h, i, fmt.Errorf("line %d: %w", i+1, err)
		}
		last = ' '
		if l != "" {
			last = l[0]
		}
		i++
	}
	return h, i, nil
}

func hunkRange(s string) (start, n int) {
	a, b, ok := strings.Cut(s, ",")
	start, _ = strconv.Atoi(a)
	n = 1
	if ok {
		n, _ = strconv.Atoi(b)
	}
	return start, n
}

// diffPath turns a ---/+++ header into a path: "" for /dev/null, without
// the a/ or b/ prefix and any trailing timestamp.
func diffPath(s string) string {
	if p, _, ok := strings.Cut(s, "\t"); ok {
		s = p
	}
	s = strings.TrimSpace(s)
	if s == "/dev/null" {
		return ""
	}
	if len(s) > 2 && (strings.HasPrefix(s, "a/") || strings.HasPrefix(s, "b/")) {
		s = s[2:]
	}
	return s
}

func validatePatch(fps []filePatch) ([]filePatch, error) {
	if len(fps) == 0 {
		return nil, errors.New("the patch changes no files")
	}
	for _, fp := range fps {
		if fp.Path == "" {
			return nil, errors.New("a file in the patch has no path")
		}
		if fp.Op == opUpdate && len(fp.Hunks) == 0 && fp.MoveTo == "" {
			return nil, fmt.Errorf("update of %s has no hunks", fp.Path)
		}
	}
	return fps, nil
}

// applyHunks applies hunks in order, each found by its old lines at or after
// where the previous one ended.
func applyHunks(content string, hunks []hunk) (string, error) {
	crlf := strings.Contains(content, "\r\n")
	if crlf {
		content = strings.ReplaceAll(content, "\r\n", "\n")
	}
	eol := strings.HasSuffix(content, "\n")
	lines := strings.Split(content, "\n")
	if eol || content == "" {
		lines = lines[:len(lines)-1]
	}
	pos, delta := 0, 0 // delta: lines added minus removed so far, to shift unified line numbers
	for n, h := range hunks {
		start := pos
		if h.Anchor != "" {
			a := findAnchor(lines, h.Anchor, pos)
			if a < 0 {
				return "", fmt.Errorf("hunk %d does not apply: the line %q it follows was not found", n+1, h.Anchor)
			}
			start = a + 1
		}
		var at int
		switch {
		case len(h.Old) == 0 && h.AtEOF:
			at = len(lines)
		case len(h.Old) == 0 && h.OldStart > 0:
			// "@@ -l,0 +..." inserts after line l.
			at = min(max(h.OldStart+delta, 0), len(lines))
		case len(h.Old) == 0:
			at = len(lines)
		default:
			hint := -1
			if h.OldStart > 0 {
				hint = h.OldStart - 1 + delta
			}
			at = findLines(lines, h.Old, start, hint, h.AtEOF)
			if at < 0 && start > 0 {
				at = findLines(lines, h.Old, 0, hint, h.AtEOF)
			}
			if at < 0 {
				return "", hunkError(n+1, lines, h.Old)
			}
		}
		rest := append([]string{}, lines[at+len(h.Old):]...)
		lines = append(append(lines[:at], h.New...), rest...)
		pos = at + len(h.New)
		delta += len(h.New) - len(h.Old)
		if pos == len(lines) {
			switch {
			case h.NewNoEOL:
				eol = false
			case h.OldNoEOL:
				eol = true
			}
		}
	}
	if !eol && content == "" && len(lines) > 0 {
		eol = true // a new file's content ends with a newline
	}
	sep := "\n"
	if crlf {
		sep = "\r\n"
	}
	out := strings.Join(lines, sep)
	if eol && len(lines) > 0 {
		out += sep
	}
	return out, nil
}

func findAnchor(lines []string, anchor string, from int) int {
	for _, eq := range []func(a, b string) bool{exactEq, trimRightEq, trimEq} {
		for i := from; i < len(lines); i++ {
			if eq(lines[i], anchor) {
				return i
			}
		}
	}
	for i := from; i < len(lines); i++ {
		if strings.Contains(lines[i], anchor) {
			return i
		}
	}
	return -1
}

func exactEq(a, b string) bool     { return a == b }
func trimRightEq(a, b string) bool { return strings.TrimRight(a, " \t") == strings.TrimRight(b, " \t") }
func trimEq(a, b string) bool      { return strings.TrimSpace(a) == strings.TrimSpace(b) }

// findLines finds want in lines at or after from: exact matches first, then
// ignoring trailing whitespace, then ignoring all surrounding whitespace.
// Among matches it takes the one nearest hint (a line index, -1 for none);
// atEOF requires the match to end at the last line.
func findLines(lines, want []string, from, hint int, atEOF bool) int {
	for _, eq := range []func(a, b string) bool{exactEq, trimRightEq, trimEq} {
		best := -1
		for i := from; i+len(want) <= len(lines); i++ {
			if atEOF && i+len(want) != len(lines) {
				continue
			}
			if !matchAt(lines, want, i, eq) {
				continue
			}
			if hint < 0 {
				return i
			}
			if best < 0 || abs(i-hint) < abs(best-hint) {
				best = i
			}
		}
		if best >= 0 {
			return best
		}
	}
	return -1
}

func matchAt(lines, want []string, i int, eq func(a, b string) bool) bool {
	for j, w := range want {
		if !eq(lines[i+j], w) {
			return false
		}
	}
	return true
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// hunkError says which hunk failed and, when part of it matched somewhere,
// the first line that differs, so the model can fix the patch.
func hunkError(n int, lines, want []string) error {
	bestAt, bestLen := -1, 0
	for i := range lines {
		k := 0
		for k < len(want) && i+k < len(lines) && trimEq(lines[i+k], want[k]) {
			k++
		}
		if k > bestLen {
			bestAt, bestLen = i, k
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "hunk %d does not apply: its context and removed lines were not found", n)
	if bestAt >= 0 && bestLen < len(want) {
		got := "(end of file)"
		if bestAt+bestLen < len(lines) {
			got = fmt.Sprintf("%q", lines[bestAt+bestLen])
		}
		fmt.Fprintf(&b, ". The closest match starts at line %d, but line %d is %s where the patch expects %q",
			bestAt+1, bestAt+bestLen+1, got, want[bestLen])
	} else {
		shown := want[:min(len(want), 8)]
		fmt.Fprintf(&b, ". The patch expects:\n%s", strings.Join(shown, "\n"))
		if len(want) > len(shown) {
			b.WriteString("\n...")
		}
	}
	b.WriteString("\nNothing was written. Read the file again and send the patch with its current text.")
	return errors.New(b.String())
}
