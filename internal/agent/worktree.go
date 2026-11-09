package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WorktreeDir is the folder, inside the workspace, that holds the git
// worktrees of delegated coding tasks.
const WorktreeDir = ".habakkuk-worktrees"

// Worktree is a fresh git worktree a sub-agent works in, on its own branch.
type Worktree struct {
	Dir    string `json:"dir"`
	Branch string `json:"branch"`
	Base   string `json:"base"` // the commit it started from
	Repo   string `json:"repo"` // the main working tree
}

// git runs git in dir with no hooks, so a repository's own scripts never run
// on Habakkuk's behalf.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null"}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("git %s: %s", args[0], msg)
	}
	return out.String(), nil
}

// newWorktree adds a worktree of the repository holding workspace, under
// workspace/.habakkuk-worktrees/<id>, on a new branch habakkuk/<id> from HEAD.
// Called again for the same id (a rerun after a crash) it reuses the one
// already there.
func newWorktree(ctx context.Context, workspace, id string) (*Worktree, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, errors.New("worktree needs git, which is not installed")
	}
	top, err := git(ctx, workspace, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("worktree needs the workspace to be in a git repository: %w", err)
	}
	wt := &Worktree{Repo: strings.TrimSpace(top), Dir: filepath.Join(workspace, WorktreeDir, id), Branch: "habakkuk/" + id}
	head, err := git(ctx, wt.Repo, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("worktree needs a repository with at least one commit: %w", err)
	}
	wt.Base = strings.TrimSpace(head)
	if _, err := os.Stat(filepath.Join(wt.Dir, ".git")); err == nil {
		if base, err := git(ctx, wt.Dir, "merge-base", "HEAD", wt.Base); err == nil {
			wt.Base = strings.TrimSpace(base)
		}
		return wt, nil
	}
	if err := os.MkdirAll(filepath.Dir(wt.Dir), 0o755); err != nil {
		return nil, err
	}
	excludeWorktrees(ctx, wt.Repo, workspace)
	if _, err := git(ctx, wt.Repo, "worktree", "add", "-b", wt.Branch, wt.Dir, wt.Base); err != nil {
		return nil, err
	}
	return wt, nil
}

// excludeWorktrees keeps the worktree folder out of the main tree's git
// status, through .git/info/exclude (never the user's .gitignore).
func excludeWorktrees(ctx context.Context, repo, workspace string) {
	gitDir, err := git(ctx, repo, "rev-parse", "--git-common-dir")
	if err != nil {
		return
	}
	dir := strings.TrimSpace(gitDir)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repo, dir)
	}
	if real, err := filepath.EvalSymlinks(workspace); err == nil {
		workspace = real
	}
	if real, err := filepath.EvalSymlinks(repo); err == nil {
		repo = real
	}
	rel, err := filepath.Rel(repo, filepath.Join(workspace, WorktreeDir))
	if err != nil || strings.HasPrefix(rel, "..") {
		return
	}
	line := "/" + filepath.ToSlash(rel) + "/"
	path := filepath.Join(dir, "info", "exclude")
	old, _ := os.ReadFile(path)
	if strings.Contains(string(old), line) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if len(old) > 0 && !bytes.HasSuffix(old, []byte("\n")) {
		line = "\n" + line
	}
	fmt.Fprintln(f, line)
}

// report commits whatever the sub-agent changed onto its branch and says
// where the work is. A rerun finds nothing new to commit and reports the
// same.
func (w *Worktree) report(ctx context.Context, task string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n[Worktree %s, branch %s from %s.", w.Dir, w.Branch, short(w.Base))
	if _, err := git(ctx, w.Dir, "add", "-A"); err != nil {
		fmt.Fprintf(&b, " Could not stage the changes: %v]", err)
		return b.String()
	}
	if st, _ := git(ctx, w.Dir, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		msg := "habakkuk: " + firstLine(task)
		if _, err := git(ctx, w.Dir, "-c", "user.name=Habakkuk", "-c", "user.email=habakkuk@localhost",
			"-c", "commit.gpgsign=false", "commit", "-q", "--no-verify", "-m", msg); err != nil {
			fmt.Fprintf(&b, " Could not commit the changes: %v]", err)
			return b.String()
		}
	}
	stat, err := git(ctx, w.Dir, "diff", "--stat", w.Base, "HEAD")
	switch {
	case err != nil:
		fmt.Fprintf(&b, " Could not read the diff: %v]", err)
	case strings.TrimSpace(stat) == "":
		b.WriteString(" No changes.]")
	default:
		fmt.Fprintf(&b, " Changes, committed to the branch:\n%s\nMerge with: git merge %s]", strings.TrimRight(stat, "\n"), w.Branch)
	}
	return b.String()
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > 72 {
		s = string(r[:72])
	}
	return s
}
