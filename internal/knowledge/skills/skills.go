// Package skills loads SKILL.md skills: a folder holding SKILL.md (YAML
// frontmatter with name and description, then the instructions) and any
// supporting files the instructions refer to. Shared skills live in
// $HABAKKUK_HOME/skills/<name>/; an agent's own skills live in
// $HABAKKUK_HOME/agents/<agent>/skills/<name>/ and win on a name clash.
package skills

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/habakkuk-ai/habakkuk/internal/core/frontmatter"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
)

const (
	FileName       = "SKILL.md"
	BackupName     = "SKILL.md.bak"
	MaxDescription = 1024
	MaxBody        = 128 << 10
	MaxFile        = 256 << 10 // largest supporting file skill_file returns
	maxListedFiles = 200
)

var reName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ValidName reports whether name is a valid skill name: lowercase letters,
// digits and hyphens, starting with a letter or digit, at most 64 characters.
func ValidName(name string) error {
	if !reName.MatchString(name) {
		return fmt.Errorf("invalid skill name %q: use lowercase letters, digits and hyphens (at most 64), starting with a letter or digit", name)
	}
	return nil
}

// Skill is one discovered skill.
type Skill struct {
	Name        string
	Description string
	Dir         string
	Shared      bool // from $HABAKKUK_HOME/skills (or a plugin) rather than the agent's own folder
	Plugin      bool // from an enabled plugin's skills folder
}

type meta struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// Library finds skills under a Habakkuk home directory.
type Library struct {
	Home string
	// Extra are more folders of shared skills (enabled plugins'), below
	// $HABAKKUK_HOME/skills in precedence.
	Extra []string
	// History, when set, records every change to an agent's own SKILL.md
	// files, and the curator leaves pinned skills alone.
	History *history.Log

	mu sync.Mutex // serializes this process's writes to the usage file
}

// writeSkill writes an agent's SKILL.md, recording the change.
func (l *Library) writeSkill(ctx context.Context, agent, name string, data []byte) error {
	t := history.SkillTarget(l.Home, agent, name)
	write := func() error { return store.WriteFileAtomic(t.Path, data, 0o600) }
	if l.History == nil {
		return write()
	}
	_, err := l.History.Write(ctx, t, data, write)
	return err
}

// removeSkill takes an agent's skill away with remove, recording that its
// SKILL.md is gone.
func (l *Library) removeSkill(ctx context.Context, agent, name string, remove func() error) error {
	if l.History == nil {
		return remove()
	}
	_, err := l.History.Remove(ctx, history.SkillTarget(l.Home, agent, name), remove)
	return err
}

// Pinned reports whether background jobs must leave an agent's skill alone.
func (l *Library) Pinned(ctx context.Context, agent, name string) bool {
	if l.History == nil {
		return false
	}
	ok, err := l.History.Pinned(ctx, agent, history.KindSkill, name)
	return ok || err != nil // when in doubt, leave it alone
}

// SharedDir holds skills every agent can use.
func (l *Library) SharedDir() string { return filepath.Join(l.Home, "skills") }

// AgentDir holds one agent's own skills.
func (l *Library) AgentDir(agent string) string {
	return filepath.Join(l.Home, "agents", agent, "skills")
}

func (l *Library) agentDir(agent string) (string, error) {
	if agent == "" || agent == "." || agent == ".." || strings.ContainsAny(agent, `/\`) {
		return "", fmt.Errorf("invalid agent name %q", agent)
	}
	return l.AgentDir(agent), nil
}

// List returns the skills available to agent, sorted by name. Folders with an
// invalid name or no SKILL.md are skipped.
func (l *Library) List(agent string) ([]Skill, error) {
	byName := map[string]Skill{}
	for _, root := range slices.Backward(l.Extra) {
		if err := scan(root, true, byName); err != nil {
			return nil, err
		}
		for n, s := range byName {
			if filepath.Dir(s.Dir) == root {
				s.Plugin = true
				byName[n] = s
			}
		}
	}
	if err := scan(l.SharedDir(), true, byName); err != nil {
		return nil, err
	}
	dir, err := l.agentDir(agent)
	if err != nil {
		return nil, err
	}
	if err := scan(dir, false, byName); err != nil {
		return nil, err
	}
	out := make([]Skill, 0, len(byName))
	for _, s := range byName {
		if s.Shared && !s.Plugin && l.Disabled(s) {
			continue // ships scripts and is not approved yet
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Skill) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func scan(root string, shared bool, into map[string]Skill) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if ValidName(e.Name()) != nil {
			continue
		}
		dir := filepath.Join(root, e.Name())
		s, _, err := load(dir)
		if err != nil {
			continue
		}
		s.Shared = shared
		into[s.Name] = s
	}
	return nil
}

// load reads a skill folder. The folder name is the skill's name; a
// different name in the frontmatter is ignored.
func load(dir string) (Skill, string, error) {
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		return Skill{}, "", err
	}
	var m meta
	body, _ := frontmatter.Parse(data, &m)
	return Skill{
		Name:        filepath.Base(dir),
		Description: strings.Join(strings.Fields(m.Description), " "),
		Dir:         dir,
	}, body, nil
}

// Get finds one skill, preferring the agent's own copy.
func (l *Library) Get(agent, name string) (Skill, error) {
	s, _, err := l.get(agent, name)
	return s, err
}

func (l *Library) get(agent, name string) (Skill, string, error) {
	if err := ValidName(name); err != nil {
		return Skill{}, "", err
	}
	dir, err := l.agentDir(agent)
	if err != nil {
		return Skill{}, "", err
	}
	type root struct {
		dir            string
		shared, plugin bool
	}
	roots := []root{{dir, false, false}, {l.SharedDir(), true, false}}
	for _, x := range l.Extra {
		roots = append(roots, root{x, true, true})
	}
	for _, c := range roots {
		s, body, err := load(filepath.Join(c.dir, name))
		if err == nil {
			s.Shared, s.Plugin = c.shared, c.plugin
			if s.Shared && !s.Plugin && l.Disabled(s) {
				return Skill{}, "", fmt.Errorf("skill %q is disabled: it was installed from outside, ships scripts and is not approved yet (the owner can run hk skills approve %s)", name, name)
			}
			return s, body, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return Skill{}, "", err
		}
	}
	return Skill{}, "", fmt.Errorf("no skill %q", name)
}

// View returns a skill's instructions and its supporting files (paths
// relative to the skill folder, sorted).
func (l *Library) View(agent, name string) (Skill, string, []string, error) {
	s, body, err := l.get(agent, name)
	if err != nil {
		return s, "", nil, err
	}
	var files []string
	err = filepath.WalkDir(s.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are just not listed
		}
		if p == s.Dir {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.Dir, p)
		if err != nil || rel == FileName || rel == BackupName {
			return nil
		}
		if len(files) == maxListedFiles {
			return filepath.SkipAll
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	slices.Sort(files)
	return s, body, files, err
}

// ReadFile reads one supporting file. The path must stay inside the skill
// folder: absolute paths, ".." and symlinks that lead outside are refused.
func (l *Library) ReadFile(agent, name, rel string) (string, error) {
	s, err := l.Get(agent, name)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if rel == "" || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == "." {
		return "", fmt.Errorf("path %q must be relative to the skill folder", rel)
	}
	// os.Root resolves every component, symlinks included, inside the folder
	// and fails rather than follow one out of it.
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(clean)
	if err != nil {
		return "", fmt.Errorf("skill %s: %w", name, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", rel)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFile+1))
	if err != nil {
		return "", err
	}
	if len(data) > MaxFile {
		return "", fmt.Errorf("%s is larger than %d bytes", rel, MaxFile)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "", fmt.Errorf("%s is a binary file", rel)
	}
	return string(data), nil
}

// Save creates or replaces the agent's own copy of a skill. An existing
// SKILL.md is kept as SKILL.md.bak first. Supporting files are untouched.
func (l *Library) Save(agent, name, description, body string) (s Skill, created bool, err error) {
	return l.SaveContext(context.Background(), agent, name, description, body)
}

// SaveContext is Save, recording the change as made by the context's
// history actor.
func (l *Library) SaveContext(ctx context.Context, agent, name, description, body string) (s Skill, created bool, err error) {
	if err := ValidName(name); err != nil {
		return s, false, err
	}
	description = strings.Join(strings.Fields(description), " ")
	body = strings.TrimSpace(body)
	switch {
	case description == "":
		return s, false, errors.New("description is empty: say in one line what the skill is for and when to use it")
	case len(description) > MaxDescription:
		return s, false, fmt.Errorf("description is longer than %d characters", MaxDescription)
	case body == "":
		return s, false, errors.New("body is empty")
	case len(body) > MaxBody:
		return s, false, fmt.Errorf("body is longer than %d bytes; move detail into supporting files", MaxBody)
	}
	root, err := l.agentDir(agent)
	if err != nil {
		return s, false, err
	}
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return s, false, err
	}
	path := filepath.Join(dir, FileName)
	old, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := store.WriteFileAtomic(filepath.Join(dir, BackupName), old, 0o600); err != nil {
			return s, false, err
		}
	case errors.Is(err, fs.ErrNotExist):
		created = true
	default:
		return s, false, err
	}
	data, err := frontmatter.Format(meta{Name: name, Description: description}, body)
	if err != nil {
		return s, false, err
	}
	if err := l.writeSkill(ctx, agent, name, data); err != nil {
		return s, false, err
	}
	return Skill{Name: name, Description: description, Dir: dir}, created, nil
}
