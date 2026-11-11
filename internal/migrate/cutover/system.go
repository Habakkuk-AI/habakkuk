package cutover

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// System is everything the cutover reads from and changes on the
// computer outside the two homes: the processes running, the service
// manager, and the old agent's own command. Tests use a fake, so no test
// ever stops or changes anything real.
type System interface {
	// GOOS is the operating system: linux, darwin, windows.
	GOOS() string
	// UserHome is the user's home folder, where user services live.
	UserHome() string
	// Processes lists the processes running now.
	Processes(ctx context.Context) ([]Process, error)
	// Alive reports whether a process is still there.
	Alive(pid int) bool
	// Stop asks a process to end; force ends it without asking.
	Stop(pid int, force bool) error
	// Run runs a program (systemctl, launchctl, schtasks) and returns what
	// it printed. An error carries its output.
	Run(ctx context.Context, name string, args ...string) (string, error)
	// Self is this process, which is never stopped.
	Self() int
	// UID is the user's id, for launchd's gui/<uid> domain (-1 on Windows).
	UID() int
	// Wait waits between checks; tests make it return at once.
	Wait(d time.Duration)
}

// Process is one running process.
type Process struct {
	PID, PPID int
	// Cmdline is its command line as the system shows it: the program and
	// its arguments, separated by spaces.
	Cmdline string
}

// Args splits the command line on spaces. Paths with spaces split too,
// which the patterns allow for: they match on the program's base name and
// on whole words.
func (p Process) Args() []string { return strings.Fields(p.Cmdline) }

// Exe is the program's name, without its folder or .exe, also when its
// path is quoted and holds spaces ("C:\Program Files\nodejs\node.exe").
func (p Process) Exe() string {
	c := strings.TrimSpace(p.Cmdline)
	if strings.HasPrefix(c, `"`) {
		if end := strings.Index(c[1:], `"`); end >= 0 {
			return program(c[1 : end+1])
		}
	}
	f := strings.Fields(c)
	if len(f) == 0 {
		return ""
	}
	return program(f[0])
}

// Host is the real System.
type Host struct{}

func (Host) GOOS() string { return runtime.GOOS }

func (Host) UserHome() string {
	h, _ := os.UserHomeDir()
	return h
}

func (Host) Self() int { return os.Getpid() }

func (Host) UID() int { return os.Getuid() }

func (Host) Wait(d time.Duration) { time.Sleep(d) }

func (Host) Run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	text := strings.TrimSpace(out.String())
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, firstLine(text))
		}
		return text, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return text, nil
}

func (h Host) Processes(ctx context.Context) ([]Process, error) {
	if runtime.GOOS == "windows" {
		// One line per process: id, parent id and command line, tab
		// separated. CommandLine is empty for processes of other users.
		out, err := h.Run(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command",
			"Get-CimInstance Win32_Process | ForEach-Object { \"$($_.ProcessId)`t$($_.ParentProcessId)`t$($_.CommandLine)\" }")
		if err != nil {
			return nil, err
		}
		return parseProcesses(out, "\t"), nil
	}
	out, err := h.Run(ctx, "ps", "-axww", "-o", "pid=,ppid=,command=")
	if err != nil {
		return nil, err
	}
	return parseProcesses(out, ""), nil
}

// parseProcesses reads "PID PPID COMMAND LINE" lines; sep "" splits the
// two numbers off on spaces.
func parseProcesses(out, sep string) []Process {
	var list []Process
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		var f []string
		if sep == "" {
			f = strings.SplitN(strings.Join(strings.Fields(line), " "), " ", 3)
		} else {
			f = strings.SplitN(line, sep, 3)
		}
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(strings.TrimSpace(f[0]))
		ppid, err2 := strconv.Atoi(strings.TrimSpace(f[1]))
		cmd := strings.TrimSpace(f[2])
		if err1 != nil || err2 != nil || cmd == "" {
			continue
		}
		list = append(list, Process{PID: pid, PPID: ppid, Cmdline: cmd})
	}
	return list
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

var errBadPID = errors.New("refusing to stop a system process")
