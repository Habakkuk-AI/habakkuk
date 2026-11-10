package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/checkpoint"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/retro"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/ops/logs"
)

// openRetro sets up retros for every agent: on the retro.every timer in
// `hk serve`, and on demand from `hk retro` and /retro. A retro
// uses retro.model first when set, then the agent's own models.
func (a *App) openRetro() {
	rc := a.Cfg.Retro
	agents := map[string]retro.Agent{}
	for name, spec := range a.Runner.Agents {
		if len(spec.Chain) == 0 {
			continue
		}
		chain := slices.Clone(spec.Chain)
		if rc != nil && rc.Model != "" {
			ref, _ := config.ParseModelRef(rc.Model) // validated at load
			first := llm.Target{Endpoint: ref.Provider, Model: ref.Model}
			chain = append([]llm.Target{first}, slices.DeleteFunc(chain, func(t llm.Target) bool { return t == first })...)
		}
		models := []string{spec.Chain[0].String()}
		if len(spec.FastChain) > 0 {
			models = append(models, spec.FastChain[0].String())
		}
		var fallbacks []string
		for _, t := range spec.Chain[1:] {
			fallbacks = append(fallbacks, t.String())
		}
		ag := retro.Agent{Name: name, Chain: chain, Models: models, Fallbacks: fallbacks,
			Deliver: a.Cfg.RetroDeliver(name), Dir: spec.Dir}
		if ac := a.Cfg.Agents[name]; ac != nil {
			ag.Home = ac.Home
		}
		agents[name] = ag
	}
	spec, _ := a.Cfg.RetroSpec()
	var only []string
	if rc != nil {
		only = rc.Agents
	}
	red := logs.NewRedactor(io.Discard)
	red.Add(a.SecretValues()...)
	a.Retro = &retro.Runner{
		Home: a.Cfg.Home, Mem: a.Memory, Skills: a.Skills, Queue: a.Queue, Models: a.Router,
		Sources: retro.Sources{
			Control: a.Control, Transcripts: a.Transcripts, Usage: a.Usage, Pricer: a.Pricer,
			History: a.Memory.History(), LogHome: a.Cfg.Home, Undos: a.retroUndos, Redact: red.Redact,
		},
		Agents: agents, Apply: a.Cfg.RetroApply(), Spec: spec, Only: only, Location: a.Runner.Location,
		Schedules: a.Schedules, Deliver: a.retroDeliver, Log: a.Log,
	}
}

// retroDeliver queues a retro summary to a `hk send` target.
func (a *App) retroDeliver(ctx context.Context, id, target, text string) error {
	if a.Gateway == nil {
		return errors.New("no channels configured")
	}
	addr, err := channel.ParseTarget(target)
	if err != nil {
		return err
	}
	return a.Gateway.SendText(ctx, id, addr, text)
}

// retroUndos lists an agent's /undo uses from its workspace checkpoints.
func (a *App) retroUndos(ctx context.Context, agentName string) ([]retro.Undo, error) {
	if a.Work == nil || a.Work.Checkpoints == nil {
		return nil, nil
	}
	list, err := a.Work.Checkpoints.List(ctx, agentName, 500)
	if err != nil {
		return nil, err
	}
	var out []retro.Undo
	for _, cp := range list {
		if cp.Kind == checkpoint.KindUndo {
			out = append(out, retro.Undo{ID: cp.ID, At: cp.Time, Session: cp.Session, Task: cp.Task})
		}
	}
	return out, nil
}

// retroPool runs retro jobs one at a time.
func (a *App) retroPool() *jobs.Pool {
	return &jobs.Pool{
		Queue: a.Queue, Concurrency: 1, Lease: a.Cfg.Workers.Lease, Log: a.Log,
		Handlers: map[string]jobs.Handler{retro.Kind: a.Retro.Handle},
	}
}

// RunRetro runs a retro of an agent's last days now and waits for it. The
// retro is not delivered: the caller shows it. In a process that does not
// serve, a pool is started for the wait; `hk serve` may pick the job
// up instead, which is just as good.
func (a *App) RunRetro(ctx context.Context, agentName string, days int) (*retro.Retro, error) {
	if days <= 0 {
		days = 7
	}
	from, to := retro.Recent(days, time.Now())
	id, err := a.Retro.Enqueue(ctx, agentName, from, to, true)
	if err != nil {
		return nil, err
	}
	if !a.serving {
		pool := a.retroPool()
		pctx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); pool.Run(pctx) }()
		defer func() { cancel(); wg.Wait() }()
		pool.Wake()
	}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		j, err := a.Queue.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		switch j.Status {
		case jobs.Done:
			if strings.HasPrefix(j.Result, "skipped:") {
				return nil, errors.New(j.Result)
			}
			return a.Retro.Get(ctx, id)
		case jobs.Failed, jobs.Canceled:
			return nil, fmt.Errorf("retro %s %s: %s", id, j.Status, j.LastError)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("retro %s is still running (%w); hk retro show %s has it once done", id, ctx.Err(), id)
		case <-tick.C:
		}
	}
}

// retroCommand is /retro: run one for this conversation's agent, or apply
// or reject a proposal of its latest retro.
func (a *App) retroCommand(ctx context.Context, session, arg string) string {
	sess, err := a.Sessions.Get(ctx, session)
	if err != nil {
		return "Couldn't find this conversation: " + err.Error()
	}
	agentName := sess.Agent
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		ret, err := a.RunRetro(ctx, agentName, 7)
		if err != nil {
			return "The retro did not finish: " + err.Error()
		}
		if ret.Plan.Quiet() {
			return fmt.Sprintf("Retro for %s: nothing notable in the last 7 days. Report: %s", agentName, ret.Report)
		}
		return retro.ChatSummary(ret)
	}
	usage := "Usage: /retro (run one now), /retro apply N [N...], /retro reject N"
	verb := strings.ToLower(fields[0])
	if (verb != "apply" && verb != "reject") || len(fields) < 2 {
		return usage
	}
	var ns []int
	for _, f := range fields[1:] {
		n, err := strconv.Atoi(strings.Trim(f, "#,."))
		if err != nil {
			return usage
		}
		ns = append(ns, n)
	}
	ret, err := a.Retro.Resolve(ctx, agentName, "latest")
	if err != nil {
		return err.Error()
	}
	var b strings.Builder
	switch verb {
	case "apply":
		out, err := a.Retro.ApplyActions(ctx, ret.ID, ns, history.Actor{By: history.ByUser, Tool: "/retro apply", Session: session})
		if err != nil {
			return "Couldn't apply that: " + err.Error()
		}
		for _, o := range out {
			fmt.Fprintf(&b, "%d. %s %s: %s. %s\n", o.N, o.Kind, o.Target, o.Status, o.Result)
		}
	case "reject":
		for _, n := range ns {
			o, err := a.Retro.Reject(ctx, ret.ID, n)
			if err != nil {
				fmt.Fprintf(&b, "%d: %v\n", n, err)
				continue
			}
			fmt.Fprintf(&b, "%d. %s %s: rejected.\n", o.N, o.Kind, o.Target)
		}
	}
	return strings.TrimSpace(b.String())
}
