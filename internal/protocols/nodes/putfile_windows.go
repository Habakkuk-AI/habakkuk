//go:build windows

package nodes

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// putFile writes data to rel under root. Windows has no O_NOFOLLOW for
// folders here, so the real folder is checked after it is made, the file
// is written under a random name (os.CreateTemp, exclusive), and the
// folder and the protected check are looked at again just before the
// rename.
func putFile(root, rel string, data []byte, protected func() bool) error {
	dir := filepath.Join(root, filepath.Dir(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	check := func() (string, error) {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return "", err
		}
		r, err := filepath.Rel(root, real)
		if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(r) {
			return "", fmt.Errorf("%s is outside the directory this node shares", rel)
		}
		return real, nil
	}
	real, err := check()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(real, ".habakkuk-put-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		if again, err := check(); err != nil || again != real {
			werr = errors.New("the folder changed while writing")
		}
	}
	if werr == nil && protected() {
		werr = errors.New("the target is inside Habakkuk's own folder on this machine, which files.put never writes")
	}
	if werr == nil {
		werr = os.Rename(tmp, filepath.Join(real, filepath.Base(rel)))
	}
	if werr != nil {
		os.Remove(tmp)
	}
	return werr
}
