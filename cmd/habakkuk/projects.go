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

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/agent/projtrust"
	"github.com/habakkuk-ai/habakkuk/internal/config"
)

const projectsUsage = `usage: hk projects trust [-y] [PATH] | list | untrust [PATH]
  trust [PATH]    show the project instruction files (AGENTS.md, CLAUDE.md, .cursorrules)
                  an agent working in PATH would read (default: this folder), or the one
                  file PATH names, and trust their exact content; -y trusts without asking
  list            every trusted file and whether it changed since
  untrust [PATH]  stop trusting a file, or every file under a folder (default: this folder)`

// maxShownLines is how much of a file hk projects trust prints before
// asking.
const maxShownLines = 200

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
		return projectsList(st, out)
	case "trust":
		fs := flag.NewFlagSet("projects trust", flag.ContinueOnError)
		fs.SetOutput(out)
		yes := fs.Bool("y", false, "trust without asking")
		if err := fs.Parse(args); err != nil {
			return err
		}
		return projectsTrust(home, st, pathArg(fs.Args()), *yes, in, out)
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

// projectFilesAt lists the instruction files a session working in path
// would read, or the one file path names.
func projectFilesAt(home, path string) ([]agent.ProjectFile, error) {
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
	cfg, err := config.Load(home)
	agentDir := filepath.Join(home, "agents", "habakkuk")
	if err == nil {
		if name, err := defaultAgent(cfg, ""); err == nil {
			agentDir = cfg.AgentDir(name)
		}
	}
	p := agent.LoadProjectFiles(agentDir, path, agent.ProjectFilesOn, nil)
	return p.Files, nil
}

func projectsTrust(home string, st *projtrust.Store, path string, yes bool, in io.Reader, out io.Writer) error {
	files, err := projectFilesAt(home, path)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Fprintf(out, "No project instruction files (%s) for %s.\n", strings.Join(agent.ProjectFileNames, ", "), path)
		return nil
	}
	if in == nil {
		in = strings.NewReader("") // nobody to ask: every answer is no
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
		if !yes {
			showFile(out, f.Path, sum, data)
			fmt.Fprint(out, "Agents will follow this exact text as instructions. Trust it? [y/N] ")
			line, _ := answers.ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
				fmt.Fprintf(out, "not trusted: %s\n", f.Path)
				continue
			}
		}
		e, err := st.Trust(real)
		if err != nil {
			return err
		}
		if e.SHA256 != sum {
			// It changed between showing and recording: trust what was read.
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

func showFile(out io.Writer, path, sum string, data []byte) {
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	fmt.Fprintf(out, "\n--- %s (%d bytes, sha256 %s)\n", path, len(data), sum[:12])
	for i, l := range lines {
		if i == maxShownLines {
			fmt.Fprintf(out, "... %d more lines: read the whole file before you trust it\n", len(lines)-maxShownLines)
			break
		}
		fmt.Fprintln(out, l)
	}
	fmt.Fprintln(out, "---")
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
		fmt.Fprintf(out, "%-28s %s  (sha256 %s, trusted %s)\n", status[i], e.Path, e.SHA256[:12], e.TrustedAt.Local().Format("2006-01-02 15:04"))
	}
	return nil
}
