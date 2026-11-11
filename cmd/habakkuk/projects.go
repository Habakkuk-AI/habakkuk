package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/agent/projtrust"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"golang.org/x/term"
)

const projectsUsage = `usage: hk projects trust [-allow-control] [PATH] | list [-agent A] [PATH] | untrust [PATH]
  trust [PATH]    show exactly what an agent working in PATH (default: this folder) would
                  get from each project instruction file (AGENTS.md, CLAUDE.md, .cursorrules),
                  or from the one file PATH names, and ask, on your terminal, whether to trust
                  that exact content. -allow-control trusts a file with control or invisible
                  format characters
  list            every trusted file and whether it changed since
  list PATH       what an agent working in PATH (or the one file PATH names) really gets,
                  and whether each file is trusted
  untrust [PATH]  stop trusting a file, or every file under a folder (default: this folder)`

// errNoTerminal is returned when nobody is at a terminal to ask.
var errNoTerminal = errors.New("hk projects trust asks you on your terminal, and there is none: run it yourself in a terminal")

// projectsTTY opens the terminal of the person running the command, for
// showing the files and reading the answer. Standard input and output are
// not used for that: they can be a pipe or /dev/null. It is a hurdle for
// scripts and agents, not a guarantee: a process on this machine that can
// fake a terminal (with script, say) can answer it. Tests replace it.
var projectsTTY = openTTY

func openTTY() (io.ReadWriteCloser, error) {
	if runtime.GOOS == "windows" {
		if !isTerminalFile(os.Stdin) || !isTerminalFile(os.Stdout) {
			return nil, errNoTerminal
		}
		return struct {
			io.Reader
			io.Writer
			io.Closer
		}{os.Stdin, os.Stdout, io.NopCloser(nil)}, nil
	}
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, errNoTerminal
	}
	if !isTerminalFile(f) {
		f.Close()
		return nil, errNoTerminal
	}
	return f, nil
}

// isTerminalFile is a real terminal check: a character device such as
// /dev/null is not one.
func isTerminalFile(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// projectsCmd runs hk projects. A project instruction file is followed as
// instructions only while its content is exactly what the owner trusted
// here; this command is the only writer of that record.
func projectsCmd(home string, args []string, in io.Reader, out io.Writer) error {
	sub := "list"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	st := &projtrust.Store{Home: home}
	switch sub {
	case "list", "ls":
		fs := flag.NewFlagSet("projects list", flag.ContinueOnError)
		fs.SetOutput(out)
		agentName := fs.String("agent", "", "the agent whose view to show (default: the default agent)")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() > 0 {
			return projectsListAt(home, st, *agentName, fs.Arg(0), out)
		}
		return projectsList(st, out)
	case "trust":
		fs := flag.NewFlagSet("projects trust", flag.ContinueOnError)
		fs.SetOutput(out)
		allowControl := fs.Bool("allow-control", false, "trust a file that holds control or invisible format characters")
		if err := fs.Parse(args); err != nil {
			return err
		}
		tty, err := projectsTTY()
		if err != nil {
			return err
		}
		defer tty.Close()
		return projectsTrust(home, st, pathArg(fs.Args()), *allowControl, tty, out)
	case "untrust", "rm":
		removed, err := st.Untrust(pathArg(args))
		if err != nil {
			return err
		}
		if len(removed) == 0 {
			fmt.Fprintln(out, "Nothing trusted there.")
		}
		for _, e := range removed {
			fmt.Fprintf(out, "no longer trusted: %s\n", e.Path)
		}
		return nil
	case "help", "-h", "--help":
		fmt.Fprintln(out, projectsUsage)
		return nil
	}
	return errors.New(projectsUsage)
}

func pathArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return "."
}

// projectAgent is an agent's folder (which the loader uses to fence off
// the Habakkuk home and the agent's own folder) and its project_files
// setting: the named agent, or the default one.
func projectAgent(home, name string) (dir, mode string, err error) {
	cfg, err := config.Load(home)
	if err != nil {
		if name != "" {
			return "", "", err
		}
		return filepath.Join(home, "agents", "habakkuk"), agent.ProjectFilesOn, nil
	}
	if name, err = defaultAgent(cfg, name); err != nil {
		return "", "", err
	}
	return cfg.AgentDir(name), cfg.Agents[name].ProjectFiles, nil
}

// projectFilesAt lists the instruction files a session working in path
// would read, or the one file path names, each with its trust now when
// trust is given.
func projectFilesAt(home, path string, trust agent.ProjectTrust) ([]agent.ProjectFile, error) {
	return projectFilesFor(home, "", path, trust, false)
}

func projectFilesFor(home, agentName, path string, trust agent.ProjectTrust, honourMode bool) ([]agent.ProjectFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		if !slices.Contains(agent.ProjectFileNames, filepath.Base(path)) {
			return nil, fmt.Errorf("%s is not a project instruction file (%s)", path, strings.Join(agent.ProjectFileNames, ", "))
		}
		abs, _ := filepath.Abs(path)
		f := agent.ProjectFile{Path: abs, Real: abs}
		if real, err := projtrust.Real(abs); err == nil {
			f.Real = real
		}
		if sum, _, err := projtrust.Hash(f.Real); err != nil {
			f.Skipped = err.Error()
		} else if trust != nil {
			f.SHA256 = sum
			f.Trusted, f.Changed = trust.Check(f.Real, sum)
			f.Changed = f.Changed && !f.Trusted
		}
		return []agent.ProjectFile{f}, nil
	}
	dir, mode, err := projectAgent(home, agentName)
	if err != nil {
		return nil, err
	}
	if !honourMode {
		mode = agent.ProjectFilesOn
	}
	return agent.LoadProjectFiles(dir, path, mode, trust).Files, nil
}

// projectsTrust shows each file on the terminal and asks there; standard
// output only gets the result lines.
func projectsTrust(home string, st *projtrust.Store, path string, allowControl bool, tty io.ReadWriter, out io.Writer) error {
	files, err := projectFilesAt(home, path, nil)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Fprintf(out, "No project instruction files (%s) for %s.\n", strings.Join(agent.ProjectFileNames, ", "), path)
		return nil
	}
	answers := bufio.NewReader(tty)
	trusted := 0
	for _, f := range files {
		if f.Skipped != "" && !strings.Contains(f.Skipped, "already fill") {
			fmt.Fprintf(out, "skipped %s: %s\n", f.Path, f.Skipped)
			continue
		}
		real, err := projtrust.Real(f.Path)
		if err != nil {
			return err
		}
		sum, data, err := projtrust.Hash(real)
		if err != nil {
			fmt.Fprintf(out, "skipped %s: %v\n", f.Path, err)
			continue
		}
		if ok, _ := st.Check(real, sum); ok {
			fmt.Fprintf(out, "already trusted: %s\n", f.Path)
			continue
		}
		hidden := hiddenChars(data)
		showFile(tty, f.Path, sum, data)
		if len(hidden) > 0 && !allowControl {
			fmt.Fprintf(out, "not trusted: %s holds %d control or invisible format character(s) (%s), which can make what you see differ from what the agent gets. Remove them, or pass -allow-control once you have read the file with them shown.\n",
				f.Path, len(hidden), strings.Join(hidden, ", "))
			continue
		}
		fmt.Fprint(tty, "Agents will follow exactly the text between the --- lines as instructions. Trust it? [y/N] ")
		line, _ := answers.ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintf(out, "not trusted: %s\n", f.Path)
			continue
		}
		e, _, err := st.Trust(real)
		if err != nil {
			return err
		}
		if e.SHA256 != sum {
			// It changed between showing and recording: trust nothing.
			_, _ = st.Untrust(real)
			fmt.Fprintf(out, "not trusted: %s changed while you read it; run this again\n", f.Path)
			continue
		}
		trusted++
		fmt.Fprintf(out, "trusted: %s (sha256 %s)\n", e.Path, e.SHA256[:12])
	}
	if trusted > 0 {
		fmt.Fprintln(out, "New sessions follow these files while their content stays exactly the same. Any change makes a file untrusted again until you run hk projects trust again.")
	}
	return nil
}

// hiddenChars lists the control and format characters in a file, other
// than tab, newline and the carriage return of a CRLF line end, as they are
// shown.
func hiddenChars(data []byte) []string {
	seen := map[string]bool{}
	var out []string
	s := string(data)
	for i, r := range s {
		if r == '\n' || r == '\t' || (r == '\r' && strings.HasPrefix(s[i+1:], "\n")) {
			continue
		}
		if v := visible(r); v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// visible is how a control or format character is shown, or "" for any
// other character.
func visible(r rune) string {
	switch {
	case r < 0x20 || r == 0x7f:
		return fmt.Sprintf(`\x%02x`, r)
	case unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Variation_Selector, r):
		return fmt.Sprintf("<U+%04X>", r)
	}
	return ""
}

// showFile prints exactly the text the agent gets from the file (trimmed,
// cut at 16 KB, look-alike tags escaped), with every control and format
// character of the file made visible, so no terminal sequence or bidi
// control can make the screen differ from the prompt.
func showFile(out io.Writer, path, sum string, data []byte) {
	text, cut := agent.ProjectPromptText(data)
	fmt.Fprintf(out, "\n%s (%d bytes, sha256 %s). The agent gets the text between the --- lines", visibleText(path), len(data), sum[:12])
	if cut {
		fmt.Fprint(out, "; the rest of the file is cut and never reaches it")
	}
	fmt.Fprintln(out, ":\n---")
	// Format characters were already dropped from what the agent gets; the
	// file's own are listed by hiddenChars.
	fmt.Fprintln(out, visibleText(text))
	fmt.Fprintln(out, "---")
}

func visibleText(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r == '\n' || r == '\t' || (r == '\r' && strings.HasPrefix(s[i+1:], "\n")) {
			if r != '\r' {
				b.WriteRune(r)
			}
			continue
		}
		if v := visible(r); v != "" {
			b.WriteString(v)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func projectsList(st *projtrust.Store, out io.Writer) error {
	entries, status, err := st.List()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(out, "No trusted project files. Every project's AGENTS.md, CLAUDE.md and .cursorrules is read as untrusted data until you run hk projects trust in its folder.")
		return nil
	}
	for i, e := range entries {
		fmt.Fprintf(out, "%-28s %s  (sha256 %s, trusted %s)\n", status[i], visibleText(e.Path), e.SHA256[:12], e.TrustedAt.Local().Format("2006-01-02 15:04"))
	}
	return nil
}

// projectsListAt shows what an agent working in path really gets: each
// file it reads (with that agent's fences and project_files setting), and
// whether that file is trusted now.
func projectsListAt(home string, st *projtrust.Store, agentName, path string, out io.Writer) error {
	if _, mode, err := projectAgent(home, agentName); err == nil && mode == agent.ProjectFilesOff {
		fmt.Fprintln(out, "project_files is off for this agent: it reads no project instruction files.")
		return nil
	}
	files, err := projectFilesFor(home, agentName, path, st, true)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Fprintf(out, "An agent working in %s reads no project instruction files.\n", visibleText(path))
		return nil
	}
	for _, f := range files {
		status := "untrusted (given to the agent as data; the session's commands need your approval)"
		switch {
		case f.Skipped != "":
			status = "left out: " + f.Skipped
		case f.Trusted:
			status = "trusted (followed as instructions)"
		case f.Changed:
			status = "changed since you trusted it (given as data until you trust it again)"
		}
		fmt.Fprintf(out, "%s\n  %s\n", visibleText(f.Path), status)
	}
	return nil
}
