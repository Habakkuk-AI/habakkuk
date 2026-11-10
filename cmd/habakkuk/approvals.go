package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

const approvalsUsage = `usage: hk approvals [list] [-agent A]
       hk approvals rm [-agent A] ID
  list   each agent's approval rules: calls it makes without asking, from
         agents.<name>.allow in config.yaml and from "Always allow" answers
  rm     remove a saved rule; a config rule is removed by editing config.yaml`

// approvalsCmd runs `hk approvals [list|rm]`.
func approvalsCmd(home string, args []string, w io.Writer) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("approvals", flag.ContinueOnError)
	fs.SetOutput(w)
	agentName := fs.String("agent", "", "only this agent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	names := cfg.AgentNames()
	if *agentName != "" {
		if cfg.Agents[*agentName] == nil {
			return fmt.Errorf("unknown agent %q", *agentName)
		}
		names = []string{*agentName}
	}
	switch sub {
	case "list":
		return listApprovals(w, cfg, names)
	case "rm":
		if fs.NArg() != 1 {
			return errors.New(approvalsUsage)
		}
		return removeApproval(w, cfg, names, fs.Arg(0))
	case "help":
		fmt.Fprintln(w, approvalsUsage)
		return nil
	}
	return errors.New(approvalsUsage)
}

func ruleFile(cfg *config.Config, agent string) *tools.RuleFile {
	return &tools.RuleFile{Path: filepath.Join(cfg.AgentDir(agent), tools.RulesFileName), Config: cfg.AllowRules(agent)}
}

func listApprovals(w io.Writer, cfg *config.Config, names []string) error {
	any := false
	for _, name := range names {
		rf := ruleFile(cfg, name)
		if _, err := rf.Saved(); err != nil {
			fmt.Fprintf(w, "%s: cannot read the saved rules (%v); they cover nothing until fixed\n", name, err)
		}
		rules := rf.Rules()
		if len(rules) == 0 {
			continue
		}
		any = true
		fmt.Fprintf(w, "%s:\n", name)
		for _, r := range rules {
			added := ""
			if !r.Added.IsZero() {
				added = r.Added.Local().Format("2006-01-02")
			}
			source := r.Source
			if source == "" {
				source = "saved"
			}
			fmt.Fprintf(w, "  %-11s %-7s %-10s %s\n", r.ID, source, added, r)
			where := "applies " + r.Where()
			if r.SavedBy != "" {
				where += ", saved by " + r.SavedBy
			}
			fmt.Fprintf(w, "  %-11s %s\n", "", where)
		}
	}
	if !any {
		fmt.Fprintln(w, `No approval rules. Answer "Always allow" in a chat, or add agents.<name>.allow to config.yaml.`)
		return nil
	}
	fmt.Fprintln(w, "\nRules never cover a turn that read outside content (or that followed one with no new message\nfrom you), a guest's turn, an inbound A2A task, a write to an agent's instruction files, or a\ncommand on the hardline blocklist. A rule saved in a chat applies in that chat only.")
	return nil
}

func removeApproval(w io.Writer, cfg *config.Config, names []string, id string) error {
	for _, name := range names {
		rf := ruleFile(cfg, name)
		for _, r := range rf.Rules() {
			if r.ID != id || r.Source != "config" {
				continue
			}
			return fmt.Errorf("%s comes from config.yaml (agents.%s.allow, entry %s); remove it there",
				id, name, strings.TrimPrefix(id, "config-"))
		}
		ok, err := rf.Remove(id)
		if err != nil {
			return err
		}
		if ok {
			fmt.Fprintf(w, "Removed %s from %s. It asks again from now on.\n", id, name)
			return nil
		}
	}
	return fmt.Errorf("no saved rule %s; hk approvals list shows them", id)
}
