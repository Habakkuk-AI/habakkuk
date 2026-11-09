// Package checkpoint snapshots an agent's workspace before a turn changes
// it, so the change can be rolled back.
//
// Snapshots are commits in a shadow git repository per agent under
// $HABAKKUK_HOME/checkpoints/<agent>, driven with GIT_DIR and GIT_WORK_TREE:
// the workspace's own .git, if it has one, is never read or written, and
// nested repositories are skipped. The workspace's .gitignore files apply.
// Files over a per-file cap are left out, and a snapshot whose new or
// changed files add up to more than the total cap is skipped. Without git on
// PATH there are no checkpoints.
package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// Kinds of checkpoint.
const (
	KindTurn     = "turn"     // taken before a turn's first change
	KindUndo     = "undo"     // the state /undo replaced
	KindRollback = "rollback" // the state `habakkuk rollback` replaced
)

var (
	ErrNoGit   = errors.New("git is not installed")
	ErrTooBig  = errors.New("the workspace changes are over the checkpoint size cap")
	ErrNothing = errors.New("no checkpoint to undo")
)

// Store keeps every agent's checkpoints under Root.
type Store struct {
	Root string
	// MaxBytes caps the new and changed bytes one snapshot adds; default
	// 200 MiB. Bigger snapshots are skipped.
	MaxBytes int64
	// AgentMaxBytes overrides MaxBytes for an agent when it returns more
	// than 0.
	AgentMaxBytes func(agent string) int64
	// MaxFileBytes leaves larger files out of snapshots; default 20 MiB.
	MaxFileBytes int64
	Git          string // git binary; "" looks it up on PATH
	Log          *slog.Logger
	// Enabled says whether an agent takes checkpoints; nil means all do.
	Enabled func(agent string) bool

	locks sync.Map // agent -> *sync.Mutex
}

// Checkpoint is one snapshot.
type Checkpoint struct {
	ID      string // the commit id's first 10 characters
	Full    string // the whole commit id
	Time    time.Time
	Kind    string
	Session string
	Task    string // the turn's request, first line
	Undoes  string // for undo and rollback: the checkpoint restored
}

// Meta describes a snapshot being taken.
type Meta struct {
	Kind    string
	Session string
	Task    string
	Undoes  string
}

func (s *Store) lock(agent string) func() {
	m, _ := s.locks.LoadOrStore(agent, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (s *Store) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Checkpoint implements tools.Checkpointer: it snapshots env's workspace
// before the turn's first change. Failures are logged, never returned.
func (s *Store) Checkpoint(ctx context.Context, env *tools.Env) {
	if s.Enabled != nil && !s.Enabled(env.Agent) {
		return
	}
	cp, err := s.Snapshot(ctx, env.Agent, env.Workspace, Meta{Kind: KindTurn, Session: env.Session, Task: env.Task})
	switch {
	case errors.Is(err, ErrNoGit):
		s.log().Debug("checkpoint skipped: git is not installed", "agent", env.Agent)
	case err != nil:
		s.log().Warn("checkpoint skipped", "agent", env.Agent, "session", env.Session, "err", err)
	default:
		s.log().Debug("checkpoint taken", "agent", env.Agent, "session", env.Session, "id", cp.ID)
	}
}

// repo runs git for one agent's shadow repository over a workspace.
type repo struct {
	git, dir, work, agent string
}

func (s *Store) repo(agent, workspace string) (*repo, error) {
	git := s.Git
	if git == "" {
		var err error
		if git, err = exec.LookPath("git"); err != nil {
			return nil, ErrNoGit
		}
	}
	return &repo{git: git, dir: filepath.Join(s.Root, agent), work: workspace, agent: agent}, nil
}

// run runs git with only this repository in view: no user or system config
// (so no hooks, signing or filters of theirs), literal pathspecs, and stdin
// from in.
func (r *repo) run(ctx context.Context, in []byte, args ...string) (string, error) {
	base := []string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false",
		"-c", "user.name=Habakkuk", "-c", "user.email=habakkuk@localhost", "-c", "core.autocrlf=false",
		"-c", "core.quotepath=false", "-c", "core.fsmonitor=false", "-c", "advice.addEmbeddedRepo=false"}
	cmd := exec.CommandContext(ctx, r.git, append(base, args...)...)
	cmd.Dir = r.work
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "GIT_DIR="+r.dir, "GIT_WORK_TREE="+r.work, "GIT_LITERAL_PATHSPECS=1",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	if in != nil {
		cmd.Stdin = bytes.NewReader(in)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func (r *repo) init(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(r.dir, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return err
	}
	_, err := r.run(ctx, nil, "init", "-q")
	return err
}

func (r *repo) hasHead(ctx context.Context) bool {
	_, err := r.run(ctx, nil, "rev-parse", "-q", "--verify", "HEAD^{commit}")
	return err == nil
}

// stage brings the shadow index up to the workspace: new, changed and
// deleted files, but not ignored files, nested repositories, or files over
// the per-file cap (those are dropped from the index if it had them).
func (s *Store) stage(ctx context.Context, r *repo) error {
	maxTotal, maxFile := s.MaxBytes, s.MaxFileBytes
	if s.AgentMaxBytes != nil {
		if n := s.AgentMaxBytes(r.agent); n > 0 {
			maxTotal = n
		}
	}
	if maxTotal <= 0 {
		maxTotal = 200 << 20
	}
	if maxFile <= 0 {
		maxFile = 20 << 20
	}
	out, err := r.run(ctx, nil, "ls-files", "-z", "--others", "--modified", "--deleted", "--exclude-standard")
	if err != nil {
		return err
	}
	var add, drop bytes.Buffer
	var total int64
	seen := map[string]bool{}
	for _, p := range strings.Split(out, "\x00") {
		if p == "" || seen[p] || strings.HasSuffix(p, "/") { // "dir/" is a nested repository
			continue
		}
		seen[p] = true
		st, err := os.Lstat(filepath.Join(r.work, p))
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			continue
		case st.IsDir():
			continue
		case st.Size() > maxFile:
			drop.WriteString(p + "\x00")
			continue
		default:
			total += st.Size()
		}
		add.WriteString(p + "\x00")
	}
	if total > maxTotal {
		return fmt.Errorf("%w (%d MiB > %d MiB)", ErrTooBig, total>>20, maxTotal>>20)
	}
	if drop.Len() > 0 {
		if _, err := r.run(ctx, drop.Bytes(), "rm", "-q", "--cached", "--ignore-unmatch", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return err
		}
	}
	if add.Len() > 0 {
		if _, err := r.run(ctx, add.Bytes(), "add", "-A", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return err
		}
	}
	return nil
}

// Snapshot commits the workspace's current state.
func (s *Store) Snapshot(ctx context.Context, agent, workspace string, m Meta) (*Checkpoint, error) {
	defer s.lock(agent)()
	r, err := s.repo(agent, workspace)
	if err != nil {
		return nil, err
	}
	return s.snapshot(ctx, r, m)
}

func (s *Store) snapshot(ctx context.Context, r *repo, m Meta) (*Checkpoint, error) {
	if err := r.init(ctx); err != nil {
		return nil, err
	}
	if err := s.stage(ctx, r); err != nil {
		return nil, err
	}
	if _, err := r.run(ctx, []byte(message(m)), "commit", "-q", "--allow-empty", "--no-verify", "-F", "-"); err != nil {
		return nil, err
	}
	out, err := r.run(ctx, nil, "log", "-1", "--format="+logFormat)
	if err != nil {
		return nil, err
	}
	list := parseLog(out)
	if len(list) == 0 {
		return nil, errors.New("checkpoint commit not found")
	}
	return &list[0], nil
}

func message(m Meta) string {
	task := strings.TrimSpace(m.Task)
	if i := strings.IndexByte(task, '\n'); i >= 0 {
		task = task[:i]
	}
	if len(task) > 100 {
		task = task[:100] + "..."
	}
	if task == "" {
		task = "(no task)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n\nKind: %s\n", m.Kind, task, m.Kind)
	if m.Session != "" {
		fmt.Fprintf(&b, "Session: %s\n", m.Session)
	}
	if m.Undoes != "" {
		fmt.Fprintf(&b, "Undoes: %s\n", m.Undoes)
	}
	return b.String()
}

const logFormat = "%H%x00%ct%x00%B%x1e"

var trailer = regexp.MustCompile(`(?m)^(Kind|Session|Undoes): (.*)$`)

func parseLog(out string) []Checkpoint {
	var list []Checkpoint
	for _, rec := range strings.Split(out, "\x1e") {
		f := strings.SplitN(strings.TrimLeft(rec, "\n"), "\x00", 3)
		if len(f) < 3 {
			continue
		}
		sec, _ := strconv.ParseInt(f[1], 10, 64)
		cp := Checkpoint{ID: f[0][:min(10, len(f[0]))], Full: f[0], Time: time.Unix(sec, 0)}
		subject, _, _ := strings.Cut(f[2], "\n")
		if _, task, ok := strings.Cut(subject, ": "); ok {
			cp.Task = task
		}
		for _, m := range trailer.FindAllStringSubmatch(f[2], -1) {
			switch m[1] {
			case "Kind":
				cp.Kind = m[2]
			case "Session":
				cp.Session = m[2]
			case "Undoes":
				cp.Undoes = m[2]
			}
		}
		list = append(list, cp)
	}
	return list
}

// List returns an agent's checkpoints, newest first; limit <= 0 means all.
func (s *Store) List(ctx context.Context, agent string, limit int) ([]Checkpoint, error) {
	r, err := s.repo(agent, s.Root) // the work tree is not needed to read history
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(r.dir, "HEAD")); err != nil || !r.hasHead(ctx) {
		return nil, nil
	}
	args := []string{"log", "--format=" + logFormat}
	if limit > 0 {
		args = append(args, "-n", strconv.Itoa(limit))
	}
	out, err := r.run(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	return parseLog(out), nil
}

// Has reports whether id names one of agent's checkpoints.
func (s *Store) Has(ctx context.Context, agent, id string) bool {
	r, err := s.repo(agent, s.Root)
	if err != nil || !validID(id) {
		return false
	}
	if _, err := os.Stat(filepath.Join(r.dir, "HEAD")); err != nil {
		return false
	}
	_, err = r.run(ctx, nil, "rev-parse", "-q", "--verify", id+"^{commit}")
	return err == nil
}

var idPattern = regexp.MustCompile(`^[0-9a-f]{4,40}$`)

func validID(id string) bool { return idPattern.MatchString(id) }

// relPath turns path into a path relative to the workspace, refusing one
// outside it. "" is the whole workspace.
func relPath(workspace, path string) (string, error) {
	if path == "" {
		return ".", nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	rel, err := filepath.Rel(workspace, filepath.Clean(path))
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the workspace %s", path, workspace)
	}
	return filepath.ToSlash(rel), nil
}

// DiffStat says what restoring checkpoint id (the whole workspace, or path)
// would change: a git diff --stat from the current state to the checkpoint.
// It stages the current state in the shadow index but commits nothing.
func (s *Store) DiffStat(ctx context.Context, agent, workspace, id, path string) (string, error) {
	defer s.lock(agent)()
	r, rel, err := s.prepare(ctx, agent, workspace, id, path)
	if err != nil {
		return "", err
	}
	if err := s.stage(ctx, r); err != nil {
		return "", err
	}
	return r.diffStat(ctx, id, rel)
}

func (r *repo) diffStat(ctx context.Context, id, rel string) (string, error) {
	out, err := r.run(ctx, nil, "diff", "--cached", "-R", "--stat=100", id, "--", rel)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "no changes", nil
	}
	return strings.TrimRight(out, "\n"), nil
}

func (s *Store) prepare(ctx context.Context, agent, workspace, id, path string) (*repo, string, error) {
	if !validID(id) {
		return nil, "", fmt.Errorf("%q is not a checkpoint id", id)
	}
	r, err := s.repo(agent, workspace)
	if err != nil {
		return nil, "", err
	}
	if _, err := os.Stat(filepath.Join(r.dir, "HEAD")); err != nil {
		return nil, "", fmt.Errorf("agent %s has no checkpoints", agent)
	}
	if _, err := r.run(ctx, nil, "rev-parse", "-q", "--verify", id+"^{commit}"); err != nil {
		return nil, "", fmt.Errorf("agent %s has no checkpoint %s", agent, id)
	}
	rel, err := relPath(workspace, path)
	if err != nil {
		return nil, "", err
	}
	return r, rel, nil
}

// Restore puts the workspace (or one path in it) back to checkpoint id. The
// current state is snapshotted first (kind m.Kind, Undoes id), so a restore
// can itself be rolled back. Files that are ignored, too big or in nested
// repositories are left alone. It returns the diff stat of what it changed.
func (s *Store) Restore(ctx context.Context, agent, workspace, id, path string, m Meta) (string, error) {
	defer s.lock(agent)()
	r, rel, err := s.prepare(ctx, agent, workspace, id, path)
	if err != nil {
		return "", err
	}
	return s.restore(ctx, r, id, rel, m)
}

func (s *Store) restore(ctx context.Context, r *repo, id, rel string, m Meta) (string, error) {
	m.Undoes = id
	if _, err := s.snapshot(ctx, r, m); err != nil {
		return "", fmt.Errorf("saving the current state first: %w", err)
	}
	stat, err := r.diffStat(ctx, id, rel)
	if err != nil {
		return "", err
	}
	// Paths in the index but not in the checkpoint are removed, the rest
	// rewritten from it.
	if _, err := r.run(ctx, nil, "restore", "--source="+id, "--staged", "--worktree", "--", rel); err != nil {
		return "", err
	}
	return stat, nil
}

// Undo restores the workspace to the newest turn checkpoint of session that
// has not been undone yet: the state before that turn's changes. Repeated,
// it walks further back.
func (s *Store) Undo(ctx context.Context, agent, workspace, session string) (*Checkpoint, string, error) {
	defer s.lock(agent)()
	r, err := s.repo(agent, workspace)
	if err != nil {
		return nil, "", err
	}
	if _, err := os.Stat(filepath.Join(r.dir, "HEAD")); err != nil || !r.hasHead(ctx) {
		return nil, "", ErrNothing
	}
	out, err := r.run(ctx, nil, "log", "--format="+logFormat)
	if err != nil {
		return nil, "", err
	}
	list := parseLog(out)
	var undone []string
	for _, cp := range list {
		if cp.Kind == KindUndo && cp.Undoes != "" {
			undone = append(undone, cp.Undoes)
		}
	}
	for _, cp := range list {
		if cp.Kind != KindTurn || cp.Session != session || slices.ContainsFunc(undone, func(u string) bool { return strings.HasPrefix(cp.Full, u) }) {
			continue
		}
		stat, err := s.restore(ctx, r, cp.ID, ".", Meta{Kind: KindUndo, Session: session, Task: "undo " + cp.Task})
		if err != nil {
			return nil, "", err
		}
		return &cp, stat, nil
	}
	return nil, "", ErrNothing
}
