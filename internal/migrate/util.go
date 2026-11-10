package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/hub"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/skills"
)

// dig walks nested maps by key; nil when a step is missing.
func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// asList reads a list of strings (or one string).
func asList(v any) []string {
	switch t := v.(type) {
	case string:
		if t != "" {
			return []string{t}
		}
	case []any:
		var out []string
		for _, x := range t {
			if x != nil {
				out = append(out, fmt.Sprint(x))
			}
		}
		return out
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string { return slices.Sorted(maps.Keys(m)) }

// parseTime reads an ISO timestamp.
func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, l := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999", "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// titleOf makes a note title from its first line: at most eight words.
func titleOf(body string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
	first = strings.Trim(first, "#-*> \t")
	if i := strings.IndexAny(first, ".:;"); i > 12 {
		first = first[:i]
	}
	words := strings.Fields(first)
	if len(words) > 8 {
		words = words[:8]
	}
	t := strings.Join(words, " ")
	if memory.Slugify(t) == "" {
		return "Imported note"
	}
	return t
}

// dedupeTitles numbers notes whose titles would share a file.
func dedupeTitles(notes []Note) {
	seen := map[string]int{}
	for i := range notes {
		slug := memory.Slugify(notes[i].Title)
		seen[slug]++
		if n := seen[slug]; n > 1 {
			notes[i].Title = fmt.Sprintf("%s (%d)", notes[i].Title, n)
		}
	}
}

// chunkNote makes one note, or numbered parts when the text is too long
// for one.
func chunkNote(title, body string, tags []string) []Note {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	const limit = memory.MaxContent - 1024
	if len(body) <= limit {
		return []Note{{Title: title, Body: body, Tags: tags}}
	}
	var out []Note
	for part := 1; body != ""; part++ {
		n := min(len(body), limit)
		if n < len(body) {
			if i := strings.LastIndex(body[:n], "\n"); i > limit/2 {
				n = i
			}
		}
		out = append(out, Note{Title: fmt.Sprintf("%s (part %d)", title, part), Body: strings.TrimSpace(body[:n]), Tags: tags})
		body = strings.TrimSpace(body[n:])
	}
	return out
}

type section struct{ name, body string }

// splitSections splits Markdown on "## " headings; text before the first
// heading has no name.
func splitSections(text string) []section {
	var out []section
	cur := section{}
	var body []string
	flush := func() {
		cur.body = strings.TrimSpace(strings.Join(body, "\n"))
		if cur.body != "" || cur.name != "" {
			out = append(out, cur)
		}
		body = nil
	}
	for _, l := range strings.Split(text, "\n") {
		if name, ok := strings.CutPrefix(strings.TrimRight(l, " \t\r"), "## "); ok {
			flush()
			cur = section{name: strings.TrimSpace(name)}
			continue
		}
		body = append(body, l)
	}
	flush()
	// A lone "# Title" heading over the whole file is not content.
	if len(out) > 0 && out[0].name == "" && strings.HasPrefix(out[0].body, "# ") && !strings.Contains(out[0].body, "\n") {
		out = out[1:]
	}
	return out
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " '\"\\$`!*?&;|<>(){}[]#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// toInt reads a whole number from JSON or YAML (a float, an int, a
// json.Number or a numeric string).
func toInt(v any) (int64, bool) {
	switch t := v.(type) {
	case int:
		return int64(t), true
	case int64:
		return t, true
	case float64:
		return int64(t), t == float64(int64(t))
	case json.Number:
		n, err := t.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// tilde writes a path under the user's home as ~/..., the way config.yaml
// spells the default workspace.
func tilde(p, home string) string {
	if home == "" {
		return p
	}
	if rel, err := filepath.Rel(home, p); err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
		return "~/" + filepath.ToSlash(rel)
	}
	return p
}

// plural is "1 skill" or "3 skills".
func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// skipCopy are folders and files a skill copy leaves out.
func skipCopy(name string) bool {
	switch name {
	case ".git", "__pycache__", "node_modules", ".DS_Store", ".venv":
		return true
	}
	return strings.HasSuffix(name, ".pyc")
}

// findSkills lists the folders under root that hold a SKILL.md, at any
// depth (categories), skipping hidden folders. A symlink is followed when
// it leads to a skill folder, or to a category folder holding skills; one
// that leads nowhere, or to something else, is listed as left out.
func (f *Found) findSkills(root string) ([]string, error) {
	var out []string
	seen := map[string]bool{} // folders already walked, against link loops
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return err
		}
		if seen[real] || depth > 8 {
			return nil
		}
		seen[real] = true
		return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && p == dir {
					return fs.SkipAll
				}
				return err
			}
			if d.Type()&fs.ModeSymlink != 0 && !strings.HasPrefix(d.Name(), ".") {
				target, err := filepath.EvalSymlinks(p)
				info, serr := os.Stat(target)
				inSkill := d.Name() != skills.FileName && exists(filepath.Join(filepath.Dir(p), skills.FileName))
				switch {
				case inSkill:
					// A link among a skill's own files: the copy handles it.
				case err != nil || serr != nil:
					f.skip("skill link %s: it leads nowhere", p)
				case !info.IsDir():
					if d.Name() != skills.FileName {
						f.skip("skill link %s: it leads to a file, not a skill folder", p)
					} else if filepath.Dir(p) != root {
						out = append(out, filepath.Dir(p))
					}
				case exists(filepath.Join(target, skills.FileName)):
					out = append(out, target)
				default:
					return walk(target, depth+1) // a category folder kept elsewhere
				}
				return nil
			}
			if d.IsDir() && p != dir && (strings.HasPrefix(d.Name(), ".") || skipCopy(d.Name())) {
				return filepath.SkipDir
			}
			if !d.IsDir() && d.Name() == skills.FileName && filepath.Dir(p) != root {
				out = append(out, filepath.Dir(p))
			}
			return nil
		})
	}
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	err := walk(root, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	slices.Sort(out)
	out = slices.Compact(out)
	// A skill's own subfolders may hold more SKILL.md files: keep the outer one.
	var keep []string
	for _, d := range out {
		if !slices.ContainsFunc(keep, func(k string) bool { return strings.HasPrefix(d, k+string(filepath.Separator)) }) {
			keep = append(keep, d)
		}
	}
	return keep, err
}

// skillName makes a folder name a valid skill name, or "".
func skillName(base string) string {
	n := cleanName(strings.ReplaceAll(base, "_", "-"), 64)
	if skills.ValidName(n) != nil {
		return ""
	}
	return n
}

// treeHash hashes a folder's files (paths and contents), leaving out what
// a copy leaves out; a missing folder hashes to "". With rw, text files are
// hashed as copyTree would write them: with their references rewritten.
func treeHash(dir string, rw *rewriter) (string, error) {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir && skipCopy(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(h, "%s\x00", filepath.ToSlash(rel))
		if data, ok, err := rewrittenFile(p, d, rw); err != nil {
			return err
		} else if ok {
			h.Write(data)
			h.Write([]byte{0})
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		h.Write([]byte{0})
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

// rewrittenFile reads a file small enough to have its references
// rewritten and returns it rewritten; ok is false when rw is nil or the
// file is too large, and the caller reads it as it is.
func rewrittenFile(path string, d fs.DirEntry, rw *rewriter) (data []byte, ok bool, err error) {
	if rw == nil {
		return nil, false, nil
	}
	if info, err := d.Info(); err != nil || info.Size() > rewriteCap {
		return nil, false, err
	}
	if data, err = os.ReadFile(path); err != nil {
		return nil, false, err
	}
	data, _ = rw.bytes(data)
	return data, true, nil
}

// copyTree copies a skill folder's regular files, leaving out symlinks and
// what skipCopy names. Files keep their executable bit; everything is
// readable only by its owner. With rw, the references in its text files
// are rewritten on the way.
func copyTree(src, dst string, rw *rewriter) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != src && skipCopy(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if info.Mode().Perm()&0o100 != 0 {
			mode = 0o700
		}
		if data, ok, err := rewrittenFile(p, d, rw); err != nil {
			return err
		} else if ok {
			return os.WriteFile(target, data, mode)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

// The helpers below treat a skill made in memory (Skill.Text) like a
// folder holding only its SKILL.md.

// skillHash is treeHash for a skill.
func skillHash(s *Skill, rw *rewriter) (string, error) {
	if s.Dir != "" {
		return treeHash(s.Dir, rw)
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00", skills.FileName)
	h.Write(skillText(s, rw))
	h.Write([]byte{0})
	return hex.EncodeToString(h.Sum(nil)), nil
}

func skillText(s *Skill, rw *rewriter) []byte {
	data := []byte(s.Text)
	if rw != nil {
		data, _ = rw.bytes(data)
	}
	return data
}

// skillCopy is copyTree for a skill.
func skillCopy(s *Skill, dst string, rw *rewriter) error {
	if s.Dir != "" {
		return copyTree(s.Dir, dst, rw)
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dst, skills.FileName), skillText(s, rw), 0o600)
}

// skillTexts is skillFiles for a skill.
func skillTexts(s *Skill, fn func(rel string, data []byte)) error {
	if s.Dir != "" {
		return skillFiles(s.Dir, fn)
	}
	fn(skills.FileName, []byte(s.Text))
	return nil
}

// skillScan is hub.Scan for a skill.
func skillScan(s *Skill) ([]hub.Finding, error) {
	if s.Dir != "" {
		return hub.Scan(s.Dir)
	}
	return hub.ScanFile(skills.FileName, []byte(s.Text)), nil
}
