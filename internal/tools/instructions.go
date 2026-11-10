package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// InstructionFileNames are the project files whose text goes into an
// agent's prompt as instructions when they sit in its own workspace. (The
// agent's own SOUL.md and AGENTS.md are in its folder, which is protected:
// a tainted turn's write there already needs a human.)
var InstructionFileNames = []string{"AGENTS.md", "CLAUDE.md", ".cursorrules"}

// instructionWrite refuses a file tool's write to an instruction file in a
// turn that read outside content or came from a guest, wherever the file
// is: text planted in a web page must not become standing instructions
// for every later session, the way it cannot set the identity or write
// trusted memory. The name is checked as written and after symlinks, so a
// link named notes.md cannot reach AGENTS.md. The shell is not covered
// here; in such a turn it needs a human anyway.
func instructionWrite(env *Env, t Tool, call llm.Block) error {
	guest := env.Policy != nil && env.Policy.Guest
	if !env.Taint.Marked() && !guest {
		return nil
	}
	var paths []string
	if pt, ok := t.(PathTool); ok {
		if p, err := pt.TargetPath(call.Input); err == nil {
			paths = append(paths, p)
		}
	}
	if pt, ok := t.(PathsTool); ok {
		if ps, err := pt.TargetPaths(call.Input); err == nil {
			paths = append(paths, ps...)
		}
	}
	for _, p := range paths {
		abs := env.Resolve(p)
		names := []string{filepath.Base(abs)}
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			names = append(names, filepath.Base(real))
		}
		for _, n := range names {
			if isInstructionFile(n) {
				why := "outside content entered this turn"
				if guest {
					why = "this message is not from the owner"
				}
				return fmt.Errorf("%w: %s holds instructions that go into the agent's prompt, and %s, so it cannot be changed now; ask the user to make the change, or to confirm it in their next message", ErrDenied, p, why)
			}
		}
	}
	return nil
}

func isInstructionFile(name string) bool {
	for _, n := range InstructionFileNames {
		if strings.EqualFold(name, n) {
			return true
		}
	}
	return false
}

// fileState is what an instruction file in the workspace was before a
// call, so the call's change can be undone.
type fileState struct {
	exists  bool
	info    os.FileInfo
	link    string // symlink target
	content []byte // a regular file's bytes, up to maxRestore
}

// maxRestore is the largest instruction file guardInstructionFiles keeps
// a copy of; the prompt reads 16 KB of each.
const maxRestore = 1 << 20

func instructionState(path string) fileState {
	info, err := os.Lstat(path)
	if err != nil {
		return fileState{}
	}
	st := fileState{exists: true, info: info}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		st.link, _ = os.Readlink(path)
	case info.Mode().IsRegular() && info.Size() <= maxRestore:
		st.content, _ = os.ReadFile(path)
	}
	return st
}

func (a fileState) same(b fileState) bool {
	if a.exists != b.exists {
		return false
	}
	if !a.exists {
		return true
	}
	return a.info.Mode() == b.info.Mode() && a.info.Size() == b.info.Size() && a.info.ModTime().Equal(b.info.ModTime()) && a.link == b.link
}

// restore puts the file back as it was.
func (a fileState) restore(path string) error {
	cur, err := os.Lstat(path)
	if err == nil && cur.IsDir() {
		return fmt.Errorf("%s is now a folder; remove it by hand", path)
	}
	if err == nil {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	switch {
	case !a.exists:
		return nil
	case a.link != "":
		return os.Symlink(a.link, path)
	case a.info.Mode().IsRegular() && a.content != nil:
		if err := os.WriteFile(path, a.content, a.info.Mode().Perm()); err != nil {
			return err
		}
		return os.Chtimes(path, a.info.ModTime(), a.info.ModTime())
	}
	return fmt.Errorf("%s changed and its old content was too large to keep", path)
}

// guardInstructionFiles covers every tool, not only the file tools that
// instructionWrite understands: a browser download, a generated image, a
// received attachment or an MCP tool can also save a file under a name it
// was given. In a turn that read outside content or came from a guest,
// and for any call that brings outside content in, it notes the
// workspace's instruction files before the call; the function it returns
// puts back anything the call created or changed there and says so. The
// shell is left out: in such a turn it needs a human, who sees the
// command. It returns nil when there is nothing to guard.
func guardInstructionFiles(env *Env, t Tool) func() string {
	guest := env.Policy != nil && env.Policy.Guest
	if env.Workspace == "" || env.remote() || t.Risk() == RiskExec || (!env.Taint.Marked() && !guest && !taints(t)) {
		return nil
	}
	before := map[string]fileState{}
	for _, n := range InstructionFileNames {
		p := filepath.Join(env.Workspace, n)
		before[p] = instructionState(p)
	}
	return func() string {
		var put, failed []string
		for _, n := range InstructionFileNames {
			p := filepath.Join(env.Workspace, n)
			was := before[p]
			if was.same(instructionState(p)) {
				continue
			}
			if err := was.restore(p); err != nil {
				failed = append(failed, err.Error())
				continue
			}
			put = append(put, n)
		}
		var notes []string
		if len(put) > 0 {
			notes = append(notes, fmt.Sprintf("this call wrote %s in the workspace; those files hold instructions for future sessions and this turn has outside content in it, so Habakkuk put them back as they were. Save it under another name, or ask the user to make the change.", strings.Join(put, ", ")))
		}
		if len(failed) > 0 {
			notes = append(notes, "could not put back an instruction file: "+strings.Join(failed, "; "))
		}
		return strings.Join(notes, "\n")
	}
}
