package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// tailscaleRunner runs the tailscale CLI; tests replace it.
type tailscaleRunner func(ctx context.Context, args ...string) ([]byte, error)

// exposeCmd runs `habakkuk expose tailscale|status|off`: it shares the HTTP
// listener with your tailnet through `tailscale serve`, which keeps it off
// the public internet (that would be `tailscale funnel`, which Habakkuk never
// runs).
func exposeCmd(ctx context.Context, home string, args []string, w io.Writer, run tailscaleRunner) error {
	const usage = "usage: habakkuk expose tailscale [-port 443] [-print] | habakkuk expose status | habakkuk expose off [-port 443]"
	if len(args) == 0 {
		return errors.New(usage)
	}
	sub := args[0]
	fs := flag.NewFlagSet("expose "+sub, flag.ContinueOnError)
	port := fs.Int("port", 443, "HTTPS port on the tailnet name")
	printOnly := fs.Bool("print", false, "print the command instead of running it")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if run == nil {
		run = runTailscale
	}
	switch sub {
	case "tailscale":
		cfg, err := config.Load(home)
		if err != nil {
			return err
		}
		target, err := exposeTarget(cfg)
		if err != nil {
			return err
		}
		cmd := []string{"serve", "--bg", "--https=" + strconv.Itoa(*port), target}
		fmt.Fprintf(w, "tailscale %s\n", strings.Join(cmd, " "))
		if *printOnly {
			fmt.Fprintln(w, "\nThis shares Habakkuk with devices on your tailnet only; nothing is made public.")
			return nil
		}
		out, err := run(ctx, cmd...)
		fmt.Fprint(w, string(out))
		if err != nil {
			return fmt.Errorf("tailscale serve: %w", err)
		}
		if name := tailnetName(ctx, run); name != "" {
			u := "https://" + name
			if *port != 443 {
				u += ":" + strconv.Itoa(*port)
			}
			fmt.Fprintf(w, "\nHabakkuk is at %s for devices on your tailnet (nothing public).\n", u)
			if cfg.HTTP.PublicURL == "" {
				fmt.Fprintf(w, "Set http.public_url: %s in config.yaml if webhook channels should use it.\n", u)
			}
		}
		return nil
	case "status":
		out, err := run(ctx, "serve", "status")
		fmt.Fprint(w, string(out))
		if err != nil {
			return fmt.Errorf("tailscale serve status: %w", err)
		}
		if strings.Contains(strings.ToLower(string(out)), "funnel on") {
			fmt.Fprintln(w, "\nWARNING: Funnel is on for this machine: something here is reachable from the public internet.")
		}
		return nil
	case "off":
		out, err := run(ctx, "serve", "--https="+strconv.Itoa(*port), "off")
		fmt.Fprint(w, string(out))
		if err != nil {
			return fmt.Errorf("tailscale serve off: %w", err)
		}
		fmt.Fprintln(w, "stopped sharing on the tailnet")
		return nil
	}
	return errors.New(usage)
}

// exposeTarget is the local URL tailscale serve proxies to. The API must
// have a token: proxied requests carry the tailnet host name, which the
// tokenless server refuses, and every device on the tailnet could reach it.
func exposeTarget(cfg *config.Config) (string, error) {
	h := cfg.HTTP
	if h == nil {
		return "", errors.New("no http section in config.yaml: add one (listen: 127.0.0.1:8484, token_env: HABAKKUK_HTTP_TOKEN) first")
	}
	if h.TokenEnv == "" {
		return "", errors.New("http.token_env is not set: give the API a token before sharing it on the tailnet")
	}
	host, port, err := net.SplitHostPort(h.Listen)
	if err != nil {
		return "", fmt.Errorf("http.listen %q: %w", h.Listen, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

// tailnetName is this machine's MagicDNS name, or "".
func tailnetName(ctx context.Context, run tailscaleRunner) string {
	out, err := run(ctx, "status", "--json")
	if err != nil {
		return ""
	}
	var st struct {
		Self struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	if json.Unmarshal(out, &st) != nil {
		return ""
	}
	return strings.TrimSuffix(st.Self.DNSName, ".")
}

func runTailscale(ctx context.Context, args ...string) ([]byte, error) {
	bin, err := exec.LookPath("tailscale")
	if err != nil {
		const mac = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
		if _, serr := os.Stat(mac); serr != nil {
			return nil, errors.New("the tailscale CLI is not installed or not on PATH (https://tailscale.com/download)")
		}
		bin = mac
	}
	return exec.CommandContext(ctx, bin, args...).CombinedOutput()
}
