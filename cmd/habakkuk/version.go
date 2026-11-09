package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"

	"github.com/habakkuk-ai/habakkuk/internal/app"
	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// Set at build time by the release workflow:
//
//	-X main.version=v1.2.3 -X main.commit=<sha> -X main.date=<RFC 3339>
//
// A plain `go build` falls back to the VCS stamp Go embeds.
var (
	commit = ""
	date   = ""
)

func buildInfo() (rev, built string) {
	rev, built = commit, date
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch {
			case s.Key == "vcs.revision" && rev == "":
				rev = s.Value
			case s.Key == "vcs.time" && built == "":
				built = s.Value
			case s.Key == "vcs.modified" && s.Value == "true" && commit == "" && rev != "":
				rev += "-dirty"
			}
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev == "" {
		rev = "unknown"
	}
	if built == "" {
		built = "unknown"
	}
	return rev, built
}

// versionCmd prints the version, build and the database schema versions
// this binary knows next to the ones in $HABAKKUK_HOME.
func versionCmd(ctx context.Context, home string, out io.Writer) error {
	rev, built := buildInfo()
	fmt.Fprintf(out, "habakkuk %s\ncommit:  %s\nbuilt:   %s\ngo:      %s %s/%s\n", version, rev, built, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(out, "config:  format version %d", config.CurrentVersion)
	if data, err := os.ReadFile(filepath.Join(home, "config.yaml")); err == nil {
		if v, err := config.FileVersion(data); err == nil {
			fmt.Fprintf(out, " (%s has version %d)", filepath.Join(home, "config.yaml"), v)
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "schemas (this binary / %s):\n", home)
	for _, s := range app.CheckSchemas(ctx, home) {
		onDisk := "not created"
		switch {
		case s.Err != nil:
			onDisk = "unreadable: " + s.Err.Error()
		case s.Exists:
			onDisk = fmt.Sprint(s.OnDisk)
		}
		note := ""
		switch {
		case s.NeedsNewer:
			note = "  <- written by a newer Habakkuk"
		case s.WillMigrate:
			note = "  (migrates on next start)"
		}
		fmt.Fprintf(out, "  %-15s %3d / %s%s\n", s.File, s.Known, onDisk, note)
	}
	return nil
}

func doctorCmd(ctx context.Context, home string, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	upgrade := fs.Bool("upgrade-check", false, "only check that this binary can take over $HABAKKUK_HOME; changes nothing")
	fs.Parse(args)
	if *upgrade {
		return upgradeCheck(ctx, home, os.Stdout)
	}
	return doctor(ctx, home)
}

// errUpgradeCheck is returned when upgradeCheck finds a blocker.
var errUpgradeCheck = errors.New("upgrade check failed")

// upgradeCheck is what `habakkuk update` asks a new binary before
// installing it: can it load this home's config, and is every database at
// a schema it knows (older is fine: it migrates forward on start)? It only
// reads, so a release that would fail leaves no trace.
func upgradeCheck(ctx context.Context, home string, out io.Writer) error {
	fmt.Fprintf(out, "habakkuk %s upgrade check on %s\n", version, home)
	failed := 0
	if _, err := os.Stat(filepath.Join(home, "config.yaml")); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(out, "config: none yet (fresh install)")
	} else {
		cfg, err := config.Load(home)
		if err != nil {
			fmt.Fprintf(out, "config: FAIL %v\n", err)
			failed++
		} else {
			fmt.Fprintf(out, "config: ok (format version %d)\n", config.CurrentVersion)
			if s := cfg.Report.Summary(); s != "" {
				fmt.Fprintln(out, "config:", s)
			}
			for _, k := range cfg.Report.Unknown {
				fmt.Fprintf(out, "config: warning: unknown key %s will be ignored\n", k)
			}
		}
	}
	for _, s := range app.CheckSchemas(ctx, home) {
		switch {
		case s.Err != nil:
			fmt.Fprintf(out, "%s: FAIL %v\n", s.File, s.Err)
			failed++
		case s.NeedsNewer:
			fmt.Fprintf(out, "%s: FAIL schema version %d is newer than this release knows (%d)\n", s.File, s.OnDisk, s.Known)
			failed++
		case s.WillMigrate:
			fmt.Fprintf(out, "%s: ok, will migrate from schema %d to %d on first start\n", s.File, s.OnDisk, s.Known)
		case s.Exists:
			fmt.Fprintf(out, "%s: ok (schema %d)\n", s.File, s.OnDisk)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%w: %d problem(s)", errUpgradeCheck, failed)
	}
	fmt.Fprintln(out, "upgrade check passed")
	return nil
}

// reportConfig tells the user, on stderr, what loading config.yaml had to
// do: an in-memory migration or ignored keys.
func reportConfig(w io.Writer, cfg *config.Config) {
	if cfg == nil || cfg.Report == nil {
		return
	}
	if s := cfg.Report.Summary(); s != "" {
		fmt.Fprintln(w, "habakkuk:", s)
	}
	for _, k := range cfg.Report.Unknown {
		fmt.Fprintf(w, "habakkuk: config.yaml: unknown key %s is ignored (a typo, or a setting for a newer release; habakkuk doctor lists them)\n", k)
	}
}
