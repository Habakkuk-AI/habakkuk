package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Saving files. Tools that save what they fetched or made (a browser PDF,
// screenshot or download, a generated image or video, an attachment that
// arrived in a chat) write into the workspace. Checking the path as a
// string is not enough: a link inside the workspace can lead anywhere,
// such as into $HABAKKUK_HOME. OutputDir and OutputPath resolve the links
// first, require the real place to be inside the real workspace, and
// refuse the turn's locked folders whatever the spelling (os.SameFile),
// so a link or another letter case cannot lead a save into the owner's
// record of trusted project files.

// OutputDir checks a folder a tool is about to save files in, and returns
// its real path to save into.
func (e *Env) OutputDir(dir string) (string, error) {
	if e.Workspace == "" {
		return "", errors.New("this agent has no workspace to save files in")
	}
	abs := e.Resolve(dir)
	real, ok := realPath(abs)
	if !ok {
		return "", fmt.Errorf("%w: %s goes through a link that leads nowhere", ErrDenied, dir)
	}
	ws, ok := realPath(e.Workspace)
	if !ok {
		ws = e.Workspace
	}
	if !underAny([]string{ws}, real) {
		return "", fmt.Errorf("%w: %s is outside the workspace %s once its links are followed", ErrDenied, dir, e.Workspace)
	}
	if e.locked(real) {
		return "", fmt.Errorf("%w: %s is kept by Habakkuk itself (the owner's list of trusted project files)", ErrDenied, dir)
	}
	return real, nil
}

// OutputPath checks a file a tool is about to save, and returns its real
// path. Its folder is checked as OutputDir does; the file itself is
// replaced, never written through, by store.WriteFileAtomic.
func (e *Env) OutputPath(path string) (string, error) {
	abs := e.Resolve(path)
	dir, err := e.OutputDir(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(abs)), nil
}

// locked reports whether real (a path with its links resolved) is in one
// of the turn's locked folders, compared as folders and not as strings.
func (e *Env) locked(real string) bool {
	if e.Policy == nil || len(e.Policy.Locked) == 0 {
		return false
	}
	if underAny(e.Policy.Locked, real) || underAny(realPaths(e.Policy.Locked), real) {
		return true
	}
	var locked []os.FileInfo
	for _, d := range e.Policy.Locked {
		if fi, err := os.Stat(d); err == nil {
			locked = append(locked, fi)
		}
	}
	for cur := real; ; cur = filepath.Dir(cur) {
		if fi, err := os.Stat(cur); err == nil {
			for _, l := range locked {
				if os.SameFile(fi, l) {
					return true
				}
			}
		}
		if filepath.Dir(cur) == cur {
			return false
		}
	}
}
