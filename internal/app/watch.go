package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// Hot reload. Most of what shapes an agent is read when a session starts:
// SOUL.md and AGENTS.md, the memory and skill listings, and persona files
// (read on every turn). So edits to them reach every new session (/new in a
// chat) without a restart; a running session keeps the prompt it was frozen
// with. Watch makes that dependable and covers what is cached:
//
//   - memory notes edited by hand are reindexed as soon as they change, so
//     search and the next session's listing see them at once;
//   - config.yaml is re-read: hooks are swapped in place, and a change to
//     anything else (agents, models, providers, channels, MCP servers, http,
//     workers, retention, timezone, transcription) is logged as needing a
//     restart of `hk serve`, since running turns and adapters hold it;
//   - an invalid config.yaml is reported and ignored; the running one stays.
//
// Files are polled (no platform file-watching API, no cgo); a change shows
// up within one interval.

// WatchInterval is how often Watch looks for changes.
const WatchInterval = 3 * time.Second

// Watch polls the configuration and agent files until ctx ends.
func (a *App) Watch(ctx context.Context) {
	w := &watcher{a: a, seen: a.fingerprints()}
	t := time.NewTicker(WatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.check(ctx)
		}
	}
}

type watcher struct {
	a    *App
	seen map[string]string // watched item -> fingerprint
}

// check compares every watched item with what it saw last and reacts to
// the ones that changed. It returns their names, sorted.
func (w *watcher) check(ctx context.Context) []string {
	now := w.a.fingerprints()
	var changed []string
	for k, v := range now {
		if w.seen[k] != v {
			changed = append(changed, k)
		}
	}
	for k := range w.seen {
		if _, ok := now[k]; !ok {
			changed = append(changed, k)
		}
	}
	w.seen = now
	slices.Sort(changed)
	for _, item := range changed {
		w.a.reload(ctx, item)
	}
	return changed
}

// Watched items: "config", "memory:<agent>", "prompt:<agent>",
// "skills:<agent>", "skills", "personalities".
func (a *App) fingerprints() map[string]string {
	home := a.Cfg.Home
	out := map[string]string{"config": fingerprint(filepath.Join(home, "config.yaml"))}
	out["skills"] = fingerprint(filepath.Join(home, "skills"))
	out["personalities"] = fingerprint(filepath.Join(home, "personalities"))
	out["plugins"] = fingerprint(filepath.Join(config.PluginsDir(home), ".lock.json")) // enable, disable
	for _, name := range a.Cfg.AgentNames() {
		dir := a.Cfg.AgentDir(name)
		out["prompt:"+name] = fingerprint(filepath.Join(dir, "SOUL.md")) + fingerprint(filepath.Join(dir, "AGENTS.md"))
		out["memory:"+name] = fingerprint(filepath.Join(dir, "memory"))
		out["skills:"+name] = fingerprint(filepath.Join(dir, "skills"))
	}
	return out
}

// fingerprint hashes the names, sizes and modification times under path
// (a file or a directory tree); "" when it does not exist. Trash folders
// are skipped.
func fingerprint(path string) string {
	h := sha256.New()
	n := 0
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == ".trash" {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		n++
		fmt.Fprintf(h, "%s\x00%d\x00%d\n", p, info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if n == 0 {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

func (a *App) reload(ctx context.Context, item string) {
	kind, agentName, _ := strings.Cut(item, ":")
	switch kind {
	case "config", "plugins":
		a.reloadConfig()
	case "memory":
		if err := a.Memory.Reindex(ctx, agentName); err != nil {
			a.Log.Warn("memory reindex failed", "agent", agentName, "err", err)
		} else {
			a.Log.Info("memory changed on disk; reindexed", "agent", agentName)
		}
	default:
		a.Log.Info("agent files changed; new sessions use them (/new in a chat starts one)", "what", item)
	}
}

// reloadConfig re-reads config.yaml, applies what can change while running
// (hooks) and names the sections that need a restart.
func (a *App) reloadConfig() {
	next, err := config.Load(a.Cfg.Home)
	if err != nil {
		a.Log.Error("config.yaml changed but does not load; still running the previous config", "err", err)
		return
	}
	if a.Hooks != nil && !reflect.DeepEqual(next.Hooks, a.Cfg.Hooks) {
		a.Hooks.Set(next.Hooks)
		a.Cfg.Hooks = next.Hooks // only the watcher reads it after startup
		a.Log.Info("hooks reloaded", "count", a.Hooks.Count())
	}
	a.reloadBudget(next)
	a.reloadDisplay(next)
	if pending := restartNeeded(a.Cfg, next); len(pending) > 0 {
		a.Log.Warn("config.yaml changed; restart hk serve to apply", "sections", strings.Join(pending, ", "))
	}
}

// reloadDisplay applies each channel's progress and approvals_detail from
// the file to the running gateway, which uses them from the next turn. The
// config the process started with is left as it was; the gateway holds the
// live values (channel.Gateway.Display).
func (a *App) reloadDisplay(next *config.Config) {
	if a.Gateway == nil {
		return
	}
	for name, ch := range next.Channels {
		if _, running := a.Cfg.Channels[name]; !running || ch == nil {
			continue // a channel added to the file starts with the next restart
		}
		before := a.Gateway.Display(name)
		a.Gateway.SetDisplay(name, ch)
		if after := a.Gateway.Display(name); after != before {
			a.Log.Info("channel display settings reloaded", "channel", name,
				"progress", after.Progress, "approvals_detail", after.ApprovalsDetail)
		}
	}
}

// withoutDisplay is the channels without the two settings that reload in
// place, so changing only those does not ask for a restart.
func withoutDisplay(channels map[string]*config.Channel) map[string]config.Channel {
	out := make(map[string]config.Channel, len(channels))
	for name, ch := range channels {
		if ch != nil {
			c := *ch
			c.Progress, c.ApprovalsDetail = "", ""
			out[name] = c
		}
	}
	return out
}

// restartNeeded names the config sections that differ and only take effect
// on restart.
func restartNeeded(cur, next *config.Config) []string {
	var out []string
	for _, s := range []struct {
		name string
		a, b any
	}{
		{"providers", cur.Providers, next.Providers},
		{"agents", withoutBudgets(cur.Agents), withoutBudgets(next.Agents)},       // budgets reload in place
		{"channels", withoutDisplay(cur.Channels), withoutDisplay(next.Channels)}, // progress and approvals_detail reload in place
		{"mcp_servers", cur.MCP, next.MCP},
		{"http", cur.HTTP, next.HTTP},
		{"transcription", cur.Transcription, next.Transcription},
		{"timezone", cur.Timezone, next.Timezone},
		{"workers", cur.Workers, next.Workers},
		{"retention", cur.Retention, next.Retention},
		{"metrics", cur.Metrics, next.Metrics},
		{"telemetry", cur.Telemetry, next.Telemetry},
		{"logging", cur.Logging, next.Logging},
		{"tools", cur.Tools, next.Tools},
		{"secrets", cur.Secrets, next.Secrets},
	} {
		if !reflect.DeepEqual(s.a, s.b) {
			out = append(out, s.name)
		}
	}
	return out
}
