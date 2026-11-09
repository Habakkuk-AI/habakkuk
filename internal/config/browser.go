package config

import (
	"fmt"
	"strings"
	"time"
)

// Browser configures the browser toolset (agents opt in by listing the
// toolset "browser"). Each agent gets its own browser: a headless Chrome or
// Chromium Habakkuk starts with a profile under $HABAKKUK_HOME/browser/<agent>
// (mode launch), or tabs in a Chrome someone already runs with
// --remote-debugging-port (mode connect), such as the owner's signed-in one.
type Browser struct {
	Mode     string   `yaml:"mode"`     // launch (default) | connect
	Path     string   `yaml:"path"`     // launch: the browser binary; default the first Chrome or Chromium found
	CDPURL   string   `yaml:"cdp_url"`  // connect: http://127.0.0.1:9222, or a ws:// DevTools URL
	Headless *bool    `yaml:"headless"` // launch: default true
	Args     []string `yaml:"args"`     // launch: extra command-line flags
	// AllowedURLs, when set, are the only pages (documents) the browser may
	// load. BlockedURLs never load, page or not. A pattern containing "://"
	// matches the whole URL, otherwise the host (or host:port); * matches
	// any run of characters, and *.example.com also matches example.com.
	AllowedURLs []string `yaml:"allowed_urls"`
	BlockedURLs []string `yaml:"blocked_urls"`
	// AllowPrivate lists host patterns the browser may reach although they
	// are loopback, private or link-local addresses, which are refused
	// otherwise. ["*"] turns the check off.
	AllowPrivate []string      `yaml:"allow_private"`
	IdleTimeout  time.Duration `yaml:"idle_timeout"` // close the browser after this long unused (default 10m)
	Timeout      time.Duration `yaml:"timeout"`      // per tool call (default 60s)
}

// BrowserConfig is the browser section with defaults applied; a missing
// section is a headless launch with defaults.
func (c *Config) BrowserConfig() Browser {
	var b Browser
	if c.Browser != nil {
		b = *c.Browser
	}
	if b.Mode == "" {
		b.Mode = "launch"
	}
	if b.Headless == nil {
		on := true
		b.Headless = &on
	}
	if b.IdleTimeout == 0 {
		b.IdleTimeout = 10 * time.Minute
	}
	if b.Timeout == 0 {
		b.Timeout = 60 * time.Second
	}
	return b
}

func (c *Config) validateBrowser() []error {
	b := c.Browser
	if b == nil {
		return nil
	}
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("browser: "+format, args...)) }
	switch b.Mode {
	case "", "launch":
		if b.CDPURL != "" {
			bad("cdp_url applies to mode connect only")
		}
	case "connect":
		switch {
		case b.CDPURL == "":
			bad("mode connect needs cdp_url, e.g. http://127.0.0.1:9222")
		case !hasAnyPrefix(b.CDPURL, "http://", "https://", "ws://", "wss://"):
			bad("cdp_url %q must start with http://, https://, ws:// or wss://", b.CDPURL)
		}
		if b.Path != "" || len(b.Args) > 0 || b.Headless != nil {
			bad("path, args and headless apply to mode launch only")
		}
	default:
		bad("mode %q must be launch or connect", b.Mode)
	}
	if b.IdleTimeout < 0 || b.Timeout < 0 {
		bad("idle_timeout and timeout must not be negative")
	}
	for _, p := range append(append(append([]string{}, b.AllowedURLs...), b.BlockedURLs...), b.AllowPrivate...) {
		if strings.TrimSpace(p) == "" {
			bad("empty URL pattern")
		}
	}
	return errs
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
