package tools

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// InstructionFileNames are the project files Habakkuk reads into new
// sessions. Whether one is followed depends on its exact content having
// been trusted by the owner (hk projects trust); refusing a tainted turn's
// file tools here is a cheap extra, not what keeps them safe. (The
// agent's own SOUL.md and AGENTS.md are in its folder, which is protected:
// a tainted turn's write there already needs a human.)
var InstructionFileNames = []string{"AGENTS.md", "CLAUDE.md", ".cursorrules"}

// instructionWrite refuses a file tool's write to an instruction file in a
// turn that read outside content or came from a guest, wherever the file
// is: text planted in a web page must not become standing instructions
// for every later session, the way it cannot set the identity or write
// trusted memory. The name is checked as written and after symlinks, so a
// link named notes.md cannot reach AGENTS.md. Other tools that save files
// are not covered: a file they leave is untrusted anyway, because its
// exact content was never trusted (hk projects trust).
func instructionWrite(env *Env, t Tool, call llm.Block) error {
	guest := env.Policy != nil && env.Policy.Guest
	if !env.Taint.Marked() && !guest {
		return nil
	}
	for _, p := range targetPaths(t, call) {
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

// targetPaths are the paths a file tool's call would write.
func targetPaths(t Tool, call llm.Block) []string {
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
	return paths
}

// lockedWrite refuses a file tool's write under one of the turn's locked
// folders (Policy.Locked), in any turn and any mode: the owner's record of
// trusted project files must only change through hk projects. The path is
// checked as written and after symlinks, and folders are compared with
// os.SameFile, so another letter case does not get past. (The shell is not
// a file tool: in a clean turn in full mode it can do anything.)
func lockedWrite(env *Env, t Tool, call llm.Block) error {
	if env.Policy == nil || len(env.Policy.Locked) == 0 {
		return nil
	}
	for _, p := range targetPaths(t, call) {
		abs := env.Resolve(p)
		real, ok := realPath(abs)
		if env.locked(abs) || (ok && env.locked(real)) {
			return fmt.Errorf("%w: %s is kept by Habakkuk itself (the owner's list of trusted project files); only the owner changes it, with hk projects", ErrDenied, p)
		}
	}
	return nil
}
