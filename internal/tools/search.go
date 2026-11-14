package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// search_files

type searchFiles struct {
	rg string // ripgrep binary; "" looks it up, "-" never uses it (tests)
}

const (
	searchMaxLine     = 300     // characters of a matching line shown
	searchMaxFileSize = 5 << 20 // larger files are skipped
	searchMaxFiles    = 200000  // files looked at before giving up
)

type searchInput struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	IgnoreCase bool   `json:"ignore_case"`
	FilesOnly  bool   `json:"files_only"`
	MaxResults int    `json:"max_results"`
}

func (searchFiles) Risk() Risk { return RiskRead }
func (searchFiles) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "search_files",
		Description: "Search file contents with a regular expression, under the workspace or path. Returns path:line: text for each match. " +
			"Hidden files and paths in .gitignore are skipped, as are binary and very large files. glob filters file names (e.g. *.go, or src/**/*.ts).",
		Schema: schema(`{"type":"object","properties":{
			"pattern":{"type":"string","description":"regular expression (RE2 syntax)"},
			"path":{"type":"string","description":"directory or file to search; default the workspace"},
			"glob":{"type":"string"},
			"ignore_case":{"type":"boolean"},
			"files_only":{"type":"boolean","description":"list matching files instead of lines"},
			"max_results":{"type":"integer","description":"default 200, at most 2000"}},"required":["pattern"]}`),
	}
}

func (s searchFiles) Run(ctx context.Context, env *Env, input json.RawMessage) (string, error) {
	in, err := decode[searchInput](input)
	if err != nil {
		return "", err
	}
	if in.Pattern == "" {
		return "", errors.New("pattern is empty")
	}
	expr := in.Pattern
	if in.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("bad pattern: %w", err)
	}
	limit := 200
	if in.MaxResults > 0 {
		limit = min(in.MaxResults, 2000)
	}
	root := env.Resolve(in.Path)
	if _, err := os.Stat(root); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	res := &searchResults{env: env, limit: limit, filesOnly: in.FilesOnly, seen: map[string]bool{}}
	rg := s.rg
	if rg == "" {
		rg, _ = exec.LookPath("rg")
	}
	if rg != "" && rg != "-" {
		err = searchRipgrep(ctx, rg, root, in, res)
	} else {
		err = searchWalk(ctx, root, re, in.Glob, res)
	}
	if err != nil && !errors.Is(err, errSearchFull) {
		return res.String(), err
	}
	return res.String(), nil
}

var errSearchFull = errors.New("result limit reached")

type searchResults struct {
	env       *Env
	limit     int
	filesOnly bool
	lines     []string
	seen      map[string]bool
	full      bool
	note      string
}

// display shows paths inside the workspace relative to it.
func (r *searchResults) display(p string) string {
	if r.env.InWorkspace(p) {
		if rel, err := filepath.Rel(r.env.Workspace, p); err == nil {
			return filepath.ToSlash(rel) // the same on every platform
		}
	}
	return p
}

func (r *searchResults) add(file string, line int, text string) error {
	if len(r.lines) >= r.limit {
		r.full = true
		return errSearchFull
	}
	if r.filesOnly {
		if r.seen[file] {
			return nil
		}
		r.seen[file] = true
		r.lines = append(r.lines, r.display(file))
		return nil
	}
	if len(text) > searchMaxLine {
		text = text[:searchMaxLine] + "..."
	}
	r.lines = append(r.lines, fmt.Sprintf("%s:%d: %s", r.display(file), line, strings.TrimRight(text, "\r")))
	return nil
}

func (r *searchResults) String() string {
	if len(r.lines) == 0 {
		return "no matches" + r.note
	}
	out := strings.Join(r.lines, "\n")
	if r.full {
		out += fmt.Sprintf("\n[stopped at %d results; narrow the pattern, path or glob]", r.limit)
	}
	return out + r.note
}

// searchRipgrep runs rg, which honors .gitignore fully and is fast on big
// trees. Its regex dialect is close to RE2. --no-require-git makes it honor
// .gitignore in a folder that is not a git repository too, as the built-in
// search does.
func searchRipgrep(ctx context.Context, rg, root string, in searchInput, res *searchResults) error {
	args := []string{"--no-require-git", "--no-heading", "--with-filename", "--line-number", "--color=never", "--no-messages", "--null",
		"--max-filesize", "5M", "--max-columns", fmt.Sprint(searchMaxLine), "--max-columns-preview"}
	if in.IgnoreCase {
		args = append(args, "--ignore-case")
	}
	if in.FilesOnly {
		args = append(args, "--files-with-matches")
	}
	if in.Glob != "" {
		args = append(args, "--glob", in.Glob)
	}
	args = append(args, "--regexp", in.Pattern, "--", root)
	cmd := exec.CommandContext(ctx, rg, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	if in.FilesOnly {
		sc.Split(splitNUL)
	}
	var addErr error
	for sc.Scan() {
		// --null ends each path with NUL, so paths may hold ":".
		file, rest, _ := strings.Cut(sc.Text(), "\x00")
		if in.FilesOnly {
			addErr = res.add(file, 0, "")
		} else if num, text, ok := strings.Cut(rest, ":"); ok {
			n, _ := strconv.Atoi(num)
			addErr = res.add(file, n, text)
		}
		if addErr != nil {
			break
		}
	}
	if addErr != nil {
		_ = cmd.Process.Kill()
		_, _ = io.Copy(io.Discard, out)
		_ = cmd.Wait()
		return addErr
	}
	err = cmd.Wait()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return nil // no matches
	}
	if err != nil && stderr.Len() > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return err
}

func splitNUL(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// searchWalk is the fallback without ripgrep: a directory walk that skips
// hidden entries, symlinks, binary and large files, and honors the basics
// of .gitignore (globs, **, anchoring, trailing /, negation).
func searchWalk(ctx context.Context, root string, re *regexp.Regexp, glob string, res *searchResults) error {
	st, err := os.Stat(root)
	if err != nil {
		return err
	}
	var globRe *regexp.Regexp
	if glob != "" {
		globRe, err = globRegexp(strings.TrimPrefix(glob, "/"), strings.Contains(glob, "/"))
		if err != nil {
			return fmt.Errorf("bad glob: %w", err)
		}
	}
	files := 0
	searchFile := func(p string) error {
		if globRe != nil {
			rel, _ := filepath.Rel(root, p)
			if !globRe.MatchString(filepath.ToSlash(rel)) {
				return nil
			}
		}
		files++
		if files > searchMaxFiles {
			res.note = fmt.Sprintf("\n[stopped after %d files; search a narrower path]", searchMaxFiles)
			return errSearchFull
		}
		return grepFile(p, re, res)
	}
	if !st.IsDir() {
		return grepFile(root, re, res)
	}
	var walk func(dir string, rules []ignoreRule) error
	walk = func(dir string, rules []ignoreRule) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil // unreadable directories are skipped
		}
		if data, err := os.ReadFile(filepath.Join(dir, ".gitignore")); err == nil {
			rules = append(rules[:len(rules):len(rules)], parseGitignore(dir, string(data))...)
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") || e.Type()&os.ModeSymlink != 0 {
				continue
			}
			p := filepath.Join(dir, name)
			if ignored(rules, p, e.IsDir()) {
				continue
			}
			if e.IsDir() {
				err = walk(p, rules)
			} else if e.Type().IsRegular() {
				err = searchFile(p)
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	return walk(root, nil)
}

func grepFile(p string, re *regexp.Regexp, res *searchResults) error {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() > searchMaxFileSize {
		return nil
	}
	br := bufio.NewReaderSize(f, 64*1024)
	if head, _ := br.Peek(8000); bytes.IndexByte(head, 0) >= 0 {
		return nil // binary
	}
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		if re.Match(sc.Bytes()) {
			if err := res.add(p, n, sc.Text()); err != nil {
				return err
			}
			if res.filesOnly {
				return nil
			}
		}
	}
	return nil
}

type ignoreRule struct {
	base    string // directory holding the .gitignore
	re      *regexp.Regexp
	neg     bool
	dirOnly bool
}

func parseGitignore(base, text string) []ignoreRule {
	var rules []ignoreRule
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, " \r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r := ignoreRule{base: base}
		if strings.HasPrefix(line, "!") {
			r.neg, line = true, line[1:]
		}
		line = strings.TrimPrefix(line, `\`)
		if strings.HasSuffix(line, "/") {
			r.dirOnly, line = true, strings.TrimRight(line, "/")
		}
		if line == "" {
			continue
		}
		anchored := strings.Contains(line, "/")
		re, err := globRegexp(strings.TrimPrefix(line, "/"), anchored)
		if err != nil {
			continue
		}
		r.re = re
		rules = append(rules, r)
	}
	return rules
}

// ignored applies the rules in order; the last one that matches decides.
func ignored(rules []ignoreRule, p string, isDir bool) bool {
	out := false
	for _, r := range rules {
		if r.dirOnly && !isDir {
			continue
		}
		rel, err := filepath.Rel(r.base, p)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		if r.re.MatchString(filepath.ToSlash(rel)) {
			out = !r.neg
		}
	}
	return out
}

// globRegexp converts a gitignore-style glob to a regexp over slash paths.
// An unanchored glob matches at any depth.
func globRegexp(glob string, anchored bool) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	if !anchored {
		b.WriteString("(?:.*/)?")
	}
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch {
		case strings.HasPrefix(glob[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(glob[i:], "/**") && i+3 == len(glob):
			b.WriteString("/.*")
			i += 2
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '[':
			j := strings.IndexByte(glob[i:], ']')
			if j < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := glob[i+1 : i+j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + class + "]")
			i += j
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
