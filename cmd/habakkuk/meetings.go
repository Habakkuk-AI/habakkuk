package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/tools/meeting"
)

// meetingsCmd lists, shows and ends meeting attendances. It reads the files
// under $HABAKKUK_HOME/meetings directly; `leave` leaves a request there that
// the attendance (run by habakkuk serve) picks up within seconds.
func meetingsCmd(_ context.Context, home string, args []string, out io.Writer) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	st := meeting.NewStore(filepath.Join(home, "meetings"))
	loc := time.Local
	if cfg, err := config.Load(home); err == nil && cfg.Timezone != "" {
		if l, err := time.LoadLocation(cfg.Timezone); err == nil {
			loc = l
		}
	}
	switch sub {
	case "list", "ls":
		list, err := st.List()
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintln(out, "no meetings")
			return nil
		}
		for _, r := range list {
			fmt.Fprintln(out, meeting.Summary(r, loc))
		}
	case "show":
		fs := flag.NewFlagSet("meetings show", flag.ExitOnError)
		transcript := fs.Bool("transcript", false, "print the whole transcript too")
		fs.Parse(args)
		if fs.NArg() != 1 {
			return errors.New("usage: habakkuk meetings show [-transcript] ID")
		}
		r, err := st.Get(fs.Arg(0))
		if err != nil {
			return err
		}
		fmt.Fprintln(out, meeting.Describe(r, loc))
		if notes, err := st.Notes(r.ID); err == nil && notes != "" {
			fmt.Fprintln(out, "\n"+notes)
		}
		lines, err := st.Lines(r.ID)
		if err != nil {
			return err
		}
		switch {
		case *transcript && len(lines) > 0:
			fmt.Fprintln(out, "\nTranscript:\n"+meeting.FormatTranscript(lines, loc))
		case len(lines) > 0:
			fmt.Fprintf(out, "\n%d caption lines; habakkuk meetings show -transcript %s prints them.\n", len(lines), r.ID)
		}
	case "leave":
		if len(args) != 1 {
			return errors.New("usage: habakkuk meetings leave ID")
		}
		r, err := st.Get(args[0])
		if err != nil {
			return err
		}
		if r.Status.Finished() {
			fmt.Fprintf(out, "%s is already over (%s).\n", r.ID, r.Status)
			return nil
		}
		if err := st.RequestLeave(r.ID); err != nil {
			return err
		}
		fmt.Fprintf(out, "Asked %s to leave; habakkuk serve ends it within seconds and sends the notes.\n", r.ID)
	default:
		return fmt.Errorf("unknown meetings command %q (list, show ID, leave ID)", sub)
	}
	return nil
}
