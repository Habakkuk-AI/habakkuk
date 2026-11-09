package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/checkpoint"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
	"github.com/habakkuk-ai/habakkuk/internal/tools/sandbox"
)

// Work is what exec and the file tools need beyond the registry: the
// background process table, each agent's command backend, and checkpoints.
type Work struct {
	Procs       *tools.Procs
	Backends    map[string]tools.Backend // by agent; absent means this machine
	Checkpoints *checkpoint.Store
	// AllowPrivateURLs is tools.allow_private_urls (fetch_url's SSRF guard).
	AllowPrivateURLs bool
}

// openWork sets up background processes, sandboxes and checkpoints. It runs
// after the channels are open, so /undo and exit notices can reach chats.
func (a *App) openWork(home string) error {
	procs, err := tools.NewProcs(filepath.Join(home, "procs"), a.Log)
	if err != nil {
		return err
	}
	procs.Notify = a.notifyProcess
	a.closers = append(a.closers, procs.Close)
	w := &Work{Procs: procs, Backends: map[string]tools.Backend{}, AllowPrivateURLs: a.Cfg.Tools.AllowPrivateURLs}
	w.Checkpoints = &checkpoint.Store{Root: filepath.Join(home, "checkpoints"), Log: a.Log,
		Enabled: func(name string) bool {
			ac := a.Cfg.Agents[name]
			return ac != nil && ac.Checkpoints != nil && *ac.Checkpoints
		},
		AgentMaxBytes: func(name string) int64 {
			if ac := a.Cfg.Agents[name]; ac != nil {
				return int64(ac.CheckpointMaxMB) << 20
			}
			return 0
		}}
	for _, name := range a.Cfg.AgentNames() {
		ac := a.Cfg.Agents[name]
		switch ac.Sandbox {
		case "docker":
			w.Backends[name] = dockerBackend(name, a.Cfg.WorkspaceDir(name), ac.Docker)
		case "ssh":
			s, err := a.sshBackend(home, name, ac.SSH)
			if err != nil {
				// Never fall back to running here what was meant for the
				// remote host: commands fail with the reason instead.
				a.Log.Warn("ssh sandbox unavailable", "agent", name, "err", err)
				w.Backends[name] = unavailable{err: fmt.Errorf("the ssh sandbox is not set up: %w", err)}
				continue
			}
			w.Backends[name] = s
			a.closers = append(a.closers, s.Close)
		}
	}
	a.Work = w
	if a.Gateway != nil {
		a.Gateway.Undo = a.undo
	}
	return nil
}

// unavailable is a remote backend that could not be set up.
type unavailable struct{ err error }

func (u unavailable) Command(context.Context, string, string, bool) (*exec.Cmd, func(), error) {
	return nil, nil, u.err
}
func (unavailable) Remote() bool     { return true }
func (unavailable) Describe() string { return "a remote host that is not set up" }

// equip gives a turn's tool environment its backend, process table and
// checkpoints.
func (w *Work) equip(env *tools.Env) {
	if w == nil {
		return
	}
	env.Procs = w.Procs
	env.AllowPrivateURLs = w.AllowPrivateURLs
	if b, ok := w.Backends[env.Agent]; ok {
		env.Backend = b
	}
	if env.Backend == nil || !env.Backend.Remote() {
		env.Checkpoints = tools.OncePerTurn(w.Checkpoints)
	}
}

func dockerBackend(name, workspace string, dc *config.DockerSandbox) *sandbox.Docker {
	d := &sandbox.Docker{Agent: name, Workspace: workspace, Image: dc.Image, Network: *dc.Network,
		CPUs: dc.CPUs, Memory: dc.Memory, User: dc.User}
	switch dc.User {
	case "":
		if os.Getuid() >= 0 { // -1 on Windows: keep the image's user
			d.User = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
		}
	case "image":
		d.User = ""
	}
	return d
}

// sshBackend builds an agent's ssh backend. key_env holds either a private
// key, which is written to $HABAKKUK_HOME/ssh/<agent>.key (mode 0600), or the
// path of one.
func (a *App) sshBackend(home, name string, sc *config.SSHSandbox) (*sandbox.SSH, error) {
	dir := filepath.Join(home, "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &sandbox.SSH{Host: sc.Host, Port: sc.Port, User: sc.User, Workdir: sc.Workdir,
		Workspace: a.Cfg.WorkspaceDir(name), ControlDir: dir}
	// Unix socket paths are short (about 104 bytes); %C adds 40.
	if len(dir) > 60 {
		s.ControlDir = filepath.Join(os.TempDir(), fmt.Sprintf("habakkuk-ssh-%d", os.Getuid()))
		if err := os.MkdirAll(s.ControlDir, 0o700); err != nil {
			return nil, err
		}
	}
	if sc.KeyEnv != "" {
		key := a.Secret(sc.KeyEnv)
		switch {
		case key == "":
			return nil, fmt.Errorf("ssh key_env %s is not set", sc.KeyEnv)
		case strings.HasPrefix(strings.TrimSpace(key), "-----BEGIN"):
			s.KeyFile = filepath.Join(dir, name+".key")
			body := strings.TrimSpace(strings.ReplaceAll(key, `\n`, "\n")) + "\n"
			if err := os.WriteFile(s.KeyFile, []byte(body), 0o600); err != nil {
				return nil, err
			}
		default:
			if rest, ok := strings.CutPrefix(key, "~/"); ok {
				if h, err := os.UserHomeDir(); err == nil {
					key = filepath.Join(h, rest)
				}
			}
			s.KeyFile = key
		}
	}
	return s, nil
}

// notifyProcess starts a turn in session saying a background process ended.
// The turn ID derives from the process, so it is queued once.
func (a *App) notifyProcess(ctx context.Context, session, procID, text string) error {
	if _, err := a.Runner.SubmitTurn(ctx, agent.Turn{ID: "turn_" + procID, Session: session, Text: text,
		Origin: a.here(ctx, session), Background: true}); err != nil {
		return err
	}
	if a.Runner.Wake != nil {
		a.Runner.Wake()
	}
	return nil
}

// undo answers the /undo chat command: it puts the session's workspace back
// to before the session's latest turn that changed it.
func (a *App) undo(ctx context.Context, sessionID string) (string, error) {
	sess, err := a.Sessions.Get(ctx, sessionID)
	if err != nil {
		return "", err
	}
	spec, ok := a.Runner.Agents[sess.Agent]
	ac := a.Cfg.Agents[sess.Agent]
	switch {
	case !ok || ac == nil:
		return "", fmt.Errorf("agent %q is not configured", sess.Agent)
	case ac.Sandbox == "ssh":
		return "My commands run on a remote host, so I keep no checkpoints to undo.", nil
	case !*ac.Checkpoints:
		return "Checkpoints are off for this agent, so there is nothing to undo.", nil
	}
	cp, stat, err := a.Work.Checkpoints.Undo(ctx, sess.Agent, spec.Workspace, sessionID)
	switch {
	case errors.Is(err, checkpoint.ErrNothing):
		return "I have no changes to undo in this conversation.", nil
	case errors.Is(err, checkpoint.ErrNoGit):
		return "Checkpoints need git, which is not installed here.", nil
	case err != nil:
		return "", err
	}
	return fmt.Sprintf("Undid the file changes from my turn %q (checkpoint %s):\n```\n%s\n```", cp.Task, cp.ID, stat), nil
}
