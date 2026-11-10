package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/habakkuk-ai/habakkuk/internal/remote"
)

const remoteUsage = `Usage:
  hk remote add NAME TARGET [-identity PATH] [-home DIR] [-bin PATH] [-token-env NAME] [-default] [-force]
                       TARGET is ssh://user@host[:port] (any command) or https://host (status and chat)
  hk remote list       the remotes; * marks the default
  hk remote rm NAME
  hk remote default NAME|none
Then: hk -r NAME COMMAND [ARGS...] runs COMMAND there (docs/interfaces/remote-cli.md).
`

// remotesPath finds the registry; tests point it at a temporary file.
var remotesPath = remote.DefaultPath

// globals are the flags before the command: -r/--remote NAME, -q/--quiet
// and --yes-remote.
type globals struct {
	remote    string
	quiet     bool
	yesRemote bool
}

// parseGlobals reads the global flags off the front of args. The first
// argument that is not one of them is the command.
func parseGlobals(args []string) (globals, []string, error) {
	var g globals
	for len(args) > 0 {
		a := args[0]
		name, val, hasVal := strings.Cut(a, "=")
		switch name {
		case "-r", "--remote", "-remote":
			if !hasVal {
				if len(args) < 2 || args[1] == "" || strings.HasPrefix(args[1], "-") {
					return g, nil, fmt.Errorf("%s needs a remote name", name)
				}
				val, args = args[1], args[1:]
			}
			if val == "" {
				return g, nil, fmt.Errorf("%s needs a remote name", name)
			}
			g.remote = val
		case "-q", "--quiet", "-quiet":
			if hasVal {
				return g, nil, fmt.Errorf("%s takes no value", name)
			}
			g.quiet = true
		case "--yes-remote", "-yes-remote":
			if hasVal {
				return g, nil, fmt.Errorf("%s takes no value", name)
			}
			g.yesRemote = true
		default:
			return g, args, nil
		}
		args = args[1:]
	}
	return g, args, nil
}

// dispatchRemote runs before the local commands. It handles `hk remote`
// and sends the command to a remote when one is selected; done is true
// when it did (code is then the exit code). Otherwise it returns the
// arguments without the global flags, for the local command.
func dispatchRemote(argv []string, std remote.Stdio, getenv func(string) string) (rest []string, code int, done bool) {
	g, rest, err := parseGlobals(argv)
	if err != nil {
		fmt.Fprintln(std.Err, "error:", err)
		return nil, 2, true
	}
	if len(rest) == 0 {
		return rest, 0, false
	}
	if rest[0] == "remote" {
		if err := remoteCmd(rest[1:], std.Out, std.Err); err != nil {
			fmt.Fprintln(std.Err, "error:", err)
			return nil, 1, true
		}
		return nil, 0, true
	}
	env := getenv("HABAKKUK_REMOTE")
	path, err := remotesPath()
	var reg *remote.Registry
	if err == nil {
		reg, err = remote.Load(path)
	}
	if err != nil {
		if g.remote == "" && env == "" {
			return rest, 0, false // no registry and none asked for: local
		}
		fmt.Fprintln(std.Err, "error:", err)
		return nil, 1, true
	}
	name, rem, ok, err := remote.Select(g.remote, env, reg)
	if err != nil {
		fmt.Fprintln(std.Err, "error:", err)
		return nil, 2, true
	}
	if !ok {
		return rest, 0, false
	}
	return nil, runRemote(name, rem, g, rest, std), true
}

// runRemote runs one command on a remote and returns the exit code.
func runRemote(name string, rem remote.Remote, g globals, args []string, std remote.Stdio) int {
	if why := remote.Risky(args); why != "" && !g.yesRemote {
		fmt.Fprintf(std.Err, "error: not running %q on remote %s: %s.\nRun it on the server, or confirm: hk -r %s --yes-remote %s\n",
			args[0], name, why, name, strings.Join(args, " "))
		return 2
	}
	t, err := remote.ParseTarget(rem.Target)
	if err != nil {
		fmt.Fprintf(std.Err, "error: remote %s: %v\n", name, err)
		return 2
	}
	if !g.quiet && remote.IsTerminal(std.Err) {
		banner := "(remote: " + name + ")"
		if os.Getenv("NO_COLOR") == "" {
			banner = "\x1b[2m" + banner + "\x1b[0m"
		}
		fmt.Fprintln(std.Err, banner)
	}
	// Ctrl-C belongs to the remote command (ssh passes it on, chat stops):
	// hk stays to report its exit code.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var code int
	switch t.Kind {
	case remote.SSH:
		tty := remote.WantTTY(std.In, std.Out, remote.IsTerminal)
		code, err = remote.RunSSH(rem, args, tty, std)
	default:
		code, err = remote.RunHTTP(ctx, name, rem, args, std, remote.IsTerminal(std.In))
	}
	if err != nil {
		fmt.Fprintln(std.Err, "error:", err)
	}
	return code
}

// remoteCmd is `hk remote`: add, list, rm and default. It always runs on
// this machine.
func remoteCmd(args []string, out, errOut io.Writer) error {
	path, err := remotesPath()
	if err != nil {
		return err
	}
	reg, err := remote.Load(path)
	if err != nil {
		return err
	}
	verb := "list"
	if len(args) > 0 {
		verb, args = args[0], args[1:]
	}
	switch verb {
	case "list", "ls":
		if len(args) > 0 {
			return errors.New("usage: hk remote list")
		}
		listRemotes(reg, out)
		return nil
	case "add":
		fs := flag.NewFlagSet("remote add", flag.ContinueOnError)
		fs.SetOutput(errOut)
		fs.Usage = func() { fmt.Fprint(errOut, remoteUsage) }
		var rem remote.Remote
		fs.StringVar(&rem.Identity, "identity", "", "ssh: private key file")
		fs.StringVar(&rem.Home, "home", "", "ssh: HABAKKUK_HOME on the server (default: the server's environment)")
		fs.StringVar(&rem.Bin, "bin", "", "ssh: habakkuk binary on the server (default habakkuk, then ~/.local/bin/habakkuk)")
		fs.StringVar(&rem.TokenEnv, "token-env", "", "https: the variable holding the server's token")
		makeDefault := fs.Bool("default", false, "make it the default remote")
		force := fs.Bool("force", false, "replace a remote with the same name")
		pos, err := parseInterspersed(fs, args)
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(pos) != 2 {
			return errors.New("usage: hk remote add NAME TARGET [flags] (hk remote help)")
		}
		rem.Target = pos[1]
		if err := reg.Add(pos[0], rem, *force); err != nil {
			return err
		}
		if *makeDefault {
			reg.Default = pos[0]
		}
		if err := reg.Save(path); err != nil {
			return err
		}
		fmt.Fprintf(out, "Added remote %s: %s\n", pos[0], reg.Remotes[pos[0]].Target)
		if reg.Default == pos[0] {
			fmt.Fprintf(out, "It is the default: plain hk commands now run there (hk -r local runs here).\n")
		} else {
			fmt.Fprintf(out, "Try: hk -r %s status\n", pos[0])
		}
		return nil
	case "rm", "remove":
		if len(args) != 1 {
			return errors.New("usage: hk remote rm NAME")
		}
		if err := reg.Remove(args[0]); err != nil {
			return err
		}
		if err := reg.Save(path); err != nil {
			return err
		}
		fmt.Fprintf(out, "Removed remote %s.\n", args[0])
		return nil
	case "default":
		switch len(args) {
		case 0:
			if reg.Default == "" {
				fmt.Fprintln(out, "No default remote: commands run on this machine.")
			} else {
				fmt.Fprintln(out, reg.Default)
			}
			return nil
		case 1:
		default:
			return errors.New("usage: hk remote default NAME|none")
		}
		if err := reg.SetDefault(args[0]); err != nil {
			return err
		}
		if err := reg.Save(path); err != nil {
			return err
		}
		if reg.Default == "" {
			fmt.Fprintln(out, "No default remote: commands run on this machine.")
		} else {
			fmt.Fprintf(out, "Default remote: %s. Plain hk commands now run there; hk -r local runs here.\n", reg.Default)
		}
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(out, remoteUsage)
		return nil
	}
	return fmt.Errorf("unknown remote command %q\n\n%s", verb, remoteUsage)
}

func listRemotes(reg *remote.Registry, out io.Writer) {
	if len(reg.Remotes) == 0 {
		fmt.Fprintln(out, "No remotes. Add one: hk remote add srv ssh://you@server")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "\tNAME\tTARGET\tOPTIONS")
	for _, n := range reg.Names() {
		r := reg.Remotes[n]
		mark := ""
		if n == reg.Default {
			mark = "*"
		}
		var opts []string
		for _, o := range [][2]string{{"identity", r.Identity}, {"home", r.Home}, {"bin", r.Bin}, {"token-env", r.TokenEnv}} {
			if o[1] != "" {
				opts = append(opts, o[0]+"="+o[1])
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", mark, n, r.Target, strings.Join(opts, " "))
	}
	_ = tw.Flush()
	if env := os.Getenv("HABAKKUK_REMOTE"); env != "" {
		fmt.Fprintf(out, "HABAKKUK_REMOTE=%s overrides the default.\n", env)
	}
}

// parseInterspersed parses flags that may come before, between or after
// the positional arguments, and returns the positional ones.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

func remoteStdio() remote.Stdio { return remote.Stdio{In: os.Stdin, Out: os.Stdout, Err: os.Stderr} }
