package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/app"
	plat "github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	usagepkg "github.com/habakkuk-ai/habakkuk/internal/llm/usage"
	"github.com/habakkuk-ai/habakkuk/internal/ops/logs"
)

// heartbeatFile is where `habakkuk serve` says it is alive.
const heartbeatFile = "serve.json"

// heartbeatEvery is how often serve rewrites it; status calls a beat older
// than three of these stale.
const heartbeatEvery = 15 * time.Second

type heartbeat struct {
	PID     int       `json:"pid"`
	Version string    `json:"version"`
	Started time.Time `json:"started"`
	Beat    time.Time `json:"beat"`
	Listen  string    `json:"listen,omitempty"`
}

// startHeartbeat labels telemetry with the version and keeps serve.json
// fresh until the returned func runs, which removes it.
func startHeartbeat(ctx context.Context, home string, a *app.App, listen string) func() {
	a.Telemetry.SetVersion(version)
	path := filepath.Join(home, heartbeatFile)
	hb := heartbeat{PID: os.Getpid(), Version: version, Started: time.Now(), Listen: listen}
	write := func() {
		hb.Beat = time.Now()
		data, _ := json.Marshal(hb)
		if err := store.WriteFileAtomic(path, data, 0o600); err != nil {
			a.Log.Warn("write heartbeat failed", "err", err)
		}
	}
	write()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(heartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				write()
			}
		}
	}()
	return func() {
		cancel()
		<-done
		os.Remove(path)
	}
}

func readHeartbeat(home string) (heartbeat, error) {
	var hb heartbeat
	data, err := os.ReadFile(filepath.Join(home, heartbeatFile))
	if err != nil {
		return hb, err
	}
	return hb, json.Unmarshal(data, &hb)
}

// serveState describes the serving process from its heartbeat.
func serveState(home string, now time.Time) string {
	hb, err := readHeartbeat(home)
	if errors.Is(err, os.ErrNotExist) {
		return "not running (start it with habakkuk serve)"
	}
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	alive := plat.Alive(hb.PID)

	age := now.Sub(hb.Beat)
	switch {
	case !alive:
		return fmt.Sprintf("not running (pid %d exited; last seen %s)", hb.PID, hb.Beat.Local().Format("2006-01-02 15:04"))
	case age > 3*heartbeatEvery:
		return fmt.Sprintf("stuck? pid %d is alive but its last heartbeat was %s ago", hb.PID, roundDur(age))
	}
	s := fmt.Sprintf("running, pid %d, version %s, up %s", hb.PID, hb.Version, roundDur(now.Sub(hb.Started)))
	if hb.Listen != "" {
		s += ", http on " + hb.Listen
	}
	return s
}

func roundDur(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return d.Truncate(time.Minute).String()
	}
	return d.Truncate(time.Second).String()
}

// statusCmd summarises Habakkuk: version, the serving process, channels,
// the queue, recent errors and today's cost.
func statusCmd(ctx context.Context, home string, w io.Writer) error {
	now := time.Now()
	fmt.Fprintf(w, "habakkuk %s (home %s)\n", version, home)
	fmt.Fprintf(w, "serve:    %s\n", serveState(home, now))
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()

	if a.Gateway != nil {
		sts, err := a.Gateway.Store.Statuses(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, "channels:")
		seen := map[string]bool{}
		for _, st := range sts {
			ch := a.Cfg.Channels[st.Account]
			if ch == nil {
				continue
			}
			seen[st.Account] = true
			line := fmt.Sprintf("  %-12s %-11s %-10s since %s", st.Account, ch.Type, st.State, st.Since.Local().Format("01-02 15:04"))
			if st.Restarts > 0 {
				line += fmt.Sprintf("  restarts=%d", st.Restarts)
			}
			fmt.Fprintln(w, line)
		}
		for _, name := range channelNames(a) {
			if !seen[name] {
				fmt.Fprintf(w, "  %-12s %-11s never started\n", name, a.Cfg.Channels[name].Type)
			}
		}
	}

	depth, err := a.Queue.DepthByKind(ctx)
	if err != nil {
		return err
	}
	if len(depth) == 0 {
		fmt.Fprintln(w, "queue:    empty")
	} else {
		var parts []string
		for _, d := range depth {
			parts = append(parts, fmt.Sprintf("%s %d pending/%d running", d.Kind, d.Pending, d.Running))
		}
		fmt.Fprintf(w, "queue:    %s\n", strings.Join(parts, "; "))
	}

	fmt.Fprintln(w, "policy (habakkuk security for the audit):")
	printPolicies(w, a.Cfg)

	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	calls, err := a.Usage.Calls(ctx, usagepkg.Filter{Since: midnight})
	if err != nil {
		return err
	}
	if len(calls) == 0 {
		fmt.Fprintln(w, "today:    no model calls")
	} else {
		fmt.Fprintf(w, "today:    %s\n", usagepkg.Summarize(calls, a.Pricer).Brief())
	}

	errs := recentErrors(ctx, a, home, now.Add(-24*time.Hour), 5)
	if len(errs) == 0 {
		fmt.Fprintln(w, "errors:   none in the last 24h")
	} else {
		fmt.Fprintln(w, "errors (last 24h, newest first):")
		for _, e := range errs {
			fmt.Fprintln(w, "  "+e)
		}
	}
	return nil
}

// recentErrors merges failed jobs and ERROR log records since a time,
// newest first.
func recentErrors(ctx context.Context, a *app.App, home string, since time.Time, n int) []string {
	type item struct {
		at   time.Time
		text string
	}
	var items []item
	if fails, err := a.Queue.RecentFailures(ctx, since, n); err == nil {
		for _, f := range fails {
			items = append(items, item{f.At, fmt.Sprintf("%s job %s (%s, attempt %d/%d, %s): %s",
				f.At.Local().Format("15:04"), f.ID, f.Kind, f.Attempts, f.MaxAttempts, f.Status, oneLine(f.LastError, 160))})
		}
	}
	var recs []logs.Record
	_ = logs.Scan(home, func(r logs.Record) {
		if r.Level >= slog.LevelError && r.Time.After(since) {
			recs = append(recs, r)
			if len(recs) > 4*n {
				recs = recs[len(recs)-n:]
			}
		}
	})
	if len(recs) > n {
		recs = recs[len(recs)-n:]
	}
	for _, r := range recs {
		at := r.Time
		r.Time = time.Time{} // printed in front instead
		items = append(items, item{at, at.Local().Format("15:04") + " log " + oneLine(logs.Format(r, false), 160)})
	}
	// newest first
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].at.After(items[j-1].at); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
	out := make([]string, 0, n)
	for _, it := range items {
		if len(out) == n {
			break
		}
		out = append(out, it.text)
	}
	return out
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
