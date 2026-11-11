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
	// Processes lists the processes running now, each marked with
	// whether it belongs to another user.
	Processes(ctx context.Context) ([]Process, error)
	// Inspect reads what it can about one of the user's processes: its
	// environment, working folder and (on Linux) control group. Fields it
	// cannot read stay empty; Env is nil when the environment is unreadable.
	Inspect(ctx context.Context, pid int) ProcInfo
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
	// Other is set for a process of another user (on Windows, another
	// session). The cutover never touches those.
	Other bool `json:"other,omitempty"`
}

// ProcInfo is what Inspect could read about a process.
type ProcInfo struct {
	Env    map[string]string // nil: unreadable
	Cwd    string
	Cgroup string
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
		// One line per process: id, parent id, 1 when it runs in this
		// session (0 otherwise) and command line, tab separated.
		out, err := h.Run(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command",
			"$s = (Get-Process -Id $PID).SessionId; Get-CimInstance Win32_Process | ForEach-Object { \"$($_.ProcessId)`t$($_.ParentProcessId)`t$([int]($_.SessionId -eq $s))`t$($_.CommandLine)\" }")
		if err != nil {
			return nil, err
		}
		return parseProcesses(out, "\t", func(f string) bool { return f != "1" }), nil
	}
	out, err := h.Run(ctx, "ps", "-axww", "-o", "pid=,ppid=,uid=,command=")
	if err != nil {
		return nil, err
	}
	me := strconv.Itoa(os.Getuid())
	return parseProcesses(out, "", func(f string) bool { return f != me }), nil
}

// parseProcesses reads "PID PPID OWNER COMMAND LINE" lines; sep "" splits
// the first three fields off on spaces. other says whether an OWNER field
// is another user's.
func parseProcesses(out, sep string, other func(owner string) bool) []Process {
	var list []Process
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSuffix(line, "\r")
		var f []string
		if sep == "" {
			f = splitFields(strings.TrimSpace(line), 4)
		} else {
			f = strings.SplitN(line, sep, 4)
		}
		if len(f) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(strings.TrimSpace(f[0]))
		ppid, err2 := strconv.Atoi(strings.TrimSpace(f[1]))
		cmd := strings.TrimSpace(f[3])
		if err1 != nil || err2 != nil || cmd == "" {
			continue
		}
		list = append(list, Process{PID: pid, PPID: ppid, Cmdline: cmd, Other: other(strings.TrimSpace(f[2]))})
	}
	return list
}

// splitFields splits off the first n-1 space-separated fields and keeps
// the rest of the line, its spacing included, as the last.
func splitFields(s string, n int) []string {
	var out []string
	for len(out) < n-1 {
		s = strings.TrimLeft(s, " \t")
		i := strings.IndexAny(s, " \t")
		if i < 0 {
			if s != "" {
				out = append(out, s)
			}
			return out
		}
		out = append(out, s[:i])
		s = s[i:]
	}
	return append(out, strings.TrimSpace(s))
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

var errBadPID = errors.New("refusing to stop a system process")
