package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// dashboardCmd prints the dashboard's address. With "link" it prints a
// sign-in link: opening it signs that browser in with the dashboard token,
// which travels after the # and so never reaches a server log.
func dashboardCmd(ctx context.Context, home string, args []string) error {
	a, closeApp, err := open(ctx, home)
	if err != nil {
		return err
	}
	defer closeApp()
	h := a.Cfg.HTTP
	if h == nil || h.Listen == "" {
		return errors.New("there is no dashboard: add an http section to config.yaml (hk setup adds one)")
	}
	base := strings.TrimRight(h.PublicURL, "/")
	if base == "" {
		base = "http://" + h.Listen
	}
	base += "/app/"
	if len(args) == 0 || args[0] != "link" {
		fmt.Println(base)
		return nil
	}
	if h.TokenEnv == "" {
		fmt.Println(base)
		fmt.Println("No token is set (http.token_env), so this address works only on this computer and needs no sign-in.")
		return nil
	}
	tok := a.Secret(h.TokenEnv)
	if tok == "" {
		return fmt.Errorf("%s is empty: put the dashboard token in .env", h.TokenEnv)
	}
	fmt.Println(base + "#token=" + url.QueryEscape(tok))
	fmt.Println("Anyone with this link can use the dashboard. Open it on your own devices only, and change the token to cut every link off.")
	return nil
}
