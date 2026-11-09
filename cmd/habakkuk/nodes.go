package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mdp/qrterminal/v3"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/ops/i18n"
	"github.com/habakkuk-ai/habakkuk/internal/protocols/nodes"
)

// cliLang is the language of CLI messages: the config's, else the
// locale's.
func cliLang(cfg *config.Config) i18n.Printer {
	if cfg != nil && cfg.Language != "" {
		return i18n.Printer(i18n.Pick(cfg.Language))
	}
	return i18n.Printer(i18n.FromEnv())
}

// nodeCmd is `habakkuk node pair|run`.
func nodeCmd(ctx context.Context, home string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: habakkuk node pair [-ttl 10m] | habakkuk node run -server URL [-code CODE] -name NAME [-allow-run] [-allow-camera] [-dir DIR]")
	}
	switch args[0] {
	case "pair":
		return nodePair(ctx, home, args[1:], os.Stdout)
	case "run":
		return nodeRun(ctx, args[1:], os.Stdout)
	}
	return fmt.Errorf("unknown node command %q (pair or run)", args[0])
}

func openNodeStore(ctx context.Context, home string) (*config.Config, *nodes.Store, error) {
	cfg, err := config.Load(home)
	if err != nil {
		return nil, nil, err
	}
	if cfg.Nodes == nil {
		return nil, nil, errors.New("nodes are off: add a nodes section (and http) to config.yaml, e.g. `nodes: {}`")
	}
	st, err := nodes.OpenStore(ctx, filepath.Join(home, "nodes.db"))
	return cfg, st, err
}

// nodePair makes a one-time pairing code on the server.
func nodePair(ctx context.Context, home string, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("node pair", flag.ExitOnError)
	ttl := fs.Duration("ttl", 0, "how long the code works (default nodes.pair_ttl, 10m)")
	fs.Parse(args)
	cfg, st, err := openNodeStore(ctx, home)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	if *ttl <= 0 {
		*ttl = cfg.Nodes.PairCodeTTL()
	}
	code, _, err := st.NewCode(ctx, *ttl)
	if err != nil {
		return err
	}
	p := cliLang(cfg)
	base := cfg.NodesURL()
	fmt.Fprintln(w, p.T("node.pair.code", code, ttl.Round(time.Second)))
	fmt.Fprintln(w)
	fmt.Fprintln(w, p.T("node.pair.run"))
	fmt.Fprintf(w, "\n  habakkuk node run -server %s -code %s -name NAME\n\n", base, code)
	fmt.Fprintln(w, p.T("node.pair.scan"))
	link := base + "/app/pair.html#" + code
	fmt.Fprintln(w, link)
	if isTerminal(w) {
		qrterminal.GenerateHalfBlock(link, qrterminal.L, w)
	}
	return nil
}

// nodeState is what a device keeps after pairing.
type nodeState struct {
	Server string `json:"server"`
	Name   string `json:"name"`
	Secret string `json:"secret"`
}

// nodeRun pairs (with -code) and then serves this machine as a node.
func nodeRun(ctx context.Context, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("node run", flag.ExitOnError)
	server := fs.String("server", "", "the Habakkuk server's URL, e.g. https://habakkuk.example.com")
	code := fs.String("code", "", "a pairing code from `habakkuk node pair` (first run only)")
	name := fs.String("name", "", "this node's name, e.g. laptop")
	allowRun := fs.Bool("allow-run", false, "offer system.run: let Habakkuk run shell commands here (each still needs approval)")
	allowCamera := fs.Bool("allow-camera", false, "offer camera.snap (needs ffmpeg)")
	dir := fs.String("dir", "", "offer files.get and files.put inside this directory")
	statePath := fs.String("state", filepath.Join(config.DefaultHome(), "node.json"), "where this node keeps its server, name and secret")
	fs.Parse(args)
	p := cliLang(nil)

	st, err := loadNodeState(*statePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if *code != "" {
		if *server == "" || *name == "" {
			return errors.New("pairing needs -server, -code and -name")
		}
		secret, err := nodes.Pair(ctx, nil, *server, *code, *name, platform())
		if err != nil {
			return err
		}
		st = &nodeState{Server: strings.TrimRight(*server, "/"), Name: *name, Secret: secret}
		if err := saveNodeState(*statePath, st); err != nil {
			return err
		}
		fmt.Fprintln(w, p.T("node.run.paired", *name))
	}
	if st == nil {
		return fmt.Errorf("this machine is not paired yet: run `habakkuk node pair` on the server, then `habakkuk node run -server URL -code CODE -name NAME` here")
	}
	if (*server != "" && strings.TrimRight(*server, "/") != st.Server) || (*name != "" && *name != st.Name) {
		return fmt.Errorf("%s is paired as %s with %s; pass -code to pair again", *statePath, st.Name, st.Server)
	}
	if *dir != "" {
		abs, err := filepath.Abs(*dir)
		if err != nil {
			return err
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return fmt.Errorf("-dir %s is not a directory", *dir)
		}
		*dir = abs
	}
	caps := nodes.LocalCaps(nodes.LocalOptions{AllowRun: *allowRun, AllowCamera: *allowCamera, Dir: *dir})
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	c := &nodes.Client{
		Server: st.Server, Name: st.Name, Secret: st.Secret, Platform: platform(), Version: version, Caps: caps, Log: log,
		OnConnect: func() {
			fmt.Fprintln(w, p.T("node.run.connected", st.Server, st.Name, strings.Join(caps.Names(), ", ")))
		},
		OnDisconnect: func(err error, retry time.Duration) {
			fmt.Fprintln(w, p.T("node.run.lost", err, retry.Round(time.Second)))
		},
	}
	return c.Run(ctx)
}

func platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

func loadNodeState(path string) (*nodeState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st nodeState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &st, nil
}

// saveNodeState writes the node's secret, readable by its owner only.
func saveNodeState(path string, st *nodeState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	return store.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// nodesCmd is `habakkuk nodes [list|revoke NAME]`, run on the server.
func nodesCmd(ctx context.Context, home string, args []string, w io.Writer) error {
	cfg, st, err := openNodeStore(ctx, home)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	p := cliLang(cfg)
	if len(args) == 0 || args[0] == "list" {
		list, err := st.List(ctx)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintln(w, p.T("nodes.none"))
			return nil
		}
		for _, n := range list {
			seen := "never connected"
			if !n.LastSeen.IsZero() {
				seen = "last connected " + n.LastSeen.Local().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(w, "%-16s %-14s %s\n", n.Name, n.Platform, seen)
			if len(n.Caps) > 0 {
				fmt.Fprintf(w, "%-16s %s\n", "", strings.Join(n.Caps, ", "))
			}
		}
		return nil
	}
	if args[0] == "revoke" && len(args) == 2 {
		ok, err := st.Revoke(ctx, args[1])
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no node named %q", args[1])
		}
		fmt.Fprintln(w, p.T("nodes.revoked", args[1]))
		return nil
	}
	return errors.New("usage: habakkuk nodes [list | revoke NAME]")
}
