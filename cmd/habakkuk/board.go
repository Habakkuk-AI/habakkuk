package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/work/board"
)

// boardCmd shows and changes the shared task board. Cards added here are
// dispatched by `hk serve` within a few seconds.
func boardCmd(ctx context.Context, home string, args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	st := a.Board
	loc := a.Runner.Location
	if loc == nil {
		loc = time.Local
	}
	switch sub {
	case "list", "ls":
		fs := flag.NewFlagSet("board", flag.ExitOnError)
		status := fs.String("status", "", "open (default), todo, doing, blocked, done, cancelled or all")
		agentName := fs.String("agent", "", "only cards assigned to this agent")
		fs.Parse(args)
		f := board.Filter{Status: *status, Assignee: *agentName}
		if f.Status == "open" {
			f.Status = ""
		}
		list, err := st.List(ctx, f)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("no cards")
			return nil
		}
		fmt.Println(board.FormatList(list))
	case "show":
		id, err := cardID(flag.NewFlagSet("board show", flag.ExitOnError), args)
		if err != nil {
			return err
		}
		c, err := st.Get(ctx, id)
		if err != nil {
			return err
		}
		fmt.Println(board.FormatCard(ctx, st, c, loc))
	case "add":
		fs := flag.NewFlagSet("board add", flag.ExitOnError)
		agentName := fs.String("agent", "", "agent to do it")
		title := fs.String("title", "", "short title")
		body := fs.String("body", "", "the task in full")
		priority := fs.Int("priority", 0, "higher runs first")
		after := fs.String("after", "", "comma-separated card ids that must be done first")
		parent := fs.String("parent", "", "card id this one is part of")
		to := fs.String("to", "", "send the result to CHANNEL:CHAT (e.g. tg:123456)")
		fs.Parse(args)
		if *agentName == "" {
			return errors.New("usage: hk board add -agent NAME -title T [-body B]")
		}
		if _, ok := a.Cfg.Agents[*agentName]; !ok {
			return fmt.Errorf("unknown agent %q", *agentName)
		}
		c := board.Card{Title: *title, Body: *body, Assignee: *agentName, Priority: *priority, Parent: *parent,
			CreatedBy: board.User}
		for _, id := range strings.Split(*after, ",") {
			if id = strings.TrimSpace(id); id != "" {
				c.DependsOn = append(c.DependsOn, id)
			}
		}
		if *to != "" {
			acct, chat, ok := strings.Cut(*to, ":")
			if _, known := a.Cfg.Channels[acct]; !ok || !known {
				return fmt.Errorf("-to %q: want CHANNEL:CHAT with a configured channel", *to)
			}
			c.ReplyTo, _ = json.Marshal(channel.Address{Account: acct, Conversation: chat})
		}
		created, err := st.Create(ctx, c)
		if err != nil {
			return err
		}
		fmt.Printf("%s added for %s; `hk serve` starts it when its dependencies are done\n", created.ID, created.Assignee)
	case "done":
		fs := flag.NewFlagSet("board done", flag.ExitOnError)
		result := fs.String("result", "", "what was done, with proof")
		id, err := cardID(fs, args)
		if err != nil {
			return err
		}
		if _, err := st.Complete(ctx, id, board.User, *result); err != nil {
			return err
		}
		fmt.Println(id, "done")
	case "cancel":
		id, err := cardID(flag.NewFlagSet("board cancel", flag.ExitOnError), args)
		if err != nil {
			return err
		}
		if _, err := st.Cancel(ctx, id, board.User); err != nil {
			return err
		}
		fmt.Println(id, "cancelled")
	default:
		return fmt.Errorf("unknown board command %q", sub)
	}
	return nil
}

// cardID takes the card id from args, accepting flags before or after it.
func cardID(fs *flag.FlagSet, args []string) (string, error) {
	var id string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id = args[0]
		fs.Parse(args[1:])
	} else {
		fs.Parse(args)
		id = fs.Arg(0)
	}
	if id == "" {
		return "", fmt.Errorf("usage: habakkuk %s ID", fs.Name())
	}
	return id, nil
}
