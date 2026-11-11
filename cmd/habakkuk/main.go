// Command habakkuk is the Habakkuk agent CLI.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/app"
	"github.com/habakkuk-ai/habakkuk/internal/channel"
	signalch "github.com/habakkuk-ai/habakkuk/internal/channel/signal"
	"github.com/habakkuk-ai/habakkuk/internal/channel/whatsapp"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/events"
	plat "github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/ops/logs"
	"github.com/habakkuk-ai/habakkuk/internal/protocols/mcp"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
	"github.com/habakkuk-ai/habakkuk/internal/work/schedule"
)

var version = "0.3.0-dev"

const usage = `Habakkuk: a durable personal AI agent.

hk is the short name for habakkuk. Every command runs under either one: hk chat is habakkuk chat.

Any command can run on a remote Habakkuk (a server you reach over ssh, or https):
  hk -r NAME [-q] [--yes-remote] COMMAND [ARGS...]
                                    run COMMAND there; HABAKKUK_REMOTE or the default remote do the
                                    same without -r, and -r local runs here; -q hides the
                                    (remote: NAME) line; --yes-remote allows serve, init, restore,
                                    update, migrate, setup service and config migrate there

Usage:
  hk init                             write a starter config to $HABAKKUK_HOME (default ~/.habakkuk)
  hk setup [model|channel [TYPE]|name|service] [-y] [flags]
                                    a one-minute wizard: model, where to talk (WhatsApp agent,
                                    Telegram, web, ...), names; -y runs it from flags (hk setup -h)
  hk migrate [--from SOURCE] [--dry-run] [--secrets copy|skip] [--force] [-y]
                                    move agents, memory, skills, schedules, MCP servers, channels
                                    and keys over from another agent (sources: hk migrate -h)
  hk chat [-agent A] [-session ID] [-resume] [-plain]
                                    interactive conversation: the terminal UI on a terminal,
                                    line by line with -plain or when piped
  hk tui [-agent A] [-session ID] [-resume]
                                    the full-screen terminal UI (-resume picks a recent session)
  hk talk [-agent A] [-session ID] [-wake "hey habakkuk"] [-no-wake]
                                    a voice loop on this machine: the mic through ffmpeg, replies
                                    spoken with tts; talk over a reply to stop it
  hk run [-agent A] [-skill NAME] PROMPT
                                    one turn, print the answer, exit (-skill: follow that skill)
  hk serve                            run workers: resumes interrupted turns, applies retention;
                                    with an http section, also the chat page, API and webhooks
  hk channels                         show channel health
  hk mcp                              connect to MCP servers and list their status and tools
  hk mcp serve                        be an MCP server on stdio (habakkuk_ask, sessions, board, schedules)
  hk mcp catalog [QUERY]              list well-known MCP servers Habakkuk can add
  hk mcp add NAME [param=value] add a catalog server to config.yaml
  hk mcp login [-device] NAME         sign in to a remote MCP server that uses OAuth (auth: oauth)
  hk mcp logout NAME                  forget that sign-in
  hk acp [-agent A]                   be a coding agent for an editor (Agent Client Protocol on stdio)
  hk send -to CHANNEL:CHAT [-file PATH] TEXT
                                    queue a message or file to a chat without running a turn
  hk pair CODE                        let the sender who got this pairing code talk to Habakkuk
  hk node pair [-ttl 10m]             print a one-time code (and QR) for pairing a device as a node
  hk node run -server URL [-code CODE] -name NAME [-allow-run] [-allow-camera] [-dir DIR]
                                    lend this machine's capabilities to a Habakkuk server
  hk nodes [list|revoke NAME]         paired device nodes; revoke cuts one off
  hk whatsapp link [-account A] [-phone NUMBER]
                                    link a WhatsApp account (phone gives a code instead of a QR)
  hk schedules [list|add|rm|pause|resume|run]
                                    scheduled tasks (add: -agent A -name N -when SPEC -prompt P [-to CHANNEL:CHAT];
                                    scripts: -run CMD [-on-output deliver|agent|silent] [-timeout 2m] [-workdir DIR])
  hk board [list|show|add|done|cancel]
                                    the shared task board (add: -agent A -title T [-body B] [-after IDS] [-to CHANNEL:CHAT];
                                    done: ID -result R)
  hk meetings [list|show [-transcript] ID|leave ID]
                                    meetings Habakkuk attends (meeting_join): status, notes, transcript
  hk sessions [-agent A]              list recent sessions
  hk sessions compact SESSION         summarize a long session's older messages now
  hk sessions export ID [-format md|json|html] [-o FILE]
                                    write a transcript as Markdown, JSON or an HTML page
  hk sessions search [-agent A] QUERY
                                    full-text search over every session
  hk sessions archive|unarchive|rm [-y] ID...
                                    hide, restore or delete sessions; sessions archived lists them
  hk show SESSION                     print a transcript
  hk memory [-agent A] [search QUERY | show SLUG | forget SLUG | trust SLUG [yes|no] | profile | log]
  hk memory history [-agent A] [-file NAME] [-since 7d] | diff ID | restore ID [-after] | pin|unpin NOTE
                                    list, search, read, remove or vouch for an agent's memories;
                                    print its user profile or the background review log;
                                    list, diff and undo changes to memory, profile, identity
                                    and skills; pin what background jobs must not rewrite
  hk retro [-agent A] [-days N] [-dry-run]
                                    look back over an agent's work now: what went wrong, likely
                                    causes, proposed changes (-dry-run prints evidence and plan only)
  hk retro list [-agent A] | show ID | apply ID N [N...] | reject ID N
                                    past retros; approve or reject their proposals
  hk skills [-agent A]                list the skills an agent can use
  hk skills [search|install|update|remove|approve|restore|curate]
                                    skills from GitHub, URLs, paths and registries (hk skills help)
  hk plugins [list|search|install|enable|disable|update|remove|restore]
                                    plugins: MCP servers, skills, hooks and commands (docs/tools/plugins.md)
  hk checkpoints [-agent A]           list workspace snapshots taken before turns changed files
  hk rollback [-agent A] [-y] ID [PATH]
                                    restore the workspace (or one path) to a checkpoint
  hk models                           list providers and the models each one serves
  hk auth [login|list|logout]         subscription logins (login chatgpt|copilot)
  hk usage [-days N] [-agent A] tokens and cost by model and agent, and spend against the limits
  hk doctor                           check config, credentials and storage
  hk status                           version, serve uptime, channels, queue, recent errors, today's cost
  hk computer view                    how to watch the agents' virtual desktops (computer backend xvfb)
  hk logs [-f] [-level warn] [-since 1h] [-grep X] [-n 100]
                                    print habakkuk.log readably (-f keeps following it)
  hk approvals [list] [-agent A] | rm ID
                                    approval rules: calls an agent makes without asking, from
                                    "Always allow" answers and agents.<name>.allow
  hk security [-fix] [-network] effective tool policy, audit of permissions, exposure and risky
                                    settings (-fix: safe fixes; -network: every outbound endpoint)
  hk expose tailscale [-port 443] [-print] | status | off
                                    share the HTTP listener on your tailnet (tailscale serve, never public)
  hk remote add NAME TARGET [-identity PATH] [-home DIR] [-bin PATH] [-token-env NAME] [-default]
                                    save a remote: ssh://user@host[:port] runs every command,
                                    https://host runs status and chat (hk remote help)
  hk remote [list] | rm NAME | default NAME|none
                                    list, remove, or pick the remote plain commands run on
  hk backup [-o FILE] [-env] [-workspaces]
                                    archive config, agents, skills and database snapshots
  hk restore [-force] FILE            unpack a backup into $HABAKKUK_HOME
  hk update [-check] [-force] [-channel stable|beta]
                                    install the newest release on the channel: checksum (and signature)
                                    verified, checked against this home by the new binary, after a
                                    backup of config and databases
  hk update --rollback [-force] put back the previous binary, databases and config
  hk config migrate [-dry-run]        rewrite config.yaml in the current format (keeps a timestamped copy)
  hk doctor --upgrade-check           check that this binary can run on $HABAKKUK_HOME, changing nothing
  hk version                          version, commit, build date and database schema versions
`

func main() {
	argv, code, done := dispatchRemote(os.Args[1:], remoteStdio(), os.Getenv)
	if done {
		os.Exit(code)
	}
	if len(argv) < 1 {
		noArgs(os.Stdout, config.DefaultHome(), cliName(), time.Now())
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	home := config.DefaultHome()
	cmd, args := argv[0], argv[1:]
	if exe, err := os.Executable(); err == nil && plat.IsWindows {
		removeReplaced(exe) // binaries an update moved aside
	}
	var err error
	switch cmd {
	case "setup":
		err = setupCmd(ctx, home, args)
	case "migrate":
		err = migrateCmd(ctx, home, args)
	case "update":
		err = updateCmd(ctx, home, args)
	case "config":
		err = configCmd(home, args, os.Stdout)
	case "backup":
		err = backupCmd(ctx, home, args)
	case "restore":
		err = restoreCmd(home, args)
	case "init":
		err = app.Init(home)
		if err == nil {
			fmt.Printf("Wrote %s/config.yaml and agents/habakkuk/SOUL.md.\nPut API keys in %s/.env (e.g. ANTHROPIC_API_KEY=...), then run `hk chat`.\n", home, home)
		}
	case "chat":
		err = chatCmd(ctx, home, args)
	case "talk":
		err = talk(ctx, home, args)
	case "tui":
		err = tuiCmd(ctx, home, args)
	case "run":
		err = run(ctx, home, args)
	case "serve":
		err = serve(ctx, home)
	case "sessions":
		err = sessions(ctx, home, args)
	case "show":
		err = show(ctx, home, args)
	case "channels":
		err = channels(ctx, home)
	case "mcp":
		if len(args) > 0 && args[0] == "serve" {
			err = mcpServe(ctx, home, args[1:])
		} else if len(args) > 0 && args[0] == "catalog" {
			err = mcpCatalogCmd(args[1:])
		} else if len(args) > 0 && args[0] == "add" {
			err = mcpAddCmd(home, args[1:])
		} else if len(args) > 0 && args[0] == "login" {
			err = mcpLoginCmd(ctx, home, args[1:])
		} else if len(args) > 0 && args[0] == "logout" {
			err = mcpLogoutCmd(ctx, home, args[1:])
		} else {
			err = mcpCmd(ctx, home)
		}
	case "acp":
		err = acpCmd(ctx, home, args)
	case "send":
		err = sendCmd(ctx, home, args)
	case "pair":
		err = pair(ctx, home, args)
	case "node":
		err = nodeCmd(ctx, home, args)
	case "nodes":
		err = nodesCmd(ctx, home, args, os.Stdout)
	case "whatsapp":
		err = whatsappCmd(ctx, home, args)
	case "schedules":
		err = schedules(ctx, home, args)
	case "board":
		err = boardCmd(ctx, home, args)
	case "meetings":
		err = meetingsCmd(ctx, home, args, os.Stdout)
	case "memory":
		err = memoryCmd(ctx, home, args)
	case "retro":
		err = retroCmd(ctx, home, args, os.Stdout)
	case "skills":
		err = skillsCmd(ctx, home, args)
	case "plugins":
		err = pluginsCmd(ctx, home, args)
	case "checkpoints":
		err = checkpointsCmd(ctx, home, args)
	case "rollback":
		err = rollbackCmd(ctx, home, args)
	case "doctor":
		err = doctorCmd(ctx, home, args)
	case "status":
		err = statusCmd(ctx, home, os.Stdout)
	case "computer":
		err = computerCmd(home, args, os.Stdout)
	case "logs":
		err = logsCmd(ctx, home, args, os.Stdout)
	case "security":
		err = securityCmd(home, args, os.Stdout)
	case "approvals":
		err = approvalsCmd(home, args, os.Stdout)
	case "expose":
		err = exposeCmd(ctx, home, args, os.Stdout, nil)
	case "auth":
		err = authCmd(ctx, home, args)
	case "usage":
		err = usageCmd(ctx, home, args)
	case "models":
		err = modelsCmd(ctx, home)
	case "version":
		err = versionCmd(ctx, home, os.Stdout)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// logger opens habakkuk.log: JSON, rotated by size (logging.max_mb,
// logging.backups), with secret values and token patterns redacted.
func logger(home string) (*slog.Logger, *logs.Redactor, func()) {
	log, red, closeLog, err := logs.Open(home)
	if err != nil {
		red = logs.NewRedactor(os.Stderr)
		return slog.New(slog.NewTextHandler(red, nil)), red, func() {}
	}
	return log, red, closeLog
}

func open(ctx context.Context, home string) (*app.App, func(), error) {
	log, red, closeLog := logger(home)
	a, err := app.Open(ctx, home, log)
	if err != nil {
		closeLog()
		return nil, nil, err
	}
	reportConfig(os.Stderr, a.Cfg)
	red.Add(a.SecretValues()...)
	return a, func() { a.Close(); closeLog() }, nil
}

func defaultAgent(cfg *config.Config, name string) (string, error) {
	if name != "" {
		if _, ok := cfg.Agents[name]; !ok {
			return "", fmt.Errorf("unknown agent %q", name)
		}
		return name, nil
	}
	if _, ok := cfg.Agents["habakkuk"]; ok {
		return "habakkuk", nil
	}
	return cfg.AgentNames()[0], nil
}

// startWorkers runs the job pool in the background and returns a func that
// stops it and waits.
func startWorkers(ctx context.Context, a *app.App, serving bool) (*poolHandle, func()) {
	if serving {
		a.MCP.StartBackground() // never hold up channels; turns wait for a server they need
	} else {
		a.MCP.Start(ctx) // turns need MCP tools registered; slow servers join later
	}
	ctx, cancel := context.WithCancel(ctx)
	pools := a.Pools(serving)
	var wg sync.WaitGroup
	for _, p := range pools {
		wg.Add(1)
		go func() { defer wg.Done(); p.Run(ctx) }()
	}
	return &poolHandle{wake: func() {
		for _, p := range pools {
			p.Wake()
		}
	}}, func() { cancel(); wg.Wait() }
}

type poolHandle struct{ wake func() }

// terminal is shared by the REPL and the approver so they never read stdin
// concurrently.
type terminal struct {
	mu sync.Mutex
	in *bufio.Reader
}

func (t *terminal) readLine(prompt string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Print(prompt)
	line, err := t.in.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (t *terminal) Approve(ctx context.Context, req tools.ApprovalRequest) (bool, error) {
	if req.Review != "" {
		fmt.Printf("\n\033[33mreviewer unsure: %s\033[0m", req.Review)
	}
	ans, err := t.readLine(fmt.Sprintf("\n\033[33m? allow %s %s [y/N] \033[0m", req.Tool, req.Summary))
	if err != nil {
		return false, nil
	}
	ans = strings.ToLower(strings.TrimSpace(ans))
	return ans == "y" || ans == "yes", nil
}

func chat(ctx context.Context, home string, args []string) error {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	agentName := fs.String("agent", "", "agent to talk to")
	sessionID := fs.String("session", "", "resume a session")
	fs.Parse(args)

	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	name, err := defaultAgent(a.Cfg, *agentName)
	if err != nil {
		return err
	}
	sid := *sessionID
	if sid == "" {
		sess, err := a.Runner.NewSessionWith(ctx, name, agent.SessionOptions{Chat: true})
		if err != nil {
			return err
		}
		sid = sess.ID
	} else if _, err := a.Sessions.Get(ctx, sid); err != nil {
		return err
	}

	term := &terminal{in: bufio.NewReader(os.Stdin)}
	stdinTerminal := isTerminal(os.Stdin)
	detach := a.Runner.Attach(sid, term)
	defer detach()
	evs, unsub := a.Bus.Subscribe(sid)
	defer unsub()
	pool, stopWorkers := startWorkers(ctx, a, false)
	defer stopWorkers()

	fmt.Printf("Habakkuk %s · agent %s · session %s\nType /exit to quit, /model to switch models, /usage for tokens and cost. Interrupted turns resume automatically.\n", version, name, sid)
	for ctx.Err() == nil {
		line, err := term.readLine("\n\033[1m> \033[0m")
		if err != nil {
			return nil
		}
		line = strings.TrimSpace(line)
		switch line {
		case "":
			continue
		case "/exit", "/quit":
			return nil
		}
		prompted := false
		if name, arg, _ := strings.Cut(line, " "); strings.HasPrefix(name, "/") {
			// The chat app's commands: /model, /usage, /skills, /skill,
			// /learn and plugin commands. A prompt command becomes the message.
			if cmd, ok := a.ChatCommands()[name]; ok {
				if cmd.Prompt == nil {
					fmt.Println(cmd.Run(ctx, sid, strings.TrimSpace(arg)))
					continue
				}
				text, err := cmd.Prompt(ctx, sid, strings.TrimSpace(arg))
				if err != nil {
					fmt.Println(err)
					continue
				}
				line, prompted = text, true
			}
		}
		// A line typed at the prompt, on a terminal, is Typed; a skill or
		// plugin prompt carries file text the owner did not type, and piped
		// input is a program's.
		submit := a.Runner.SubmitTyped
		if prompted || !stdinTerminal {
			submit = a.Runner.Submit
		}
		if _, err := submit(ctx, sid, line); err != nil {
			return err
		}
		pool.wake()
		if err := streamTurn(ctx, evs, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "\n\033[31m"+err.Error()+"\033[0m")
		}
	}
	return nil
}

// streamTurn prints events until the turn finishes.
func streamTurn(ctx context.Context, evs <-chan events.Event, w io.Writer) error {
	thinking := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e := <-evs:
			switch e.Kind {
			case events.Thinking:
				if !thinking && e.Text != "" {
					fmt.Fprint(w, "\033[2m")
					thinking = true
				}
				fmt.Fprint(w, e.Text)
			case events.Text:
				if thinking {
					fmt.Fprint(w, "\033[0m\n")
					thinking = false
				}
				fmt.Fprint(w, e.Text)
			case events.ToolStart:
				fmt.Fprintf(w, "\n\033[36m→ %s\033[0m", e.Text)
			case events.ToolDone:
				color := "32"
				if e.IsError {
					color = "31"
				}
				fmt.Fprintf(w, "\n\033[%sm  %s\033[0m\n", color, e.Text)
			case events.Retry, events.Review:
				fmt.Fprintf(w, "\n\033[33m[%s]\033[0m\n", e.Text)
			case events.TurnDone:
				fmt.Fprintln(w)
				return nil
			case events.TurnError:
				return errors.New(e.Text)
			}
		}
	}
}

func run(ctx context.Context, home string, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	agentName := fs.String("agent", "", "agent to use")
	skill := fs.String("skill", "", "follow this skill's instructions for the prompt")
	fs.Parse(args)
	prompt := strings.Join(fs.Args(), " ")
	if prompt == "" && *skill == "" {
		return errors.New("usage: hk run [-skill NAME] PROMPT")
	}
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	name, err := defaultAgent(a.Cfg, *agentName)
	if err != nil {
		return err
	}
	if *skill != "" {
		if prompt, err = a.SkillPrompt(name, *skill, prompt); err != nil {
			return err
		}
	}
	sess, err := a.Runner.NewSession(ctx, name)
	if err != nil {
		return err
	}
	evs, unsub := a.Bus.Subscribe(sess.ID)
	defer unsub()
	jobID, err := a.Runner.Submit(ctx, sess.ID, prompt)
	if err != nil {
		return err
	}
	pool, stopWorkers := startWorkers(ctx, a, false)
	defer stopWorkers()
	pool.wake()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e := <-evs:
			if e.Kind == events.TurnError {
				fmt.Fprintln(os.Stderr, "attempt failed:", e.Text)
			}
		case <-time.After(50 * time.Millisecond): // the job is marked done just after its last event
		}
		j, err := a.Queue.Get(ctx, jobID)
		if err != nil {
			return err
		}
		switch j.Status {
		case "done":
			fmt.Println(j.Result)
			fmt.Fprintln(os.Stderr, "session:", sess.ID)
			return nil
		case "failed", "canceled":
			return fmt.Errorf("turn %s: %s", j.Status, j.LastError)
		}
	}
}

func serve(ctx context.Context, home string) error {
	// serve.json from the first moment, so hk status says "starting"
	// rather than "not running" while the rest comes up.
	progress := newServeProgress(home, time.Now)
	defer progress.run(ctx)()
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	a.Telemetry.SetVersion(version)
	progress.setLog(a.Log)
	fmt.Fprintf(os.Stderr, "habakkuk %s serving agents %v, channels %v; logs at %s/habakkuk.log\n",
		version, a.Cfg.AgentNames(), channelNames(a), home)
	var ln net.Listener
	if a.Web != nil {
		if ln, err = a.Web.Listen(); err != nil { // before any work starts, so a bad address fails fast
			return err
		}
		fmt.Fprintf(os.Stderr, "http on http://%s\n", ln.Addr())
		progress.setListen(ln.Addr().String())
		fmt.Fprintf(os.Stderr, "dashboard: http://%s/app/\n", ln.Addr())
	}
	progress.setPhase("starting workers")
	// MCP servers connect in the background: channels, schedules and the
	// board start now, and a turn that needs a slow server waits for it.
	_, stopWorkers := startWorkers(ctx, a, true)
	defer stopWorkers()
	var wg sync.WaitGroup
	defer wg.Wait()
	if ln != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.Web.Serve(ctx, ln); err != nil {
				a.Log.Error("http server stopped", "err", err)
			}
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); a.Scheduler.Run(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); a.Watch(ctx) }() // hot reload: hooks, memory index, restart notices
	wg.Add(1)
	go func() { defer wg.Done(); a.Dispatcher.Run(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); a.MemoryLoop(ctx) }() // embeddings, memory review, nightly pass
	wg.Add(1)
	go func() { defer wg.Done(); a.RunIntegrations(ctx) }() // Home Assistant event triggers
	if a.Gateway != nil {
		a.Gateway.OnChannelState = progress.channelState
		progress.channelsStarting(slices.Sorted(maps.Keys(a.Gateway.Adapters)))
		wg.Add(1)
		go func() { defer wg.Done(); a.Gateway.Run(ctx) }()
	} else {
		progress.channelsStarting(nil)
	}
	if err := a.Maintain(ctx); err != nil {
		a.Log.Error("maintenance failed", "err", err)
	}
	tick := time.NewTicker(6 * time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "shutting down; in-flight turns resume on next start")
			return nil
		case <-tick.C:
			if err := a.Maintain(ctx); err != nil {
				a.Log.Error("maintenance failed", "err", err)
			}
		}
	}
}

func sessions(ctx context.Context, home string, args []string) error {
	if len(args) > 0 && args[0] == "compact" {
		return compactSession(ctx, home, args[1:])
	}
	if handled, err := sessionCommand(ctx, home, args); handled {
		return err
	}
	fs := flag.NewFlagSet("sessions", flag.ExitOnError)
	agentName := fs.String("agent", "", "filter by agent")
	limit := fs.Int("n", 20, "how many")
	fs.Parse(args)
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	list, err := a.Sessions.List(ctx, *agentName, *limit)
	if err != nil {
		return err
	}
	for _, s := range list {
		fmt.Printf("%s  %-10s  %s  %s\n", s.ID, s.Agent, s.UpdatedAt.Format("2006-01-02 15:04"), s.Title)
	}
	return nil
}

// compactSession queues a compaction as a turn of the session, so it never
// runs beside a turn in progress, and waits for it.
func compactSession(ctx context.Context, home string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: hk sessions compact SESSION")
	}
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	jobID, err := a.Runner.SubmitTurn(ctx, agent.Turn{Session: args[0], Compact: true})
	if err != nil {
		return err
	}
	pool, stopWorkers := startWorkers(ctx, a, false)
	defer stopWorkers()
	pool.wake()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		j, err := a.Queue.Get(ctx, jobID)
		if err != nil {
			return err
		}
		switch j.Status {
		case "done":
			fmt.Println(j.Result)
			return nil
		case "failed", "canceled":
			return fmt.Errorf("compaction %s: %s", j.Status, j.LastError)
		}
	}
}

func show(ctx context.Context, home string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: hk show SESSION")
	}
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	recs, err := a.Sessions.Messages(ctx, args[0])
	if err != nil {
		return err
	}
	for _, r := range recs {
		for _, b := range r.Message.Blocks {
			switch b.Type {
			case llm.Text:
				fmt.Printf("[%s] %s\n", r.Message.Role, b.Text)
			case llm.ToolUse:
				fmt.Printf("[tool call] %s %s\n", b.ToolName, b.Input)
			case llm.ToolResult:
				fmt.Printf("[tool result%s] %s\n", map[bool]string{true: " ERROR"}[b.IsError], firstLines(b.Text, 8))
			}
		}
	}
	return nil
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		return strings.Join(lines[:n], "\n") + "\n..."
	}
	return s
}

func doctor(ctx context.Context, home string) error {
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	fmt.Println("config: ok")
	configProblems := 0
	if s := cfg.Report.Summary(); s != "" {
		fmt.Println("config:", s)
	}
	for _, k := range cfg.Report.Unknown {
		fmt.Printf("config: UNKNOWN KEY %s (ignored; a typo, or a setting for a newer release)\n", k)
		configProblems++
	}
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	problems := configProblems
	for name, ep := range a.Router.Endpoints {

		have := 0
		for _, c := range ep.Credentials {
			if c.Key != "" {
				have++
			}
		}
		p := cfg.Providers[name]
		if p.Auth != "" {
			logins, _ := a.Auth.List()
			if _, ok := logins[p.Auth]; ok {
				fmt.Printf("provider %s: logged in (%s)\n", name, p.Auth)
			} else {
				fmt.Printf("provider %s: NOT LOGGED IN (run hk auth login %s)\n", name, p.Auth)
				problems++
			}
			continue
		}
		status := "ok"
		switch {
		case have == 0 && p.KeyOptional():
			status = "no key needed"
		case have == 0:
			status = "NO KEY SET; fix: " + keyFix(home, p.KeysEnv)
			problems++
		}
		fmt.Printf("provider %s: %d/%d keys present, %s\n", name, have, len(p.KeysEnv), status)
	}
	for _, db := range []struct {
		name string
		q    func() error
	}{
		{"control.db", func() error { return a.Control.QueryRowContext(ctx, "PRAGMA quick_check").Scan(new(string)) }},
		{"transcripts.db", func() error { return a.Transcripts.QueryRowContext(ctx, "PRAGMA quick_check").Scan(new(string)) }},
	} {
		if err := db.q(); err != nil {
			fmt.Printf("%s: %v\n", db.name, err)
			problems++
		} else {
			fmt.Printf("%s: ok\n", db.name)
		}
	}
	for _, name := range channelNames(a) {
		ch := cfg.Channels[name]
		var missing []string
		for _, env := range ch.SecretVars() {
			if a.Secret(env) == "" {
				missing = append(missing, env)
			}
		}
		if ch.Type == "signal" {
			if err := signalch.Check(ctx, ch.APIURL); err != nil {
				fmt.Printf("channel %s (signal): %v\n", name, err)
				problems++
				continue
			}
		}
		if len(missing) > 0 {
			fmt.Printf("channel %s (%s): MISSING %s; fix: add it to %s, or run hk setup channel %s\n", name, ch.Type,
				strings.Join(missing, ", "), filepath.Join(home, ".env"), ch.Type)
			problems++
		} else {
			fmt.Printf("channel %s (%s): credentials present, agent %s\n", name, ch.Type, ch.Agent)
		}
		if config.WebhookTypes[ch.Type] && cfg.HTTP != nil && cfg.HTTP.PublicURL != "" {
			fmt.Printf("  webhook URL for the platform: %s/channels/%s\n", cfg.HTTP.PublicURL, name)
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
			fmt.Printf("http %s: MISSING %s; fix: add it to %s\n", h.Listen, strings.Join(missing, ", "), filepath.Join(home, ".env"))
			problems++
		} else {
			fmt.Printf("http %s: ok, %d webhook(s)\n", h.Listen, len(h.Webhooks))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.MCP)) {
		m := cfg.MCP[name]
		var missing []string
		for _, env := range slices.Sorted(maps.Values(m.Env)) {
			if a.Secret(env) == "" {
				missing = append(missing, env)
			}
		}
		for _, spec := range slices.Sorted(maps.Values(m.Headers)) {
			for _, env := range mcp.HeaderVars(spec) {
				if a.Secret(env) == "" {
					missing = append(missing, env)
				}
			}
		}
		switch {
		case len(missing) > 0:
			fmt.Printf("mcp %s: MISSING %s; fix: add it to %s\n", name, strings.Join(missing, ", "), filepath.Join(home, ".env"))
			problems++
		case m.Auth == "oauth" && !hasMCPLogin(a, name):
			fmt.Printf("mcp %s: not signed in; run hk mcp login %s\n", name, name)
			problems++
		case len(m.Env)+len(m.Headers) == 0:
			fmt.Printf("mcp %s: no credentials needed (test the connection with hk mcp)\n", name)
		default:
			fmt.Printf("mcp %s: credentials present (test the connection with hk mcp)\n", name)
		}
	}
	problems += computerDoctor(cfg, os.Stdout)
	problems += permissionsDoctor(home, cfg, os.Stdout)
	if problems > 0 {
		return fmt.Errorf("%d problem(s) found; each one above says how to fix it", problems)
	}
	return nil
}

func mcpCmd(ctx context.Context, home string) error {
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	if len(a.Cfg.MCP) == 0 {
		fmt.Println("no MCP servers configured; see mcp_servers in config.yaml")
		return nil
	}
	a.MCP.Start(ctx)
	for _, st := range a.MCP.Status() {
		line := fmt.Sprintf("%-12s %-5s %-10s %d tools", st.Name, st.Transport, st.State, len(st.Tools))
		if st.Error != "" {
			line += "  " + st.Error
		}
		fmt.Println(line)
		for _, t := range st.Tools {
			fmt.Printf("  %-48s %-7s %s\n", t.Name, t.Risk, t.Remote)
		}
	}
	return nil
}

func channelNames(a *app.App) []string {
	names := make([]string, 0, len(a.Cfg.Channels))
	for n := range a.Cfg.Channels {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func channels(ctx context.Context, home string) error {
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	if a.Gateway == nil {
		fmt.Println("no channels configured; see the channels section in config.yaml")
		return nil
	}
	sts, err := a.Gateway.Store.Statuses(ctx)
	if err != nil {
		return err
	}
	byName := map[string]channel.Status{}
	for _, st := range sts {
		byName[st.Account] = st
	}
	for _, name := range channelNames(a) {
		ch := a.Cfg.Channels[name]
		st, ok := byName[name]
		if !ok {
			fmt.Printf("%-12s %-9s never started (is `hk serve` running?)\n", name, ch.Type)
			continue
		}
		line := fmt.Sprintf("%-12s %-9s %-10s since %s", name, ch.Type, st.State, st.Since.Format("2006-01-02 15:04:05"))
		if st.Detail != "" {
			line += "  " + st.Detail
		}
		if st.Restarts > 0 {
			line += fmt.Sprintf("  restarts=%d", st.Restarts)
		}
		if st.LastError != "" {
			line += "  last error: " + st.LastError
		}
		fmt.Println(line)
	}
	return nil
}

func pair(ctx context.Context, home string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: hk pair CODE")
	}
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	if a.Gateway == nil {
		return errors.New("no channels configured")
	}
	account, sender, name, err := a.Gateway.Store.Pair(ctx, args[0])
	if err != nil {
		return err
	}
	who := sender
	if name != "" {
		who = name + " (" + sender + ")"
	}
	fmt.Printf("%s can now talk to Habakkuk on %s.\n", who, account)
	return nil
}

func whatsappCmd(ctx context.Context, home string, args []string) error {
	if len(args) == 0 || args[0] != "link" {
		return errors.New("usage: hk whatsapp link [-account A] [-phone NUMBER]")
	}
	fs := flag.NewFlagSet("whatsapp link", flag.ExitOnError)
	account := fs.String("account", "", "whatsapp account from config (default: the only one)")
	phone := fs.String("phone", "", "phone number with country code; prints a pairing code instead of a QR")
	fs.Parse(args[1:])
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	if a.Gateway == nil {
		return errors.New("no channels configured")
	}
	var wa *whatsapp.Adapter
	for _, name := range channelNames(a) {
		if w, ok := a.Gateway.Adapters[name].(*whatsapp.Adapter); ok && (*account == "" || *account == name) {
			if wa != nil {
				return errors.New("several whatsapp accounts configured: pass -account")
			}
			wa = w
		}
	}
	if wa == nil {
		return errors.New("no whatsapp channel configured (add one with type: whatsapp)")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	return wa.Link(ctx, *phone, filepath.Join(home, "whatsapp-qr.png"), os.Stdout)
}

func schedules(ctx context.Context, home string, args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	st := a.Schedules
	switch sub {
	case "list", "ls":
		if err := a.Scheduler.Settle(ctx); err != nil {
			return err
		}
		list, err := st.List(ctx, "")
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("no schedules")
			return nil
		}
		loc := time.Local
		if a.Cfg.Timezone != "" {
			loc, _ = time.LoadLocation(a.Cfg.Timezone)
		}
		fmt.Println(schedule.Format(list, loc))
	case "add":
		fs := flag.NewFlagSet("schedules add", flag.ExitOnError)
		agentName := fs.String("agent", "", "agent to run")
		name := fs.String("name", "", "label")
		when := fs.String("when", "", `cron ("0 9 * * 1-5"), "@every 2h" or "at 2026-11-08 09:00"`)
		prompt := fs.String("prompt", "", "what to do each run (with -run: what the agent does with the output)")
		to := fs.String("to", "", "send results to CHANNEL:CHAT[:THREAD] (e.g. tg:123456)")
		deliver := fs.String("deliver", "", "same as -to")
		command := fs.String("run", "", "run this shell command instead of a prompt (no model unless -on-output agent)")
		onOutput := fs.String("on-output", "", "with -run: deliver (send stdout, default), agent (give it to the agent) or silent")
		timeout := fs.Duration("timeout", 0, "with -run: kill the command after this long (default 2m)")
		workdir := fs.String("workdir", "", "with -run: directory to run in (default the agent's workspace)")
		fs.Parse(args)
		ag, err := defaultAgent(a.Cfg, *agentName)
		if err != nil {
			return err
		}
		sc := schedule.Schedule{Agent: ag, Name: *name, Spec: *when, Prompt: *prompt,
			Command: *command, OnOutput: *onOutput, Timeout: *timeout, Workdir: *workdir}
		if sc.Workdir != "" {
			if sc.Workdir, err = filepath.Abs(sc.Workdir); err != nil {
				return err
			}
		}
		if *to != "" && *deliver != "" && *to != *deliver {
			return errors.New("-to and -deliver disagree; pass one")
		}
		target := *to
		if target == "" {
			target = *deliver
		}
		if target != "" {
			acct, rest, ok := strings.Cut(target, ":")
			chat, thread, _ := strings.Cut(rest, ":")
			if ch := a.Cfg.Channels[acct]; ch != nil && ch.Type == "matrix" && thread != "" {
				// A room id holds a colon itself: !room:server[:$thread].
				server, th, _ := strings.Cut(thread, ":")
				chat, thread = chat+":"+server, th
			}
			if _, known := a.Cfg.Channels[acct]; !ok || !known || chat == "" {
				return fmt.Errorf("-deliver %q: want CHANNEL:CHAT with a configured channel", target)
			}
			addr := channel.Address{Account: acct, Conversation: chat, Thread: thread}
			if a.Gateway != nil {
				if addr, err = a.Gateway.NormalizeTarget(addr); err != nil {
					return fmt.Errorf("-deliver %q: %w", target, err)
				}
			}
			sc.DeliverTo, _ = json.Marshal(addr)
		}
		created, err := st.Create(ctx, sc)
		if err != nil {
			return err
		}
		fmt.Printf("%s scheduled; first run %s\n", created.ID, schedule.When(created.NextRun, st.Location()))
		if note := schedule.ZoneNote(created.Spec, st.Location()); note != "" {
			fmt.Println(note)
		}
	case "rm", "delete":
		if len(args) != 1 {
			return errors.New("usage: hk schedules rm ID")
		}
		return st.Delete(ctx, args[0])
	case "pause", "resume":
		if len(args) != 1 {
			return fmt.Errorf("usage: hk schedules %s ID", sub)
		}
		return st.SetEnabled(ctx, args[0], sub == "resume")
	case "run":
		if len(args) != 1 {
			return errors.New("usage: hk schedules run ID")
		}
		jobID, err := a.Scheduler.RunNow(ctx, args[0])
		if err != nil {
			return err
		}
		fmt.Printf("queued %s; `hk serve` runs it (see `hk schedules list` for the result)\n", jobID)
	default:
		return fmt.Errorf("unknown schedules command %q", sub)
	}
	return nil
}
