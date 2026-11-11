//go:build unix

package platform

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

const exeSuffix = ""

func defaultDataDir() string { return defaultDataDirUnix() }

// SetProcessGroup makes cmd lead a new process group, so KillGroup can end it
// and everything it started.
func SetProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Detach makes cmd start in a new session, so it outlives this process and
// its terminal (an app being opened).
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// KillGroup sends SIGKILL to the process group led by pid.
func KillGroup(pid int) error {
	if pid <= 0 {
		return errors.New("invalid pid")
	}
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// TerminateGroup asks the process group led by pid to stop (SIGTERM).
func TerminateGroup(pid int) error {
	if pid <= 0 {
		return errors.New("invalid pid")
	}
	return syscall.Kill(-pid, syscall.SIGTERM)
}

// Kill kills the single process pid.
func Kill(pid int) error {
	if pid <= 0 {
		return errors.New("invalid pid")
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}

// Alive reports whether a process with pid exists (even one owned by
// another user).
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// FileOwner returns the uid that owns a file, or -1 when unknown.
func FileOwner(info os.FileInfo) int {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}

// Links returns how many names (hard links) the file at path has, after
// symlinks. An error means the count could not be read; callers that
// guard files treat that as more than one.
func Links(path string) (uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("no link count for " + path)
	}
	return uint64(st.Nlink), nil //nolint:unconvert // uint16 on some platforms
}

// DiskUsage reports the bytes available to unprivileged users and the total
// size of the file system holding path.
func DiskUsage(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)                                     //nolint:gosec,unconvert // Bsize is int64 on Linux, uint32 on macOS
	return uint64(st.Bavail) * bs, uint64(st.Blocks) * bs, nil //nolint:unconvert // field types differ by OS
}

// SyncDir flushes a directory, so a rename in it survives a crash.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Lock takes an exclusive advisory lock on f, waiting for it.
func Lock(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX) //nolint:gosec // fds fit in int
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// TryLock takes an exclusive advisory lock on f if nobody holds it, and
// reports whether it did.
func TryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // fds fit in int
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EINTR):
		return false, nil
	}
	return false, err
}

// Unlock releases a lock taken with Lock or TryLock. Closing f releases it
// too.
func Unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // fds fit in int
}
