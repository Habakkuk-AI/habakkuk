package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/app"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/retro"
)

const retroUsage = `usage:
  habakkuk retro [-agent A] [-days N] [-dry-run]
  habakkuk retro list [-agent A]
  habakkuk retro show ID
  habakkuk retro apply ID N [N...]
  habakkuk retro reject ID N [N...]
ID may be latest: the newest retro of the default agent, or of -agent A`

// retroModels, when set, replaces the model a retro asks (tests).
var retroModels func(*app.App) retro.Sender

// retroCmd runs a retro now, or lists, shows, applies and rejects past
// ones.
func retroCmd(ctx context.Context, home string, args []string, w io.Writer) error {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("retro "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	agentFlag := fs.String("agent", "", "agent (default: the default agent)")
	days := fs.Int("days", 7, "how many days to look back over")
	dry := fs.Bool("dry-run", false, "print the evidence and the plan; store, change and send nothing")
	// Flags may come before or after the positional arguments.
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return errors.New(retroUsage)
		}
		if fs.NArg() == 0 {
			break
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
	switch sub {
	case "", "list", "show", "apply", "reject":
	default:
		return errors.New(retroUsage)
	}
	if *days < 1 || *days > 90 {
		return errors.New("-days must be between 1 and 90")
	}
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	if retroModels != nil {
		a.Retro.Models = retroModels(a)
	}
	name, err := defaultAgent(a.Cfg, *agentFlag)
	if err != nil {
		return err
	}
	r := a.Retro
	switch sub {
	case "":
		if len(pos) != 0 {
			return errors.New(retroUsage)
		}
		if *dry {
			return retroDryRun(ctx, a, name, *days, w)
		}
		fmt.Fprintf(w, "Looking back over %s's last %d days...\n", name, *days)
		ret, err := a.RunRetro(ctx, name, *days)
		if err != nil {
			return err
		}
		ret.Evidence = nil
		fmt.Fprint(w, "\n"+retro.Markdown(ret, a.Runner.Location))
		fmt.Fprintf(w, "\nReport: %s\n", ret.Report)
	case "list":
		if len(pos) != 0 {
			return errors.New(retroUsage)
		}
		agentName := ""
		if *agentFlag != "" {
			agentName = name
		}
		list, err := r.List(ctx, agentName, 50)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintln(w, "No retros yet. habakkuk retro runs one now; retro.every in config.yaml runs them on a timer.")
		}
		for _, ret := range list {
			state := ret.Summary
			if !ret.Done {
				state = "running"
			}
			fmt.Fprintf(w, "%s  %s to %s  %s\n", ret.ID, ret.From.Local().Format("2006-01-02"), ret.To.Local().Format("2006-01-02"), state)
		}
	case "show":
		if len(pos) != 1 {
			return errors.New(retroUsage)
		}
		ret, err := r.Resolve(ctx, name, pos[0])
		if err != nil {
			return err
		}
		fmt.Fprint(w, retro.Markdown(ret, a.Runner.Location))
	case "apply", "reject":
		if len(pos) < 2 {
			return errors.New(retroUsage)
		}
		ret, err := r.Resolve(ctx, name, pos[0])
		if err != nil {
			return err
		}
		var ns []int
		for _, p := range pos[1:] {
			n, err := strconv.Atoi(strings.Trim(p, "#,"))
			if err != nil {
				return fmt.Errorf("%q is not an action number", p)
			}
			ns = append(ns, n)
		}
		if sub == "reject" {
			for _, n := range ns {
				o, err := r.Reject(ctx, ret.ID, n)
				if err != nil {
					return err
				}
				fmt.Fprintf(w, "%d. %s %s: rejected\n", o.N, o.Kind, o.Target)
			}
			return nil
		}
		out, err := r.ApplyActions(ctx, ret.ID, ns, history.Actor{By: history.ByUser, Tool: "cli"})
		if err != nil {
			return err
		}
		failed := 0
		for _, o := range out {
			fmt.Fprintf(w, "%d. %s %s: %s\n", o.N, o.Kind, o.Target, o.Status)
			if o.Result != "" {
				fmt.Fprintf(w, "   %s\n", strings.ReplaceAll(o.Result, "\n", "\n   "))
			}
			if o.Status == retro.Failed {
				failed++
			}
		}
		if failed > 0 {
			return fmt.Errorf("%d of %d actions failed", failed, len(out))
		}
	}
	return nil
}

// retroDryRun prints the evidence and the model's plan without storing,
// changing or sending anything.
func retroDryRun(ctx context.Context, a *app.App, name string, days int, w io.Writer) error {
	ag, ok := a.Retro.Agents[name]
	if !ok {
		return fmt.Errorf("agent %s has no models for a retro", name)
	}
	from, to := retro.Recent(days, time.Now())
	ev, plan, err := a.Retro.Plan(ctx, ag, from, to)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "# Evidence\n\n%s\n", ev.Text())
	ret := &retro.Retro{ID: retro.DryRunID, Agent: name, From: from, To: to, Plan: *plan}
	for i, act := range plan.Actions {
		ret.Actions = append(ret.Actions, retro.ActionState{N: i + 1, Action: act, Status: retro.Proposed})
	}
	fmt.Fprintf(w, "# Plan\n\n%s\nDry run: nothing was stored, changed or sent.\n", retro.Markdown(ret, a.Runner.Location))
	return nil
}
