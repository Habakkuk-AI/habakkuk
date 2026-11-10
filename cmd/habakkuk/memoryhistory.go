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
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/skills"
)

const memoryHistoryUsage = `usage:
  hk memory history [-agent A] [-file NAME] [-since 7d] [-n N]
  hk memory diff ID
  hk memory restore ID [-after]
  hk memory pin NOTE | skill:NAME | profile [-agent A]
  hk memory unpin NOTE | skill:NAME | profile [-agent A]
  hk memory pins [-agent A]`

// cliActor marks changes made from the command line as the user's.
func cliActor(ctx context.Context) context.Context {
	return history.WithActor(ctx, history.Actor{By: history.ByUser, Tool: "cli"})
}

// openMemoryHistory attaches history.db to a memory store opened by the
// CLI, so its changes are recorded like the server's.
func openMemoryHistory(ctx context.Context, home string, mem *memory.Store) (func(), error) {
	h, err := history.Open(ctx, home)
	if err != nil {
		return nil, err
	}
	mem.UseHistory(h)
	return h.Close, nil
}

// memoryHistoryCmd runs the history subcommands of `hk memory`.
// agentName is the agent from the flags before the subcommand ("" when
// none was given) and def the default agent.
func memoryHistoryCmd(ctx context.Context, cfg *config.Config, mem *memory.Store, agentName, def string, args []string) error {
	h := mem.History()
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("memory "+sub, flag.ContinueOnError)
	agentFlag := fs.String("agent", agentName, "agent")
	file := fs.String("file", "", "only changes to this note, USER.md, IDENTITY.md or skill")
	since := fs.String("since", "", "only changes this recent: 7d, 12h, or a date")
	limit := fs.Int("n", 50, "how many")
	after := fs.Bool("after", false, "restore: put back the content after the change (redo) rather than before (undo)")
	// Flags may come before or after the positional argument.
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return errors.New(memoryHistoryUsage)
		}
		if fs.NArg() == 0 {
			break
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
	agents := cfg.AgentNames()
	if *agentFlag != "" {
		name, err := defaultAgent(cfg, *agentFlag)
		if err != nil {
			return err
		}
		agents = []string{name}
	}
	for _, a := range agents {
		if err := h.Scan(ctx, a); err != nil {
			return err
		}
	}
	switch sub {
	case "history":
		if len(pos) != 0 {
			return errors.New(memoryHistoryUsage)
		}
		from, err := history.ParseSince(*since, time.Now())
		if err != nil {
			return err
		}
		f := history.Filter{File: *file, Since: from, Limit: *limit}
		if *agentFlag != "" {
			f.Agent = agents[0]
		}
		cs, err := h.List(ctx, f)
		if err != nil {
			return err
		}
		if len(cs) == 0 {
			fmt.Println("no memory changes recorded")
			return nil
		}
		for _, c := range cs {
			line := c.Describe()
			if f.Agent == "" {
				line = c.Agent + "  " + line
			}
			fmt.Println(line)
		}
	case "diff":
		if len(pos) != 1 {
			return errors.New(memoryHistoryUsage)
		}
		c, err := h.Get(ctx, pos[0])
		if err != nil {
			return err
		}
		d, err := h.Diff(ctx, c)
		if err != nil {
			return err
		}
		fmt.Println(c.Describe())
		if d == "" {
			fmt.Println("(no difference in content)")
			return nil
		}
		fmt.Print(d)
	case "restore":
		if len(pos) != 1 {
			return errors.New(memoryHistoryUsage)
		}
		version := history.Before
		if *after {
			version = history.After
		}
		c, r, err := mem.Restore(cliActor(ctx), "", pos[0], version)
		if err != nil {
			return err
		}
		if r == nil {
			fmt.Printf("%s already holds the version %s change %s\n", c.Path, version, c.ID)
			return nil
		}
		fmt.Printf("restored %s to the version %s change %s; recorded as change %s (hk memory restore %s undoes it)\n",
			c.Path, version, c.ID, r.ID, r.ID)
	case "pin", "unpin":
		if len(pos) != 1 {
			return errors.New(memoryHistoryUsage)
		}
		name := def
		if *agentFlag != "" {
			name = agents[0]
		}
		kind, target, err := pinTarget(ctx, cfg, mem, name, pos[0])
		if err != nil {
			return err
		}
		if sub == "pin" {
			if err := h.Pin(ctx, name, kind, target); err != nil {
				return err
			}
			fmt.Printf("pinned %s %s of agent %s: the background review and the skills curator leave it alone\n", kind, target, name)
			return nil
		}
		ok, err := h.Unpin(ctx, name, kind, target)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Printf("%s %s of agent %s was not pinned\n", kind, target, name)
			return nil
		}
		fmt.Printf("unpinned %s %s of agent %s\n", kind, target, name)
	case "pins":
		for _, a := range agents {
			pins, err := h.Pins(ctx, a)
			if err != nil {
				return err
			}
			for _, p := range pins {
				fmt.Printf("%s  %s %s\n", a, p.Kind, p.Name)
			}
		}
	default:
		return errors.New(memoryHistoryUsage)
	}
	return nil
}

// pinTarget reads what `memory pin` names: a note slug, skill:NAME, or
// profile (USER.md). Notes and skills must exist.
func pinTarget(ctx context.Context, cfg *config.Config, mem *memory.Store, agentName, arg string) (kind, name string, err error) {
	switch lower := strings.ToLower(arg); {
	case lower == "profile" || lower == "user.md":
		return history.KindProfile, history.ProfileFile, nil
	case strings.HasPrefix(lower, "skill:"):
		name = strings.TrimPrefix(lower, "skill:")
		if err := skills.ValidName(name); err != nil {
			return "", "", err
		}
		if _, err := os.Stat(history.SkillTarget(cfg.Home, agentName, name).Path); err != nil {
			return "", "", fmt.Errorf("agent %s has no skill %q of its own", agentName, name)
		}
		return history.KindSkill, name, nil
	}
	if _, err := mem.Read(ctx, agentName, arg); err != nil {
		return "", "", err
	}
	return history.KindNote, arg, nil
}
