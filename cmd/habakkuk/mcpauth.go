package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/habakkuk-ai/habakkuk/internal/app"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/llm/auth"
)

const mcpLoginUsage = `usage: hk mcp login [-device] [-no-browser] NAME
  Signs in to the MCP server NAME (auth: oauth in config.yaml) and keeps
  the tokens in $HABAKKUK_HOME/auth.json.
  -device      use the device flow (enter a code on any device), when the server offers it
  -no-browser  print the sign-in URL without opening a browser
On a machine without a browser, the device flow is used when the server
offers it; otherwise sign in on a machine with a browser and copy auth.json`

// mcpLoginCmd runs the OAuth sign-in for a remote MCP server.
func mcpLoginCmd(ctx context.Context, home string, args []string) error {
	fs := flag.NewFlagSet("mcp login", flag.ContinueOnError)
	device := fs.Bool("device", false, "use the device flow")
	noBrowser := fs.Bool("no-browser", false, "print the URL only")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, mcpLoginUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New(mcpLoginUsage)
	}
	name := fs.Arg(0)
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	s, err := oauthServer(a.Cfg, name)
	if err != nil {
		return err
	}
	l := auth.MCPLogin{
		Server: name, URL: s.URL, ClientID: s.ClientID, Scopes: s.Scopes, Port: s.CallbackPort, Device: *device,
		Say: func(line string) { fmt.Println(line) },
	}
	if s.ClientSecretEnv != "" {
		if l.ClientSecret = a.Secret(s.ClientSecretEnv); l.ClientSecret == "" {
			return fmt.Errorf("%s is not set in the environment or %s/.env", s.ClientSecretEnv, home)
		}
	}
	headless := !*device && !*noBrowser && !hasBrowser()
	if headless {
		l.Auto = true // the device flow if offered, else instructions
	} else if !*noBrowser {
		l.Open = openBrowser
	}
	e, err := a.Auth.LoginMCP(ctx, l)
	if errors.Is(err, auth.ErrNoBrowserFlow) && headless {
		return fmt.Errorf("%w.\nThis machine has no browser. Either run `hk mcp login %s` on a machine that has one "+
			"(with the same server in its config) and copy its %s/auth.json here (mode 0600), or forward a fixed callback_port "+
			"over SSH (ssh -L PORT:127.0.0.1:PORT) and run `hk mcp login -no-browser %s`", err, name, home, name)
	}
	if err != nil {
		return err
	}
	fmt.Printf("Signed in to %s", name)
	if e.Scope != "" {
		fmt.Printf(" (scopes: %s)", e.Scope)
	}
	if e.RefreshToken == "" {
		fmt.Print("; the server gave no refresh token, so sign in again when it expires")
	}
	fmt.Printf(".\nCheck it with hk mcp; a running hk serve picks the login up on its next connection.\n")
	return nil
}

// mcpLogoutCmd forgets a server's OAuth login.
func mcpLogoutCmd(ctx context.Context, home string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: hk mcp logout NAME")
	}
	found, err := auth.NewStore(home).Delete(ctx, auth.MCPLoginName(args[0]))
	if err != nil {
		return err
	}
	if !found {
		fmt.Printf("no login stored for %s\n", args[0])
		return nil
	}
	fmt.Printf("Removed the login for %s from %s/auth.json. The server may still list Habakkuk as an authorized app; revoke it there too.\n", args[0], home)
	return nil
}

func oauthServer(cfg *config.Config, name string) (*config.MCPServer, error) {
	s, ok := cfg.MCP[name]
	switch {
	case !ok || s == nil:
		return nil, fmt.Errorf("no MCP server %q in config.yaml", name)
	case s.Auth != "oauth":
		return nil, fmt.Errorf("MCP server %s does not use OAuth; add `auth: oauth` to it in config.yaml", name)
	}
	return s, nil
}

// hasBrowser guesses whether a browser can open on this machine: not over
// SSH, and on Linux and the BSDs only with a display.
func hasBrowser() bool {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return false
	}
	switch runtime.GOOS {
	case "darwin", "windows":
		return true
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// hasMCPLogin reports whether auth.json holds a login for the server.
func hasMCPLogin(a *app.App, server string) bool {
	logins, err := a.Auth.List()
	if err != nil {
		return false
	}
	_, ok := logins[auth.MCPLoginName(server)]
	return ok
}
