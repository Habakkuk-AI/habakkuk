//go:build windows

package platform

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

const exeSuffix = ".exe"

// defaultDataDir is %APPDATA%\Habakkuk, the per-user roaming application
// data folder, falling back to the Unix layout when it is not set.
func defaultDataDir() string {
	if d := os.Getenv("APPDATA"); d != "" {
		return filepath.Join(d, "Habakkuk")
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "Habakkuk")
	}
	return defaultDataDirUnix()
}

// SetProcessGroup starts cmd in a new process group with no console window
// of its own. KillGroup then ends the process tree rooted at it.
func SetProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW
}

// Detach starts cmd detached from this process's console, so it outlives
// it (an app being opened).
func Detach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS
}

// KillGroup ends the process tree rooted at pid with taskkill /T /F, which
// walks child processes; if that fails it kills pid alone.
func KillGroup(pid int) error {
	if pid <= 0 {
		return errors.New("invalid pid")
	}
	tk := "taskkill"
	if root := os.Getenv("SystemRoot"); root != "" {
		tk = filepath.Join(root, "System32", "taskkill.exe")
	}
	cmd := exec.Command(tk, "/T", "/F", "/PID", strconv.Itoa(pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	if err := cmd.Run(); err == nil || !Alive(pid) {
		return nil
	}
	return Kill(pid)
}

// TerminateGroup is KillGroup on Windows: a console program in another
// process group cannot be sent a polite Ctrl+C.
func TerminateGroup(pid int) error { return KillGroup(pid) }

// Kill ends the single process pid.
func Kill(pid int) error {
	if pid <= 0 {
		return errors.New("invalid pid")
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid)) //nolint:gosec // pids fit in uint32
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h) //nolint:errcheck // nothing to do about it
	return windows.TerminateProcess(h, 1)
}

// stillActive is the exit code GetExitCodeProcess reports for a process
// that has not exited (STILL_ACTIVE).
const stillActive = 259

// Alive reports whether a process with pid exists and has not exited.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid)) //nolint:gosec // pids fit in uint32
	if err != nil {
		// Access denied means it exists but belongs to someone else.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h) //nolint:errcheck // nothing to do about it
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}

// FileOwner returns -1: Windows files are owned by a SID, not a uid.
func FileOwner(os.FileInfo) int { return -1 }

// Links returns how many names (hard links) a file has; os.FileInfo does
// not say on Windows, so it is 1.
func Links(os.FileInfo) uint64 { return 1 }

// DiskUsage reports the bytes available to this user and the total size of
// the volume holding path.
func DiskUsage(path string) (free, total uint64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, 0, err
	}
	return free, total, nil
}

// SyncDir does nothing on Windows: NTFS journals renames itself, and a
// directory handle cannot be flushed.
func SyncDir(string) error { return nil }

// Lock takes an exclusive lock on the first byte of f, waiting for it. Use
// a dedicated lock file: Windows locks are mandatory, so other processes
// cannot read the locked byte.
func Lock(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol)
}

// TryLock takes an exclusive lock on f if nobody holds it, and reports
// whether it did.
func TryLock(f *os.File) (bool, error) {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION), errors.Is(err, windows.ERROR_IO_PENDING):
		return false, nil
	}
	return false, err
}

// Unlock releases a lock taken with Lock or TryLock.
func Unlock(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}
