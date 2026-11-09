package config

import (
	"strings"
	"testing"
	"time"
)

func TestBrowserDefaults(t *testing.T) {
	cfg, err := Parse([]byte(Template))
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.BrowserConfig()
	if cfg.Browser != nil || b.Mode != "launch" || !*b.Headless || b.IdleTimeout != 10*time.Minute || b.Timeout != 60*time.Second {
		t.Fatalf("defaults = %+v", b)
	}
	cfg, err = Parse([]byte(Template + "browser:\n  headless: false\n  timeout: 5s\n  allow_private: [\"localhost:3000\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if b := cfg.BrowserConfig(); *b.Headless || b.Timeout != 5*time.Second || b.IdleTimeout != 10*time.Minute || b.AllowPrivate[0] != "localhost:3000" {
		t.Fatalf("configured = %+v", b)
	}
}

func TestBrowserValidation(t *testing.T) {
	for in, want := range map[string]string{
		"mode: connect\n  cdp_url: http://127.0.0.1:9222":                  "",
		"mode: connect\n  cdp_url: ws://127.0.0.1:9222/devtools/browser/x": "",
		"mode: launch\n  path: /usr/bin/chromium\n  args: [--lang=en]":     "",
		"mode: connect": "needs cdp_url",
		"mode: connect\n  cdp_url: 127.0.0.1:9222":              "must start with",
		"mode: connect\n  cdp_url: http://x\n  headless: false": "launch only",
		"cdp_url: http://127.0.0.1:9222":                        "mode connect only",
		"mode: remote":                                          `mode "remote"`,
		"timeout: -1s":                                          "must not be negative",
		"blocked_urls: [\"\"]":                                  "empty URL pattern",
		"headles: true":                                         "not found",
	} {
		_, err := Parse([]byte(Template + "browser:\n  " + in + "\n"))
		switch {
		case want == "" && err != nil:
			t.Errorf("%q: %v", in, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%q: err = %v, want %q", in, err, want)
		}
	}
}
