package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/agent/projtrust"
	"github.com/habakkuk-ai/habakkuk/internal/config"
)

const projectsUsage = `usage: hk projects trust [-y] [-allow-control] [PATH] | list [PATH] | untrust [PATH]
  trust [PATH]    show exactly what an agent working in PATH (default: this folder) would
                  get from each project instruction file (AGENTS.md, CLAUDE.md, .cursorrules),
                  or from the one file PATH names, and trust that exact content. It needs a
                  terminal: you, not a script, decide. -y trusts without asking;
                  -allow-control trusts a file with control or invisible format characters
  list            every trusted file and whether it changed since
  list PATH       the files an agent working in PATH reads, and whether each is trusted
  untrust [PATH]  stop trusting a file, or every file under a folder (default: this folder)`

// projectsTerminal reports whether hk projects trust is talking to a
// person: its input is a terminal. Tests replace it.
var projectsTerminal = func(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

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
		if len(args) > 0 {
			return projectsListAt(home, st, args[0], out)
		}
		return projectsList(st, out)
	case "trust":
		fs := flag.NewFlagSet("projects trust", flag.ContinueOnError)
		fs.SetOutput(out)
		yes := fs.Bool("y", false, "trust without asking")
		allowControl := fs.Bool("allow-control", false, "trust a file that holds control or invisible format characters")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if !projectsTerminal(in) {
			return errors.New("hk projects trust needs a terminal: run it yourself, so a script or an agent cannot trust files for you")
		}
		return projectsTrust(home, st, pathArg(fs.Args()), *yes, *allowControl, in, out)
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

// projectAgentDir is the default agent's folder, which the loader uses to
// fence off the Habakkuk home.
func projectAgentDir(home string) string {
	if cfg, err := config.Load(home); err == nil {
		if name, err := defaultAgent(cfg, ""); err == nil {
			return cfg.AgentDir(name)
		}
	}
	return filepath.Join(home, "agents", "habakkuk")
}

// projectFilesAt lists the instruction files a session working in path
// would read, or the one file path names.
func projectFilesAt(home, path string, trust agent.ProjectTrust) ([]agent.ProjectFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		if !slices.Contains(agent.ProjectFileNames, filepath.Base(path)) {
			return nil, fmt.Errorf("%s is not a project instruction file (%s)", path, strings.Join(agent.ProjectFileNames, ", "))
		}
		abs, _ := filepath.Abs(path)
		return []agent.ProjectFile{{Path: abs}}, nil
	}
	return agent.LoadProjectFiles(projectAgentDir(home), path, agent.ProjectFilesOn, trust).Files, nil
}

func projectsTrust(home string, st *projtrust.Store, path string, yes, allowControl bool, in io.Reader, out io.Writer) error {
	files, err := projectFilesAt(home, path, nil)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Fprintf(out, "No project instruction files (%s) for %s.\n", strings.Join(agent.ProjectFileNames, ", "), path)
		return nil
	}
	answers := bufio.NewReader(in)
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
		if !yes {
			showFile(out, f.Path, sum, data)
		}
		if len(hidden) > 0 && !allowControl {
			fmt.Fprintf(out, "not trusted: %s holds %d control or invisible format character(s) (%s), which can make what you see differ from what the agent gets. Remove them, or pass -allow-control once you have read the file with them shown.\n",
				f.Path, len(hidden), strings.Join(hidden, ", "))
			continue
		}
		if !yes {
			fmt.Fprint(out, "Agents will follow exactly the text between the --- lines as instructions. Trust it? [y/N] ")
			line, _ := answers.ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
				fmt.Fprintf(out, "not trusted: %s\n", f.Path)
				continue
			}
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
// file it reads, and whether that file is trusted now.
func projectsListAt(home string, st *projtrust.Store, path string, out io.Writer) error {
	files, err := projectFilesAt(home, path, st)
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
