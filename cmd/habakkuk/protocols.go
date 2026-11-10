package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/protocols/acp"
)

// mcpServe runs Habakkuk as an MCP server on stdin and stdout. Its turns run
// here, or in `hk serve` if that claims them first.
func mcpServe(ctx context.Context, home string, args []string) error {
	fs := flag.NewFlagSet("mcp serve", flag.ExitOnError)
	fs.Parse(args)
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	_, stopWorkers := startWorkers(ctx, a, false)
	defer stopWorkers()
	a.Log.Info("mcp server on stdio")
	return a.MCPServer(version).ServeStdio(ctx)
}

// acpCmd speaks the Agent Client Protocol on stdin and stdout for a code
// editor. Editor turns run only in this process, since their file access
// and approvals go through the editor.
func acpCmd(ctx context.Context, home string, args []string) error {
	fs := flag.NewFlagSet("acp", flag.ExitOnError)
	agentName := fs.String("agent", "", "agent the editor talks to")
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
	ed, err := acp.New(a.Runner, a.Tools, name, version, a.Log)
	if err != nil {
		return err
	}
	pools, stopWorkers := startWorkers(ctx, a, false) // sub-turns, so delegation works without `hk serve`
	defer stopWorkers()
	pool := &jobs.Pool{Queue: a.Queue, Handlers: map[string]jobs.Handler{acp.KindTurn: ed.Handle},
		Concurrency: a.Cfg.Workers.Concurrency, Lease: a.Cfg.Workers.Lease, Log: a.Log}
	ed.SetWake(func() { pool.Wake(); pools.wake() })
	pctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); pool.Run(pctx) }()
	defer func() { cancel(); wg.Wait() }()
	go func() { <-ctx.Done(); os.Stdin.Close() }() // unblock the reader on a signal
	a.Log.Info("acp on stdio", "agent", name)
	return ed.Serve(ctx, os.Stdin, os.Stdout)
}

// sendCmd queues a message or a file to a chat without running a turn.
// `hk serve` delivers it, durably, like a reply.
func sendCmd(ctx context.Context, home string, args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	to := fs.String("to", "", "CHANNEL:CHAT[:THREAD], e.g. telegram:123456")
	file := fs.String("file", "", "send this file; TEXT becomes its caption")
	fs.Parse(args)
	text := strings.Join(fs.Args(), " ")
	if text == "-" {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
		if err != nil {
			return err
		}
		text = string(data)
	}
	if *to == "" || (text == "" && *file == "") {
		return errors.New("usage: hk send -to CHANNEL:CHAT [-file PATH] TEXT (TEXT - reads stdin)")
	}
	addr, err := channel.ParseTarget(*to)
	if err != nil {
		return err
	}
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	if a.Gateway == nil {
		return errors.New("no channels configured")
	}
	if _, ok := a.Cfg.Channels[addr.Account]; !ok {
		return fmt.Errorf("no channel %q in config", addr.Account)
	}
	id := jobs.NewID("send")
	if *file != "" {
		info, err := os.Stat(*file)
		if err != nil {
			return err
		}
		if info.Size() > 100<<20 {
			return fmt.Errorf("%s is larger than 100 MB", *file)
		}
		data, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		err = a.Gateway.SendFile(ctx, id, addr, filepath.Base(*file), data, text)
		if err != nil {
			return err
		}
	} else if err := a.Gateway.SendText(ctx, id, addr, text); err != nil {
		return err
	}
	fmt.Printf("queued %s for %s; `hk serve` delivers it\n", id, *to)
	return nil
}
