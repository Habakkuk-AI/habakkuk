package tools

import (
	"fmt"
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
