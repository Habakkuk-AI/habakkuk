package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// sessionVerbs are the `habakkuk sessions` subcommands handled here.
var sessionVerbs = map[string]bool{"export": true, "rm": true, "delete": true, "archive": true,
	"unarchive": true, "archived": true, "search": true}

// sessionCommand runs `habakkuk sessions export|rm|archive|unarchive|archived|search`.
// It reports false for anything else, which `sessions` handles itself.
func sessionCommand(ctx context.Context, home string, args []string) (bool, error) {
	if len(args) == 0 || !sessionVerbs[args[0]] {
		return false, nil
	}
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return true, err
	}
	defer closeApp()
	return true, runSessionVerb(ctx, a.Sessions, args, os.Stdout, os.Stdin)
}

// sessionFlags splits args into flags (which may come after positional
// arguments, as in "export ID -format html") and the rest.
func sessionFlags(args []string, valued ...string) (map[string]string, []string, error) {
	flags := map[string]string{}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			rest = append(rest, a)
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		takes := false
		for _, v := range valued {
			takes = takes || v == name
		}
		switch {
		case takes && !hasVal:
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("-%s needs a value", name)
			}
			i++
			val = args[i]
		case !takes && !hasVal:
			val = "true"
		}
		flags[name] = val
	}
	return flags, rest, nil
}

func runSessionVerb(ctx context.Context, s *session.Store, args []string, out io.Writer, in io.Reader) error {
	verb := args[0]
	flags, rest, err := sessionFlags(args[1:], "format", "o", "agent", "n")
	if err != nil {
		return err
	}
	limit := 20
	if n := flags["n"]; n != "" {
		if limit, err = strconv.Atoi(n); err != nil || limit < 1 {
			return errors.New("-n takes a positive number")
		}
	}
	switch verb {
	case "export":
		if len(rest) != 1 {
			return errors.New("usage: habakkuk sessions export ID [-format md|json|html] [-o FILE]")
		}
		sess, err := s.Get(ctx, rest[0])
		if err != nil {
			return err
		}
		recs, err := s.Messages(ctx, sess.ID)
		if err != nil {
			return err
		}
		data, err := session.Export(sess, recs, flags["format"], time.Now())
		if err != nil {
			return err
		}
		if path := flags["o"]; path != "" && path != "-" {
			if err := store.WriteFileAtomic(path, data, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "wrote %s (%d messages)\n", path, len(recs))
			return nil
		}
		_, err = out.Write(data)
		return err
	case "rm", "delete":
		if len(rest) == 0 {
			return errors.New("usage: habakkuk sessions rm [-y] ID [ID ...]")
		}
		for _, id := range rest {
			sess, err := s.Get(ctx, id)
			if err != nil {
				return err
			}
			recs, _ := s.Messages(ctx, id)
			if flags["y"] != "true" {
				fmt.Fprintf(out, "Delete session %s (%s, %d messages, %q)? It cannot be undone; archive keeps it instead. [y/N] ",
					id, sess.Agent, len(recs), sess.Title)
				line, _ := bufio.NewReader(in).ReadString('\n')
				if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
					fmt.Fprintln(out, "Kept.")
					continue
				}
			}
			if err := s.Delete(ctx, id); err != nil {
				return err
			}
			fmt.Fprintf(out, "Deleted %s.\n", id)
		}
		return nil
	case "archive", "unarchive":
		if len(rest) == 0 {
			return fmt.Errorf("usage: habakkuk sessions %s ID [ID ...]", verb)
		}
		for _, id := range rest {
			if err := s.SetArchived(ctx, id, verb == "archive"); err != nil {
				return err
			}
			fmt.Fprintf(out, "%sd %s.\n", strings.ToUpper(verb[:1])+verb[1:], id)
		}
		return nil
	case "archived":
		list, err := s.ListArchived(ctx, flags["agent"], limit)
		if err != nil {
			return err
		}
		for _, x := range list {
			fmt.Fprintf(out, "%s  %-10s  %s  %s\n", x.ID, x.Agent, x.UpdatedAt.Format("2006-01-02 15:04"), x.Title)
		}
		return nil
	case "search":
		query := strings.Join(rest, " ")
		if strings.TrimSpace(query) == "" {
			return errors.New("usage: habakkuk sessions search [-agent A] [-n N] QUERY")
		}
		hits, err := s.Search(ctx, flags["agent"], query, "", limit)
		if err != nil {
			return err
		}
		if len(hits) == 0 {
			fmt.Fprintln(out, "no matches")
		}
		for _, h := range hits {
			fmt.Fprintf(out, "%s #%d  %-10s  %s  %s\n    %s: %s\n", h.Session, h.Seq, h.Agent, h.At.Format("2006-01-02"), h.Title, h.Role, h.Snippet)
		}
		return nil
	}
	return fmt.Errorf("unknown sessions command %q", verb)
}
