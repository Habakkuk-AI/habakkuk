package main

import (
	"fmt"
	"io"
	"slices"
	"sort"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/tools/computer"
)

// computerCmd: hk computer view.
func computerCmd(home string, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "view" {
		return fmt.Errorf("usage: hk computer view")
	}
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, computer.View(cfg.ComputerConfig(), home))
	return err
}

// computerDoctor prints what the computer backend needs when an agent uses
// it, and returns the number of problems.
func computerDoctor(cfg *config.Config, out io.Writer) int {
	var agents []string
	for name, ac := range cfg.Agents {
		if slices.Contains(ac.Toolsets, computer.Toolset) {
			agents = append(agents, name)
		}
	}
	if len(agents) == 0 {
		return 0
	}
	sort.Strings(agents)
	k := cfg.ComputerConfig()
	problems := 0
	for _, c := range computer.Doctor(k) {
		if !c.OK {
			problems++
		}
		fmt.Fprintf(out, "computer (%s) %s: %s\n", k.Backend, c.What, c.Detail)
	}
	fmt.Fprintf(out, "computer: used by %v, at most %d actions per turn\n", agents, k.MaxActions)
	return problems
}
