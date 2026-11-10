package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/habakkuk-ai/habakkuk/internal/ops/logs"
)

// logsCmd prints habakkuk.log (and its rotated files) readably:
// hk logs [-f] [-level warn] [-since 1h] [-grep X] [-n 100].
func logsCmd(ctx context.Context, home string, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	follow := fs.Bool("f", false, "keep printing new records as they are written")
	level := fs.String("level", "", "lowest level to show: debug, info, warn or error")
	since := fs.String("since", "", "only records newer than this: a duration (1h, 30m) or a time (2026-11-07 or RFC 3339)")
	grep := fs.String("grep", "", "only records matching this (case-insensitive regular expression)")
	n := fs.Int("n", 100, "how many past records to print (0: all)")
	noColor := fs.Bool("no-color", false, "plain output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	lvl, err := logs.ParseLevel(*level)
	if err != nil {
		return fmt.Errorf("-level %q: want debug, info, warn or error", *level)
	}
	f := logs.Filter{MinLevel: lvl, Grep: logs.NewGrep(*grep)}
	if *since != "" {
		if f.Since, err = parseSince(*since, time.Now()); err != nil {
			return err
		}
	}
	color := !*noColor && os.Getenv("NO_COLOR") == "" && isTerminal(w)
	if len(logs.Files(home)) == 0 && !*follow {
		fmt.Fprintf(w, "no log yet at %s/%s\n", home, logs.FileName)
		return nil
	}
	if err := logs.Last(home, f, *n, w, color); err != nil {
		return err
	}
	if *follow {
		return logs.Follow(ctx, home, f, w, color, 0)
	}
	return nil
}

func parseSince(s string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	if strings.HasSuffix(s, "d") {
		if d, err := time.ParseDuration(strings.TrimSuffix(s, "d") + "h"); err == nil {
			return now.Add(-24 * d), nil
		}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("-since %q: want a duration like 1h or 2d, or a time like 2026-11-07 09:00", s)
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
