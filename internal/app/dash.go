package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/config"

	"github.com/habakkuk-ai/habakkuk/internal/channel/web"
	"github.com/habakkuk-ai/habakkuk/internal/protocols/mcp"
)

// openDash gives the web server the stores behind the dashboard at /app.
// It runs last in Open, once every store it hands over exists.
func (a *App) openDash() {
	if a.Web == nil {
		return
	}
	d := &web.Dash{
		Schedules: a.Schedules, Scheduler: a.Scheduler, Board: a.Board, Memory: a.Memory, Skills: a.Skills,
		Usage: a.Usage, Pricer: a.Pricer, Config: a.Cfg, Secret: a.Secret, Doctor: a.DoctorChecks,
		Budget: a.BudgetReport, Retros: a.Retro,
	}
	if a.Dispatcher != nil {
		d.BoardWake = a.Dispatcher.Wake
	}
	d.Home = a.Cfg.Home
	d.Pending = a.PendingRestart
	d.Models = a.cachedModels()
	d.Restart = func() error {
		if a.RequestRestart == nil {
			return errNoRestart
		}
		return a.RequestRestart()
	}
	a.Web.Dash = d
	if a.Scheduler != nil {
		a.Scheduler.OnFailure = a.Web.ScheduleFailed // web push
	}
}

// DoctorChecks checks what hk doctor checks that a running process can
// see: provider keys and logins, the databases, channel, HTTP and MCP
// credentials. Details name variables, never their values.
func (a *App) DoctorChecks(ctx context.Context) []web.Check {
	var out []web.Check
	add := func(name string, ok bool, format string, args ...any) {
		out = append(out, web.Check{Name: name, OK: ok, Detail: fmt.Sprintf(format, args...)})
	}
	cfg := a.Cfg
	for _, name := range slices.Sorted(maps.Keys(cfg.Providers)) {
		p := cfg.Providers[name]
		if p.Auth != "" {
			logins, _ := a.Auth.List()
			if _, ok := logins[p.Auth]; ok {
				add("provider "+name, true, "logged in (%s)", p.Auth)
			} else {
				add("provider "+name, false, "not logged in: run hk auth login %s", p.Auth)
			}
			continue
		}
		have := 0
		for _, env := range p.KeysEnv {
			if a.Secret(env) != "" {
				have++
			}
		}
		switch {
		case have == 0 && p.KeyOptional():
			add("provider "+name, true, "no key needed")
		case have == 0:
			add("provider "+name, false, "no keys set (%s)", strings.Join(p.KeysEnv, ", "))
		default:
			add("provider "+name, true, "%d/%d keys present", have, len(p.KeysEnv))
		}
	}
	for _, db := range []struct {
		name string
		db   *sql.DB
	}{{"control.db", a.Control}, {"transcripts.db", a.Transcripts}} {
		var res string
		if err := db.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&res); err != nil {
			add(db.name, false, "%v", err)
		} else {
			add(db.name, res == "ok", "%s", res)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Channels)) {
		ch := cfg.Channels[name]
		var missing []string
		for _, env := range ch.SecretVars() {
			if a.Secret(env) == "" {
				missing = append(missing, env)
			}
		}
		label := fmt.Sprintf("channel %s (%s)", name, ch.Type)
		if len(missing) > 0 {
			add(label, false, "missing %s", strings.Join(missing, ", "))
		} else {
			add(label, true, "credentials present, agent %s", ch.Agent)
		}
	}
	if h := cfg.HTTP; h != nil {
		var missing []string
		if h.TokenEnv != "" && a.Secret(h.TokenEnv) == "" {
			missing = append(missing, h.TokenEnv)
		}
		for _, name := range slices.Sorted(maps.Keys(h.Webhooks)) {
			if env := h.Webhooks[name].SecretEnv; a.Secret(env) == "" {
				missing = append(missing, env)
			}
		}
		if len(missing) > 0 {
			add("http "+h.Listen, false, "missing %s", strings.Join(missing, ", "))
		} else {
			add("http "+h.Listen, true, "%d webhook(s)", len(h.Webhooks))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.MCP)) {
		m := cfg.MCP[name]
		var missing []string
		for _, env := range mcp.ServerVars(m) {
			if a.Secret(env) == "" {
				missing = append(missing, env)
			}
		}
		if len(missing) > 0 {
			add("mcp "+name, false, "missing %s", strings.Join(missing, ", "))
		} else {
			add("mcp "+name, true, "credentials present")
		}
	}
	if a.MCP != nil {
		for _, st := range a.MCP.Status() {
			if st.State != "" && st.State != "connected" && st.Error != "" {
				add("mcp "+st.Name+" connection", false, "%s: %s", st.State, st.Error)
			}
		}
	}
	return out
}

var errNoRestart = errors.New("this process cannot restart itself; restart hk serve")

// PendingRestart names the config sections that changed in config.yaml and
// take effect only when hk serve restarts. A file that does not load counts
// as no change: the running config stays in charge.
func (a *App) PendingRestart() []string {
	next, err := config.Load(a.Cfg.Home)
	if err != nil {
		return nil
	}
	return restartNeeded(a.Cfg, next)
}

// cachedModels lists providers' models for the dashboard's model picker,
// asking the providers at most once every ten minutes.
func (a *App) cachedModels() func(ctx context.Context) []web.ModelList {
	var (
		mu   sync.Mutex
		at   time.Time
		last []web.ModelList
	)
	return func(ctx context.Context) []web.ModelList {
		mu.Lock()
		defer mu.Unlock()
		if last != nil && time.Since(at) < 10*time.Minute {
			return last
		}
		out := []web.ModelList{}
		for _, pm := range a.ListModels(ctx) {
			m := web.ModelList{Provider: pm.Name, Type: pm.Type, Models: pm.Models}
			if m.Models == nil {
				m.Models = []string{}
			}
			switch {
			case pm.Unsupported:
				m.Error = "this provider has no model list; type the model name"
			case pm.Err != nil:
				m.Error = "could not list models: " + pm.Err.Error()
			}
			out = append(out, m)
		}
		last, at = out, time.Now()
		return out
	}
}
