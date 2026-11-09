package tools

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// CommandTool is implemented by tools that run shell commands, so the
// hardline blocklist can check them before anything else.
type CommandTool interface {
	Commands(input json.RawMessage) []string
}

func (execTool) Commands(input json.RawMessage) []string {
	in, err := decode[execInput](input)
	if err != nil {
		return nil
	}
	return []string{in.Command}
}

// processWrite's input may be a line typed into a shell.
func (processWrite) Commands(input json.RawMessage) []string {
	in, err := decode[struct {
		Stdin string `json:"stdin"`
	}](input)
	if err != nil {
		return nil
	}
	return []string{in.Stdin}
}

// RefusingTool is implemented by tools with a hardline of their own: inputs
// they never act on, in any mode and whoever approves (the computer tools
// refuse to type text that looks like a secret). Refuse says why, or nil.
type RefusingTool interface {
	Refuse(input json.RawMessage) error
}

// hardlineCheck refuses a call whose commands are on the blocklist, before
// approval is asked for: no mode, reviewer or human can allow them.
func hardlineCheck(t Tool, input json.RawMessage) error {
	if rt, ok := t.(RefusingTool); ok {
		if err := rt.Refuse(input); err != nil {
			return fmt.Errorf("%w: refused: %v; this never runs, in any mode", ErrDenied, err)
		}
	}
	ct, ok := t.(CommandTool)
	if !ok {
		return nil
	}
	for _, c := range ct.Commands(input) {
		if err := Hardline(c); err != nil {
			return err
		}
	}
	return nil
}

// Hardline returns an error when a shell command is one Habakkuk never runs,
// in any permission mode and whoever approves: recursive deletion of / or
// the home directory, making a filesystem, dd onto a disk device, a fork
// bomb, or a script downloaded from a remote host piped into a shell. It is
// a last line of defense against a confused or manipulated model, not a
// sandbox: a determined command can always be obfuscated past it.
func Hardline(command string) error {
	if why := hardline(command); why != "" {
		return fmt.Errorf("%w: refused: %s is on Habakkuk's hardline blocklist and never runs, in any mode", ErrDenied, why)
	}
	return nil
}

var (
	funcDef = regexp.MustCompile(`([A-Za-z_:.][A-Za-z0-9_:.]*)\s*\(\s*\)\s*\{([^}]*)\}`)
	// fetchToShell: curl or wget output piped into a shell or interpreter.
	fetchToShell = regexp.MustCompile(`(?i)\b(curl|wget|fetch)\b([^|;&\n]*)\|\s*(?:sudo\s+(?:-\S+\s+)*)?(?:env\s+)?(?:\S*/)?(sh|bash|zsh|dash|ksh|fish|python[0-9.]*|perl|ruby|node)\b`)
	// shellOfFetch: bash <(curl ...), sh -c "$(curl ...)", eval "$(wget ...)".
	shellOfFetch = regexp.MustCompile(`(?i)\b(?:sh|bash|zsh|dash|ksh|eval|source|\.)\s+(?:-c\s+)?["']?(?:<\(|\$\(|` + "`" + `)\s*(curl|wget|fetch)\b([^)` + "`" + `]*)`)
	urlIn        = regexp.MustCompile(`(?i)\b(?:https?|ftp)://[^\s"'|;&)]+`)
	diskDevice   = regexp.MustCompile(`^/dev/(?:sd[a-z]|hd[a-z]|vd[a-z]|xvd[a-z]|nvme\d|mmcblk\d|disk\d|rdisk\d|md\d|dm-\d|mapper/|loop\d|sr\d|zd\d)`)
	redirectDev  = regexp.MustCompile(`>\s*/dev/(?:sd[a-z]|hd[a-z]|vd[a-z]|xvd[a-z]|nvme\d|mmcblk\d|disk\d|rdisk\d)`)
)

func hardline(command string) string {
	cmd := strings.NewReplacer("\\\n", " ", "${HOME}", "$HOME").Replace(command)
	if forkBomb(cmd) {
		return "a fork bomb"
	}
	for _, m := range fetchToShell.FindAllStringSubmatch(cmd, -1) {
		if remoteFetch(m[2]) {
			return "piping a download from a remote host into " + m[3]
		}
	}
	for _, m := range shellOfFetch.FindAllStringSubmatch(cmd, -1) {
		if remoteFetch(m[2]) {
			return "running a script downloaded from a remote host"
		}
	}
	if redirectDev.MatchString(cmd) {
		return "writing straight onto a disk device"
	}
	if why := hardlineWindows(cmd); why != "" {
		return why
	}
	for _, seg := range segments(cmd) {
		if why := checkSegment(seg); why != "" {
			return why
		}
	}
	return ""
}

// forkBomb finds a function that pipes itself into itself in the
// background, like :(){ :|:& };:
func forkBomb(cmd string) bool {
	for _, m := range funcDef.FindAllStringSubmatch(cmd, -1) {
		body := strings.Join(strings.Fields(m[2]), "")
		if strings.Contains(body, m[1]+"|"+m[1]+"&") {
			return true
		}
	}
	return false
}

// remoteFetch reports whether a download's arguments name a host other than
// this machine. No URL at all counts as remote (it may come from a variable).
func remoteFetch(args string) bool {
	urls := urlIn.FindAllString(args, -1)
	if len(urls) == 0 {
		return true
	}
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil {
			return true
		}
		switch h := strings.ToLower(u.Hostname()); {
		case h == "localhost", h == "::1", strings.HasPrefix(h, "127."), strings.HasSuffix(h, ".localhost"):
		default:
			return true
		}
	}
	return false
}

// segments splits a command line into simple commands at ;, &&, ||, |, &,
// newlines and command substitutions. Quotes are dropped rather than
// honoured, which errs toward catching more.
func segments(cmd string) [][]string {
	r := strings.NewReplacer("&&", "\n", "||", "\n", ";", "\n", "|", "\n", "&", "\n",
		"$(", "\n", "`", "\n", "(", "\n", ")", "\n", "{", "\n", "}", "\n", "\"", " ", "'", " ")
	var out [][]string
	for _, line := range strings.Split(r.Replace(cmd), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			out = append(out, f)
		}
	}
	return out
}

// wrappers run the command that follows them.
var wrappers = map[string]bool{"sudo": true, "doas": true, "command": true, "exec": true, "nice": true,
	"nohup": true, "time": true, "env": true, "builtin": true, "xargs": true, "busybox": true, "ionice": true}

// commandWord skips variable assignments and wrappers (with their flags)
// and returns the index of the command a simple command runs, or -1.
func commandWord(words []string) int {
	for i := 0; i < len(words); {
		w := words[i]
		switch {
		case strings.Contains(w, "=") && !strings.HasPrefix(w, "-"):
			i++
		case wrappers[path.Base(w)]:
			i++
			for i < len(words) && strings.HasPrefix(words[i], "-") {
				i++
			}
		default:
			return i
		}
	}
	return -1
}

func checkSegment(words []string) string {
	i := commandWord(words)
	if i < 0 {
		return ""
	}
	name, args := path.Base(words[i]), words[i+1:]
	switch {
	case name == "rm":
		return checkRm(args)
	case name == "mkfs" || strings.HasPrefix(name, "mkfs.") || name == "mke2fs" || name == "newfs" || strings.HasPrefix(name, "newfs_"):
		return "making a filesystem (" + name + ")"
	case name == "dd":
		for _, a := range args {
			if dev, ok := strings.CutPrefix(a, "of="); ok && diskDevice.MatchString(dev) {
				return "dd onto a disk device (" + dev + ")"
			}
		}
	case name == "diskutil" && len(args) > 0 && strings.HasPrefix(strings.ToLower(args[0]), "erase"):
		return "erasing a disk (diskutil " + args[0] + ")"
	}
	return ""
}

// checkRm refuses a recursive rm whose target is /, the home directory or
// everything directly under either.
func checkRm(args []string) string {
	recursive, opts := false, true
	var targets []string
	for _, a := range args {
		switch {
		case opts && a == "--":
			opts = false
		case opts && (a == "--recursive" || a == "--no-preserve-root"):
			recursive = true
		case opts && strings.HasPrefix(a, "--"):
		case opts && strings.HasPrefix(a, "-") && len(a) > 1:
			if strings.ContainsAny(a[1:], "rR") {
				recursive = true
			}
		default:
			targets = append(targets, a)
		}
	}
	if !recursive {
		return ""
	}
	for _, t := range targets {
		if dangerousTarget(t) {
			return "recursive deletion of " + t
		}
	}
	return ""
}

var homeForms = []string{"~", "$HOME", "${HOME}"}

func dangerousTarget(t string) bool {
	t = strings.TrimSpace(t)
	for strings.HasSuffix(t, "/*") || strings.HasSuffix(t, "/.") || (len(t) > 1 && strings.HasSuffix(t, "/")) {
		t = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(t, "/*"), "/."), "/")
	}
	switch t {
	case "", "/", "~":
		return true
	}
	for _, h := range homeForms {
		if t == h {
			return true
		}
	}
	// ~user and /home/user, /Users/user, /root themselves.
	if strings.HasPrefix(t, "~") && !strings.Contains(t, "/") {
		return true
	}
	clean := path.Clean(t)
	if clean == "/root" {
		return true
	}
	for _, base := range []string{"/home", "/Users"} {
		if clean == base || path.Dir(clean) == base {
			return true
		}
	}
	// Top-level system directories.
	switch clean {
	case "/bin", "/boot", "/dev", "/etc", "/lib", "/lib64", "/opt", "/proc", "/sbin", "/sys", "/usr", "/var",
		"/System", "/Library", "/Applications", "/private":
		return true
	}
	return false
}
