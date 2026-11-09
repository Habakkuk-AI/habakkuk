package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/checkpoint"
)

func checkpointStore(home string, cfg *config.Config) *checkpoint.Store {
	return &checkpoint.Store{Root: filepath.Join(home, "checkpoints"),
		AgentMaxBytes: func(name string) int64 {
			if ac := cfg.Agents[name]; ac != nil {
				return int64(ac.CheckpointMaxMB) << 20
			}
			return 0
		}}
}

// checkpointsCmd lists the workspace snapshots taken before turns changed
// files, newest first.
func checkpointsCmd(ctx context.Context, home string, args []string) error {
	fs := flag.NewFlagSet("checkpoints", flag.ExitOnError)
	agentName := fs.String("agent", "", "only this agent (default all)")
	n := fs.Int("n", 20, "how many per agent")
	fs.Parse(args)
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	names := cfg.AgentNames()
	if *agentName != "" {
		if cfg.Agents[*agentName] == nil {
			return fmt.Errorf("unknown agent %q", *agentName)
		}
		names = []string{*agentName}
	}
	st := checkpointStore(home, cfg)
	found := false
	for _, name := range names {
		list, err := st.List(ctx, name, *n)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			continue
		}
		found = true
		fmt.Printf("%s (%s):\n", name, cfg.WorkspaceDir(name))
		for _, cp := range list {
			task := cp.Task
			if len(task) > 60 {
				task = task[:60] + "..."
			}
			fmt.Printf("  %s  %s  %-8s  %-14s  %s\n", cp.ID, cp.Time.Format("2006-01-02 15:04"), cp.Kind, orDash(cp.Session), task)
		}
	}
	if !found {
		fmt.Println("no checkpoints")
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// rollbackCmd restores an agent's workspace, or one file in it, to a
// checkpoint, after showing what would change. The state it replaces is
// snapshotted first, so a rollback can be rolled back.
func rollbackCmd(ctx context.Context, home string, args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ExitOnError)
	agentName := fs.String("agent", "", "the agent whose checkpoint it is (default: the one that has it)")
	yes := fs.Bool("y", false, "restore without asking")
	fs.Parse(args)
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return errors.New("usage: habakkuk rollback [-agent A] [-y] ID [PATH]")
	}
	id, path := strings.ToLower(fs.Arg(0)), fs.Arg(1)
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	st := checkpointStore(home, cfg)
	name := *agentName
	if name == "" {
		for _, n := range cfg.AgentNames() {
			if st.Has(ctx, n, id) {
				if name != "" {
					return fmt.Errorf("checkpoint %s matches agents %s and %s; pick one with -agent", id, name, n)
				}
				name = n
			}
		}
		if name == "" {
			return fmt.Errorf("no agent has checkpoint %s (see habakkuk checkpoints)", id)
		}
	} else if cfg.Agents[name] == nil {
		return fmt.Errorf("unknown agent %q", name)
	}
	ws := cfg.WorkspaceDir(name)
	stat, err := st.DiffStat(ctx, name, ws, id, path)
	if err != nil {
		return err
	}
	what := ws
	if path != "" {
		what = path + " in " + ws
	}
	fmt.Printf("Restoring %s to checkpoint %s changes:\n%s\n", what, id, stat)
	if stat == "no changes" {
		return nil
	}
	if !*yes {
		fmt.Print("Restore? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Println("Nothing changed.")
			return nil
		}
	}
	if _, err := st.Restore(ctx, name, ws, id, path, checkpoint.Meta{Kind: checkpoint.KindRollback, Task: "rollback to " + id}); err != nil {
		return err
	}
	fmt.Println("Restored. The replaced state is the newest checkpoint, so this can be rolled back too.")
	return nil
}
