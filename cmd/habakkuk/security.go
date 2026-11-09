package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/app"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/ops/security"
)

const securityUsage = `usage: habakkuk security [audit] [-fix] [-network]
  (no flags)  the effective tool policy of each agent, then the audit's findings
  -fix        apply the safe fixes (file and folder permissions)
  -network    list every outbound endpoint and whether this config turns it on`

// securityCmd runs `habakkuk security [audit] [-fix] [-network]`.
func securityCmd(home string, args []string, w io.Writer) error {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "audit":
			args = args[1:]
		case "help":
			fmt.Fprintln(w, securityUsage)
			return nil
		default:
			return errors.New(securityUsage)
		}
	}
	fs := flag.NewFlagSet("security", flag.ContinueOnError)
	fs.SetOutput(w)
	fix := fs.Bool("fix", false, "apply the safe fixes (file permissions)")
	network := fs.Bool("network", false, "list outbound endpoints and whether they are on")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts := security.Options{Home: home}
	if cwd, err := os.Getwd(); err == nil {
		opts.BackupDirs = append(opts.BackupDirs, cwd)
	}
	opts.Config, opts.ConfigErr = config.Load(home)
	if *network {
		if opts.Config == nil {
			return opts.ConfigErr
		}
		printNetwork(w, opts.Config)
		return nil
	}
	if opts.Config != nil {
		opts.Secret = app.SecretLookup(opts.Config, home, slog.New(slog.DiscardHandler))
		fmt.Fprintln(w, "Effective tool policy:")
		printPolicies(w, opts.Config)
		fmt.Fprintln(w)
	}
	findings := security.Audit(opts)
	if *fix {
		fixed := 0
		for _, f := range findings {
			if !f.Fixable() {
				continue
			}
			if err := f.Apply(); err != nil {
				fmt.Fprintf(w, "could not fix (%s): %v\n", f.Message, err)
				continue
			}
			fixed++
			fmt.Fprintf(w, "fixed: %s\n", f.Message)
		}
		if fixed > 0 {
			fmt.Fprintln(w)
			findings = security.Audit(opts)
		}
	}
	return printFindings(w, findings)
}

// permissionsDoctor is the doctor's file permission check: chats, memory,
// credentials and keys under $HABAKKUK_HOME must be owner-only. It returns
// the number of problems and points at habakkuk security -fix.
func permissionsDoctor(home string, cfg *config.Config, w io.Writer) int {
	n := 0
	for _, f := range security.Audit(security.Options{Home: home, Config: cfg}) {
		if f.Check != "permissions" || f.Severity < security.Medium {
			continue
		}
		n++
		fmt.Fprintf(w, "permissions: %s\n", f.Message)
	}
	if n == 0 {
		fmt.Fprintln(w, "permissions: ok (chats, memory and credentials are owner-only)")
	} else {
		fmt.Fprintln(w, "permissions: fix with habakkuk security -fix")
	}
	return n
}

func printPolicies(w io.Writer, c *config.Config) {
	for _, p := range security.Policies(c) {
		for _, line := range p.Lines() {
			fmt.Fprintln(w, "  "+line)
		}
	}
}

func printNetwork(w io.Writer, c *config.Config) {
	fmt.Fprintln(w, "Outbound endpoints ([on] = this config can contact it; nothing else is contacted):")
	for _, e := range security.Network(c) {
		fmt.Fprintln(w, "  "+e.String())
	}
}

func printFindings(w io.Writer, findings []security.Finding) error {
	if len(findings) == 0 {
		fmt.Fprintln(w, "no problems found")
		return nil
	}
	serious, fixable := 0, 0
	for _, f := range findings {
		fmt.Fprintf(w, "[%s] %s: %s\n", f.Severity, f.Check, f.Message)
		if f.Fix != "" {
			fmt.Fprintf(w, "    fix: %s\n", f.Fix)
		}
		if f.Severity >= security.High {
			serious++
		}
		if f.Fixable() {
			fixable++
		}
	}
	if fixable > 0 {
		fmt.Fprintf(w, "\n%d finding(s) can be fixed with: habakkuk security -fix\n", fixable)
	}
	if serious > 0 {
		return fmt.Errorf("%d high or critical finding(s)", serious)
	}
	return nil
}
