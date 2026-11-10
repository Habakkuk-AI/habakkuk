// Package app wires configuration, storage, models, tools and workers into a
// running Habakkuk instance.
package app

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/channel/discord"
	"github.com/habakkuk-ai/habakkuk/internal/channel/matrix"
	"github.com/habakkuk-ai/habakkuk/internal/channel/signal"
	"github.com/habakkuk-ai/habakkuk/internal/channel/slack"
	"github.com/habakkuk-ai/habakkuk/internal/channel/telegram"
	"github.com/habakkuk-ai/habakkuk/internal/channel/web"
	"github.com/habakkuk-ai/habakkuk/internal/channel/whatsapp"
	"github.com/habakkuk-ai/habakkuk/internal/channel/whatsappagent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/core/session"
	"github.com/habakkuk-ai/habakkuk/internal/core/state"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/consolidate"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/skills"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/llm/auth"
	"github.com/habakkuk-ai/habakkuk/internal/llm/budget"
	"github.com/habakkuk-ai/habakkuk/internal/llm/judge"
	"github.com/habakkuk-ai/habakkuk/internal/llm/usage"
	"github.com/habakkuk-ai/habakkuk/internal/ops/hooks"
	"github.com/habakkuk-ai/habakkuk/internal/ops/secrets"
	"github.com/habakkuk-ai/habakkuk/internal/protocols/mcp"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
	"github.com/habakkuk-ai/habakkuk/internal/tools/browser"
	"github.com/habakkuk-ai/habakkuk/internal/tools/media"
	"github.com/habakkuk-ai/habakkuk/internal/tools/meeting"
	"github.com/habakkuk-ai/habakkuk/internal/work/board"
	"github.com/habakkuk-ai/habakkuk/internal/work/schedule"
)

// MCPTurnWait is how long a turn waits for an MCP server it uses that is
// still making its first connection before going on without its tools.
const MCPTurnWait = 15 * time.Second

type App struct {
	Cfg         *config.Config
	Control     *sql.DB
	Transcripts *sql.DB
	Queue       *jobs.Queue
	Sessions    *session.Store
	Blobs       *store.Blobs
	Bus         *events.Bus
	Router      *llm.Router
	Tools       *tools.Registry
	MCP         *mcp.Manager
	Runner      *agent.Runner
	Gateway     *channel.Gateway // nil when no channels are configured
	Web         *web.Server      // nil when http is not configured
	Schedules   *schedule.Store
	Scheduler   *schedule.Scheduler
	Board       *board.Store
	Dispatcher  *board.Dispatcher
	Memory      *memory.Store
	Skills      *skills.Library
	Work        *Work           // background processes, sandboxes, checkpoints
	State       *state.Store    // per-session settings, goals, task lists, drafts (state.db)
	Hooks       *hooks.Runner   // commands run on events
	Usage       *usage.Store    // per-call token usage and per-session model choices
	Pricer      *usage.Pricer   // prices usage for habakkuk usage and /usage
	Budget      *budget.Tracker // spending limits, checked before every model call
	Auth        *auth.Store     // subscription logins (auth.json)
	Telemetry   *Telemetry      // metrics and trace export
	Log         *slog.Logger
	// Meetings attends video calls (meeting.go).
	Meetings *meeting.Manager
	// Consolidate reviews idle sessions into memory; nil unless memory.review.
	Consolidate *consolidate.Consolidator

	secrets map[string]string
	refs    *secrets.Resolver // secrets.refs: secret managers
	closers []func()
	// Judge is the optional decision model (judge.go).
	Judge *judge.Client
}

// Secret reads a credential from the environment, then a secret manager
// named in secrets.refs, then $HABAKKUK_HOME/.env.
func (a *App) Secret(name string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	if v := a.refs.Get(context.Background(), name); v != "" {
		return v
	}
	return a.secrets[name]
}

func Open(ctx context.Context, home string, log *slog.Logger) (*App, error) {
	cfg, err := config.Load(home)
	if err != nil {
		return nil, err
	}
	if log != nil {
		if s := cfg.Report.Summary(); s != "" {
			log.Info(s)
		}
		for _, k := range cfg.Report.Unknown {
			log.Warn("config.yaml: unknown key ignored", "key", k)
		}
	}
	a := &App{Cfg: cfg, Bus: events.NewBus(), Log: log}
	if a.Control, err = store.Open(ctx, filepath.Join(home, "control.db"), controlMigrations); err != nil {
		return nil, err
	}
	if a.Transcripts, err = store.Open(ctx, filepath.Join(home, "transcripts.db"), session.Migrations); err != nil {
		a.Close()
		return nil, err
	}
	blobDir := filepath.Join(home, "blobs")
	if a.Blobs, err = store.NewBlobs(blobDir); err != nil {
		a.Close()
		return nil, err
	}
	a.Queue = jobs.NewQueue(a.Control)
	a.Sessions = session.NewStore(a.Transcripts)

	a.secrets = loadDotEnv(filepath.Join(home, ".env"))
	a.refs = secrets.New(cfg, home, log)
	if err := a.openModels(ctx, home); err != nil {
		a.Close()
		return nil, err
	}

	a.Tools = tools.NewRegistry()
	tools.RegisterBuiltins(a.Tools)
	// The browser starts on an agent's first browser call, not here.
	br := browser.New(cfg.BrowserConfig(), home, log)
	browser.RegisterTools(a.Tools, br)
	a.closers = append(a.closers, br.Close)
	a.openComputer(home)
	a.openJudge()
	if err := a.openKnowledge(ctx, home); err != nil {
		a.Close()
		return nil, err
	}
	// MCP servers connect on Start (chat, run, habakkuk mcp) or in the
	// background (serve), not here, so commands that never run tools do not
	// spawn them.
	a.MCP = mcp.New(cfg.MCP, a.Tools, a.Secret, log)
	a.closers = append(a.closers, a.MCP.Close)

	specs := map[string]*agent.Spec{}
	for name, ac := range cfg.Agents {
		spec := &agent.Spec{
			Name: name, MaxTokens: ac.MaxTokens, Effort: ac.Effort, MaxIterations: ac.MaxIterations,
			Toolsets: ac.Toolsets, Mode: tools.Mode(ac.Permission), Approvals: tools.Approvals(ac.Approvals),
			Workspace: cfg.WorkspaceDir(name), Dir: cfg.AgentDir(name),
		}
		spec.CompactAt = max(ac.CompactAt, 0) // -1 (off) becomes 0
		if *ac.Web {
			webTools(cfg, ac, spec)
		}
		computerTools(cfg, ac, spec)
		for _, m := range append([]string{ac.Model}, ac.Fallbacks...) {
			ref, _ := config.ParseModelRef(m) // validated at load
			spec.Chain = append(spec.Chain, llm.Target{Endpoint: ref.Provider, Model: ref.Model})
		}
		if ac.FastModel != "" {
			ref, _ := config.ParseModelRef(ac.FastModel) // validated at load
			spec.FastChain = agent.FastChain(llm.Target{Endpoint: ref.Provider, Model: ref.Model}, spec.Chain)
			spec.Routing, spec.EscalateAfter = ac.Routing, ac.EscalateAfter
		}
		agentFeatures(ac, spec)
		spec.Home, spec.ReportHome = ac.Home, ac.ReportHome
		if spec.Approvals != tools.ApproveHuman {
			spec.Reviewer = newReviewer(a, name, ac, spec.Chain)
		}
		// The agent's folder holds its memory and chats' files: owner-only,
		// like the workspace Habakkuk creates for it.
		if err := os.MkdirAll(spec.Dir, 0o700); err != nil {
			a.Close()
			return nil, fmt.Errorf("agent %s folder: %w", name, err)
		}
		if err := os.MkdirAll(spec.Workspace, 0o700); err != nil {
			a.Close()
			return nil, fmt.Errorf("agent %s workspace: %w", name, err)
		}
		specs[name] = spec
	}
	a.Runner = &agent.Runner{
		Judge:  a.judgeForRouting(),
		Agents: specs, Sessions: a.Sessions, Queue: a.Queue, Models: a.Router, Tools: a.Tools,
		Bus: a.Bus, BlobDir: blobDir, Log: log, Media: a.Blobs.Get,
		Env: func(spec *agent.Spec, sessionID string) *tools.Env {
			env := &tools.Env{Agent: spec.Name, Workspace: spec.Workspace, Mode: spec.Mode, Blobs: a.Blobs, BlobDir: blobDir}
			a.Work.equip(env)
			env.Taint = tools.NewTaint(a.untrustedSession(context.Background(), sessionID))
			return env
		},
	}
	a.Runner.Snapshot = a.snapshot
	a.Runner.Policy = a.applyPolicy
	a.Runner.ToolsPending = func(ctx context.Context, toolsets []string) []string {
		return a.MCP.Pending(ctx, toolsets, MCPTurnWait)
	}
	if cfg.HTTP != nil {
		if err := a.openWeb(ctx, home); err != nil {
			a.Close()
			return nil, err
		}
	}
	if err := a.openNodes(ctx, home); err != nil {
		a.Close()
		return nil, err
	}
	if len(cfg.Channels) > 0 {
		a.openChannels(home)
	}
	a.openMedia()
	a.wireModelCommands()

	loc := time.Local
	if cfg.Timezone != "" {
		loc, _ = time.LoadLocation(cfg.Timezone) // validated at load
	}
	a.Runner.Location = loc
	a.Schedules = schedule.NewStore(a.Control, loc)
	a.Scheduler = &schedule.Scheduler{Store: a.Schedules, Queue: a.Queue, Runner: a.Runner, Log: log,
		// Read per run, so scripts see .env edits without a restart.
		Secrets: func() map[string]string { return loadDotEnv(filepath.Join(home, ".env")) }}
	if a.Gateway != nil {
		a.Scheduler.Deliver = a.Gateway.Deliver
	}
	schedule.RegisterTools(a.Tools, a.Schedules, a.here)
	a.openMeetings(home)
	a.Runner.RegisterDelegate(a.Tools)
	a.Runner.RegisterIdentityWithHistory(a.Tools, a.Memory.History())
	a.openBoard()
	a.openMCPHTTP()
	a.openIntegrations()
	if err := a.openWork(home); err != nil {
		a.Close()
		return nil, err
	}
	if err := a.openExtras(ctx, home); err != nil {
		a.Close()
		return nil, err
	}
	if err := a.openTelemetry(); err != nil {
		a.Close()
		return nil, err
	}
	a.wireMemory()
	a.openDash()
	return a, nil
}

// here is the reply address of the chat a session belongs to, if any.
func (a *App) here(ctx context.Context, sessionID string) json.RawMessage {
	if a.Gateway == nil {
		return nil
	}
	conv, err := a.Gateway.Store.BySession(ctx, sessionID)
	if err != nil || conv == nil {
		return nil
	}
	addr := conv.Address
	addr.ReplyTo = ""
	out, _ := json.Marshal(addr)
	return out
}

// controlMigrations is the single ordered schema for control.db. Packages
// own their steps; sorting lets a package add a later step without caring
// where it sits in this list.
var controlMigrations = sortedMigrations(slices.Concat(jobs.Migrations, channel.Migrations, schedule.Migrations, board.Migrations))

// OpenControl opens home's control.db on its own (creating and migrating
// it as needed), for commands that need its stores without the rest of the
// app.
func OpenControl(ctx context.Context, home string) (*sql.DB, error) {
	return store.Open(ctx, filepath.Join(home, "control.db"), controlMigrations)
}

func sortedMigrations(ms []store.Migration) []store.Migration {
	slices.SortStableFunc(ms, func(a, b store.Migration) int { return a.Version - b.Version })
	return ms
}

func (a *App) openChannels(home string) {
	g := &channel.Gateway{
		Channels: a.Cfg.Channels, Adapters: map[string]channel.Adapter{}, Runner: a.Runner,
		Queue: a.Queue, Store: channel.NewStore(a.Control), Log: a.Log, Blobs: a.Blobs, Language: a.Cfg.Language,
	}
	if t := a.Cfg.Transcription; t != nil {
		p := a.Cfg.Providers[t.Provider]
		base := p.BaseURL
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		tr := &media.Transcriber{BaseURL: base, Model: t.Model, Prompt: t.Prompt}
		for _, env := range p.KeysEnv {
			if tr.Key = a.Secret(env); tr.Key != "" {
				break
			}
		}
		g.Transcribe = tr.Transcribe
	}
	for name, ch := range a.Cfg.Channels {
		switch ch.Type {
		case "telegram":
			g.Adapters[name] = telegram.New(a.Secret(ch.TokenEnv), ch.APIURL)
		case "slack":
			var bots []string
			for _, env := range ch.BotTokensEnv {
				bots = append(bots, a.Secret(env))
			}
			sa := slack.New(a.Secret(ch.AppTokenEnv), bots, ch.APIURL)
			sa.Listen = ch.ListenChannels
			g.Adapters[name] = sa
		case "whatsapp":
			wa := whatsapp.New(name, filepath.Join(home, "channels", "whatsapp-"+name+".db"),
				filepath.Join(home, "backups"), a.Log)
			a.closers = append(a.closers, wa.Close)
			g.Adapters[name] = wa
		case "web":
			g.Adapters[name] = a.Web
			a.Web.Gateway = g
		case "discord":
			g.Adapters[name] = discord.New(a.Secret(ch.TokenEnv), ch.APIURL)
		case "whatsappagent":
			g.Adapters[name] = whatsappagent.New(whatsappagent.Config{Token: a.Secret(ch.TokenEnv), APIURL: ch.APIURL,
				StatePath: filepath.Join(home, "channels", "whatsappagent-"+name+".json")})
		case "signal":
			g.Adapters[name] = signal.New(ch.APIURL, ch.Account, ch.Agent)
		case "matrix":
			mx := matrix.New(ch.Homeserver, a.Secret(ch.TokenEnv), ch.Account,
				filepath.Join(home, "channels", "matrix-"+name+".json"))
			mx.Allowed = func(ctx context.Context, user string) bool { return g.Allowed(ctx, name, user) }
			g.Adapters[name] = mx
		default:
			if ad := a.platformAdapter(home, name, ch); ad != nil {
				g.Adapters[name] = ad
			}
		}
	}
	a.Runner.Approvers = g.ApproverFor
	a.Runner.Home = g // posts to agents' home chats
	channel.RegisterTools(a.Tools, g)
	a.Gateway = g
}

// Pools returns the worker pools for this app's job kinds: one for turns
// (and, in the serving process, deliveries), one for delegated sub-turns,
// and in the serving process one for schedules, whose scripts may run for
// minutes without holding up chat turns. Only the process that runs the
// channel adapters (serve) should pass serving=true; others leave those
// jobs to it.
func (a *App) Pools(serving bool) []*jobs.Pool {
	handlers := map[string]jobs.Handler{agent.KindTurn: a.Runner.HandleTurn}
	if a.Gateway != nil {
		handlers[agent.KindTurn] = a.Gateway.HandleTurn
		if serving {
			handlers[channel.KindDeliver] = a.Gateway.HandleDeliver
		}
	}
	if serving {
		handlers[board.KindCard] = a.Dispatcher.Handle
	}
	main := &jobs.Pool{
		Queue: a.Queue, Handlers: handlers, Concurrency: a.Cfg.Workers.Concurrency,
		Lease: a.Cfg.Workers.Lease, Log: a.Log,
	}
	sub := &jobs.Pool{
		Queue: a.Queue, Handlers: map[string]jobs.Handler{agent.KindSubturn: a.Runner.HandleTurn},
		Concurrency: a.Cfg.Workers.Concurrency, Lease: a.Cfg.Workers.Lease, Log: a.Log,
	}
	pools := []*jobs.Pool{main, sub}
	if serving {
		pools = append(pools, &jobs.Pool{
			Queue: a.Queue, Handlers: map[string]jobs.Handler{schedule.KindScheduled: a.Scheduler.Handle},
			Concurrency: a.Cfg.Workers.Concurrency, Lease: a.Cfg.Workers.Lease, Log: a.Log,
		})
	}
	if serving && a.Consolidate != nil {
		pools = append(pools, a.memoryPool())
	}
	if serving && a.Meetings != nil {
		pools = append(pools, a.meetingPool())
	}
	a.observePools(pools)
	wake := func() {
		for _, p := range pools {
			p.Wake()
		}
	}
	a.Runner.Wake, a.Scheduler.Wake = wake, wake
	if a.Gateway != nil {
		a.Gateway.Wake = wake
	}
	return pools
}

// Maintain applies retention: prunes old sessions and finished jobs, then
// checkpoints and vacuums so the files actually shrink.
func (a *App) Maintain(ctx context.Context) error {
	a.curateSkills()          // when skills.curate says it is due
	a.pruneMemoryHistory(ctx) // memory.history_days, apart from retention.days
	if a.Cfg.Retention.Days <= 0 {
		return nil
	}
	cutoff := time.Now().AddDate(0, 0, -a.Cfg.Retention.Days)
	ns, err := a.Sessions.Prune(ctx, cutoff)
	if err != nil {
		return err
	}
	nj, err := a.Queue.Prune(ctx, cutoff)
	if err != nil {
		return err
	}
	a.pruneExtras(ctx, cutoff)
	a.pruneMeetings(cutoff)
	if _, err := a.Usage.Prune(ctx, cutoff); err != nil {
		return err
	}
	for _, db := range []*sql.DB{a.Control, a.Transcripts} {
		if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			return err
		}
		if ns+nj > 0 {
			if _, err := db.ExecContext(ctx, "VACUUM"); err != nil {
				return err
			}
		}
	}
	a.Log.Info("maintenance done", "sessions_pruned", ns, "jobs_pruned", nj)
	return nil
}

func (a *App) Close() {
	for _, c := range a.closers {
		c()
	}
	for _, db := range []*sql.DB{a.Control, a.Transcripts} {
		if db != nil {
			db.Close()
		}
	}
}

// loadDotEnv reads KEY=VALUE lines. Process environment wins over the file.
func loadDotEnv(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out
}

// Init writes a starter config and agent directory. It never overwrites.
func Init(home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	cfgPath := filepath.Join(home, "config.yaml")
	if _, err := os.Stat(cfgPath); err == nil {
		return fmt.Errorf("%s already exists", cfgPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := store.WriteFileAtomic(cfgPath, []byte(config.Template), 0o600); err != nil {
		return err
	}
	dir := filepath.Join(home, "agents", "habakkuk")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	soul := "You are Habakkuk, a sharp, warm and dependable personal AI agent. You act, verify, and report plainly.\n"
	return store.WriteFileAtomic(filepath.Join(dir, "SOUL.md"), []byte(soul), 0o600)
}
