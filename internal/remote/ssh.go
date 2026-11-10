package remote

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// DefaultBin is the binary run on an SSH remote when -bin is not set. When
// it is not on the PATH of the non-interactive ssh session (which often
// leaves out ~/.local/bin), ~/.local/bin/habakkuk is tried.
const DefaultBin = "habakkuk"

// script runs on the server under sh. Its arguments are the binary, the
// HABAKKUK_HOME to set ("" to leave the environment alone) and the command.
// A leading "~/" in either path means the remote user's home. It holds no
// single quote, so Quote keeps it one word.
const script = `b=$1 h=$2; shift 2; ` +
	`case $b in "~/"*) b=$HOME/${b#"~/"};; esac; ` +
	`case $h in "~/"*) h=$HOME/${h#"~/"};; esac; ` +
	`if [ -n "$h" ]; then HABAKKUK_HOME=$h; export HABAKKUK_HOME; fi; ` +
	`if [ "$b" = habakkuk ] && ! command -v habakkuk >/dev/null 2>&1 && [ -x "$HOME/.local/bin/habakkuk" ]; then b=$HOME/.local/bin/habakkuk; fi; ` +
	`if ! command -v "$b" >/dev/null 2>&1; then echo "hk: $b not found on the remote; set its path with: hk remote add NAME TARGET -bin PATH -force" >&2; exit 127; fi; ` +
	`exec "$b" "$@"`

var plainWord = regexp.MustCompile(`^[A-Za-z0-9_./-][A-Za-z0-9_./:@%+=,-]*$`)

// Quote returns s as one word for a POSIX shell (sh, bash, dash, zsh): as
// is when it holds nothing a shell would interpret, else in single quotes.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	if plainWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RemoteCommand is the command line the server's login shell runs. Every
// argument is quoted, so spaces, quotes, $, ; and globs reach habakkuk
// unchanged.
func RemoteCommand(rem Remote, args []string) string {
	bin := rem.Bin
	if bin == "" {
		bin = DefaultBin
	}
	words := []string{"sh", "-c", Quote(script), "habakkuk", Quote(bin), Quote(rem.Home)}
	for _, a := range args {
		words = append(words, Quote(a))
	}
	return strings.Join(words, " ")
}

// SSHArgs are the arguments for the ssh client: a TTY (-t) for interactive
// use, none (-T) otherwise, so output can be piped.
func SSHArgs(rem Remote, t Target, args []string, tty bool) []string {
	a := []string{"-T"}
	if tty {
		a[0] = "-t"
	}
	if t.Port != 0 {
		a = append(a, "-p", strconv.Itoa(t.Port))
	}
	if rem.Identity != "" {
		a = append(a, "-i", rem.Identity, "-o", "IdentitiesOnly=yes")
	}
	return append(a, "--", t.Dest(), RemoteCommand(rem, args))
}

// IsTerminal reports whether f is an *os.File on a terminal.
func IsTerminal(f any) bool {
	file, ok := f.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

// WantTTY decides whether ssh should allocate a TTY: only when both stdin
// and stdout are terminals (chat, the TUI, wizards). A pipe on either side
// gets none, so `hk -r srv status | grep x` sees plain output.
func WantTTY(stdin, stdout any, isTerminal func(any) bool) bool {
	return isTerminal(stdin) && isTerminal(stdout)
}

// Stdio is where a remote command reads and writes.
type Stdio struct {
	In       io.Reader
	Out, Err io.Writer
}

// ErrNoSSH is returned when there is no ssh client on the PATH.
var ErrNoSSH = errors.New("no ssh client on the PATH: install OpenSSH " +
	"(on Windows 10 and later: Settings, Apps, Optional features, OpenSSH Client)")

// RunSSH runs args on the remote with the system ssh client and returns the
// remote command's exit code (ssh itself exits 255 when it cannot connect).
func RunSSH(rem Remote, args []string, tty bool, std Stdio) (int, error) {
	t, err := ParseTarget(rem.Target)
	if err != nil {
		return 2, err
	}
	if t.Kind != SSH {
		return 2, fmt.Errorf("%s is not an ssh remote", rem.Target)
	}
	bin, err := exec.LookPath("ssh")
	if err != nil {
		return 1, ErrNoSSH
	}
	cmd := exec.Command(bin, SSHArgs(rem, t, args, tty)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = std.In, std.Out, std.Err
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code >= 0 {
			return code, nil
		}
		return 1, nil // killed by a signal
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}
