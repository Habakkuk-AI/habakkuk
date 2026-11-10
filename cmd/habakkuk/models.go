package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm/auth"
	"github.com/habakkuk-ai/habakkuk/internal/llm/budget"
	usagepkg "github.com/habakkuk-ai/habakkuk/internal/llm/usage"
)

// authCmd manages subscription logins. It never needs the config, so a
// login can come before the provider that uses it.
func authCmd(ctx context.Context, home string, args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	st := auth.NewStore(home)
	switch sub {
	case "login":
		if len(args) != 1 {
			return fmt.Errorf("usage: hk auth login %s", strings.Join(auth.Kinds, "|"))
		}
		kind := strings.ToLower(args[0])
		if kind == "claude-token" || kind == "claude" {
			return errors.New("claude-token is not supported: Anthropic's terms do not allow third-party apps to use Claude subscription credentials; use an API key (ANTHROPIC_API_KEY)")
		}
		ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
		defer cancel()
		e, err := st.Login(ctx, kind, kind, func(d auth.DeviceCode) {
			fmt.Printf("Open %s and enter the code:\n\n    %s\n\nWaiting for you to finish signing in (the code expires in %s)...\n",
				d.URL, d.Code, d.ExpiresIn.Round(time.Minute))
		})
		if err != nil {
			return err
		}
		who := ""
		if e.Email != "" {
			who = " as " + e.Email
		}
		fmt.Printf("Logged in to %s%s. Use it with a provider like:\n\n  providers:\n    %s:\n      auth: %s\n", kind, who, kind, kind)
	case "list", "ls":
		list, err := st.List()
		if err != nil {
			return err
		}
		names, _ := st.Names()
		if len(names) == 0 {
			fmt.Printf("no logins; run hk auth login %s\n", strings.Join(auth.Kinds, "|"))
			return nil
		}
		for _, n := range names {
			e := list[n]
			line := fmt.Sprintf("%-10s logged in %s", n, e.UpdatedAt.Local().Format("2006-01-02 15:04"))
			if e.Email != "" {
				line += "  " + e.Email
			}
			if !e.ExpiresAt.IsZero() {
				line += "  token until " + e.ExpiresAt.Local().Format("2006-01-02 15:04") + " (refreshed automatically)"
			}
			fmt.Println(line)
		}
	case "logout", "rm":
		if len(args) != 1 {
			return errors.New("usage: hk auth logout NAME")
		}
		found, err := st.Delete(ctx, args[0])
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("no login named %q", args[0])
		}
		fmt.Printf("Removed the %s login from %s.\n", args[0], st.Path)
	default:
		return fmt.Errorf("unknown auth command %q (login, list, logout)", sub)
	}
	return nil
}

func usageCmd(ctx context.Context, home string, args []string) error {
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	days := fs.Int("days", 30, "how many days back")
	agentName := fs.String("agent", "", "only this agent")
	session := fs.String("session", "", "only this session")
	fs.Parse(args)
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	since := time.Now().AddDate(0, 0, -*days)
	calls, err := a.Usage.Calls(ctx, usagepkg.Filter{Since: since, Agent: *agentName, Session: *session})
	if err != nil {
		return err
	}
	fmt.Printf("Model usage since %s", since.Format("2006-01-02"))
	if *agentName != "" {
		fmt.Printf(", agent %s", *agentName)
	}
	fmt.Print("\n\n")
	usagepkg.Summarize(calls, a.Pricer).Write(os.Stdout)
	if *session == "" {
		rep := a.BudgetReport()
		if *agentName != "" {
			var kept []budget.Scope
			for _, s := range rep.Agents {
				if s.Agent == *agentName {
					kept = append(kept, s)
				}
			}
			rep.Agents = kept
		}
		fmt.Println()
		rep.Write(os.Stdout)
	}
	return nil
}

func modelsCmd(ctx context.Context, home string) error {
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	for _, pm := range a.ListModels(ctx) {
		head := fmt.Sprintf("%s (%s", pm.Name, pm.Type)
		if pm.BaseURL != "" {
			head += ", " + pm.BaseURL
		}
		fmt.Printf("%s; %s)\n", head, pm.Credentials)
		switch {
		case pm.Unsupported:
			fmt.Println("  this API has no model listing")
		case pm.Err != nil:
			fmt.Printf("  could not list models: %v\n", pm.Err)
		case len(pm.Models) == 0:
			fmt.Println("  no models listed")
		default:
			for _, m := range pm.Models {
				fmt.Printf("  %s/%s\n", pm.Name, m)
			}
		}
	}
	fmt.Printf("\nPresets: %s\n", strings.Join(config.PresetNames(), ", "))
	return nil
}
