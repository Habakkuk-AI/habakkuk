//go:build !windows

package nodes

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// putFile writes data to rel under root without following a link anywhere
// on the way. Each folder is opened from the one before it with
// O_NOFOLLOW (and created there when missing), the file is written under
// a random name created with O_EXCL in its final folder, and then renamed
// to its name within that same folder. A link already sitting at the name
// is replaced, never written through. protected is asked once more just
// before the rename.
func putFile(root, rel string, data []byte, protected func() bool) error {
	dirfd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { unix.Close(dirfd) }()
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, part := range parts[:len(parts)-1] {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("bad path %q", rel)
		}
		next, err := unix.Openat(dirfd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) {
			if err := unix.Mkdirat(dirfd, part, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
				return err
			}
			next, err = unix.Openat(dirfd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if err != nil {
			return fmt.Errorf("%s: %w (a link or not a folder)", part, err)
		}
		unix.Close(dirfd)
		dirfd = next
	}
	name := parts[len(parts)-1]
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("bad path %q", rel)
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := ".habakkuk-put-" + hex.EncodeToString(rnd[:])
	fd, err := unix.Openat(dirfd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), tmp)
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil && protected() {
		werr = errors.New("the target is inside Habakkuk's own folder on this machine, which files.put never writes")
	}
	if werr == nil {
		werr = unix.Renameat(dirfd, tmp, dirfd, name)
	}
	if werr != nil {
		_ = unix.Unlinkat(dirfd, tmp, 0)
	}
	return werr
}
