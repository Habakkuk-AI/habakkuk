package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
)

// memoryCmd lists an agent's memories, newest first, searches them, shows,
// forgets or vouches for one, and prints the user profile or the review
// log; its history subcommands (memoryhistory.go) list, diff, restore and
// pin. It opens only the memory index, not the whole app, so it is safe to
// run beside `habakkuk serve`. Search here is full text only; semantic search
// needs the embedding provider, which `habakkuk serve` and chats use.
func memoryCmd(ctx context.Context, home string, args []string) error {
	fs := flag.NewFlagSet("memory", flag.ExitOnError)
	agentName := fs.String("agent", "", "agent whose memory to show")
	limit := fs.Int("n", 50, "how many")
	fs.Parse(args)
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	name, err := defaultAgent(cfg, *agentName)
	if err != nil {
		return err
	}
	mem, err := memory.Open(ctx, home)
	if err != nil {
		return err
	}
	defer mem.Close()
	closeHistory, err := openMemoryHistory(ctx, home, mem)
	if err != nil {
		return err
	}
	defer closeHistory()
	ctx = cliActor(ctx)

	rest := fs.Args()
	usage := errors.New("usage: habakkuk memory [-agent A] [-n N] [search QUERY | show SLUG | forget SLUG | trust SLUG [yes|no] | profile | log]\n" + memoryHistoryUsage)
	if len(rest) == 0 {
		list, total, err := mem.Recent(ctx, name, *limit)
		if err != nil {
			return err
		}
		if total == 0 {
			fmt.Printf("agent %s has no memories yet (%s)\n", name, mem.Dir(name))
			return nil
		}
		for _, e := range list {
			fmt.Printf("%s  %-32s  %s%s%s\n", e.Updated.Format("2006-01-02"), e.Slug, e.Title, tagSuffix(e.Tags), untrustedSuffix(e.Untrusted))
		}
		if total > len(list) {
			fmt.Printf("(%d of %d shown)\n", len(list), total)
		}
		return nil
	}
	switch rest[0] {
	case "history", "diff", "restore", "pin", "unpin", "pins":
		return memoryHistoryCmd(ctx, cfg, mem, *agentName, name, rest)
	case "search":
		if len(rest) < 2 {
			return usage
		}
		hits, err := mem.Search(ctx, name, strings.Join(rest[1:], " "), nil, *limit)
		if err != nil {
			return err
		}
		if len(hits) == 0 {
			fmt.Println("no memories match")
		}
		for _, h := range hits {
			fmt.Printf("%s  %s%s%s\n    %s\n", h.Slug, h.Title, tagSuffix(h.Tags), untrustedSuffix(h.Untrusted), h.Snippet)
		}
	case "show":
		if len(rest) != 2 {
			return usage
		}
		m, err := mem.Read(ctx, name, rest[1])
		if err != nil {
			return err
		}
		fmt.Printf("%s\n\nfile: %s\n", memory.FormatNote(m), filepath.Join(mem.Dir(name), m.Slug+".md"))
		if m.Session != "" {
			fmt.Printf("written in session %s (habakkuk show %s)\n", m.Session, m.Session)
		}
	case "forget":
		if len(rest) != 2 {
			return usage
		}
		dst, err := mem.Forget(ctx, name, rest[1])
		if err != nil {
			return err
		}
		fmt.Printf("forgot %s; moved to %s\n", rest[1], dst)
	case "trust":
		if len(rest) < 2 || len(rest) > 3 {
			return usage
		}
		trust := memory.TrustTrusted
		if len(rest) == 3 {
			switch strings.ToLower(rest[2]) {
			case "yes", "on", "true", "trusted":
			case "no", "off", "false", "untrusted":
				trust = memory.TrustUntrusted
			default:
				return usage
			}
		}
		m, err := mem.SetTrust(ctx, name, rest[1], trust)
		if err != nil {
			return err
		}
		fmt.Printf("%s is now %s\n", m.Slug, m.Trust)
	case "profile":
		p, err := mem.Profile(name)
		if err != nil {
			return err
		}
		if p == "" {
			fmt.Printf("agent %s has no user profile yet (%s)\n", name, mem.ProfilePath(name))
			return nil
		}
		fmt.Println(p)
	case "log":
		data, err := os.ReadFile(filepath.Join(mem.Dir(name), ".review.log"))
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println("no background reviews yet (turn them on with memory: {review: true})")
			return nil
		}
		if err != nil {
			return err
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		fmt.Println(strings.Join(lines[max(0, len(lines)-*limit):], "\n"))
	default:
		return usage
	}
	return nil
}

func untrustedSuffix(untrusted bool) string {
	if untrusted {
		return "  (untrusted)"
	}
	return ""
}

func tagSuffix(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	return "  [" + strings.Join(tags, ", ") + "]"
}
