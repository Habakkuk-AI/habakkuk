package config

import (
	"strings"
	"testing"
	"time"
)

func TestNodesConfig(t *testing.T) {
	cfg, err := Parse([]byte(Template + `
http:
  listen: 127.0.0.1:9000
nodes:
  url: https://habakkuk.example.com/
  timeout: 30s
  allow:
    laptop: [screen.snapshot]
    "*": [system.notify]
    pi: ["*"]
`))
	if err != nil {
		t.Fatal(err)
	}
	n := cfg.Nodes
	for _, c := range []struct {
		node, cmd string
		want      bool
	}{
		{"laptop", "screen.snapshot", true}, {"laptop", "system.notify", true}, {"phone", "system.notify", true},
		{"phone", "screen.snapshot", false}, {"pi", "system.run", true}, {"laptop", "system.run", false},
	} {
		if got := n.Allowed(c.node, c.cmd); got != c.want {
			t.Errorf("Allowed(%s, %s) = %v", c.node, c.cmd, got)
		}
	}
	if n.InvokeTimeout() != 30*time.Second || n.PairCodeTTL() != 10*time.Minute {
		t.Errorf("timeouts %v %v", n.InvokeTimeout(), n.PairCodeTTL())
	}
	if got := cfg.NodesURL(); got != "https://habakkuk.example.com" {
		t.Errorf("NodesURL = %q", got)
	}
	cfg.Nodes.URL = ""
	if got := cfg.NodesURL(); got != "http://127.0.0.1:9000" {
		t.Errorf("NodesURL from listen = %q", got)
	}
	var none *Nodes
	if none.Allowed("x", "y") || none.InvokeTimeout() != time.Minute {
		t.Error("a nil nodes section should allow nothing and use defaults")
	}
}

func TestNodesAndLanguageValidation(t *testing.T) {
	for in, want := range map[string]string{
		"nodes: {}\n": "need an http section",
		"http: {listen: 127.0.0.1:9000}\nnodes: {url: ftp://x}\n":                  "must be an http or https URL",
		"http: {listen: 127.0.0.1:9000}\nnodes: {allow: {laptop: [\"run it\"]}}\n": "is not a command name",
		"http: {listen: 127.0.0.1:9000}\nnodes: {timeout: -1s}\n":                  "must not be negative",
		"language: tlh\n": "language \"tlh\" is not one Habakkuk has text for",
	} {
		_, err := Parse([]byte(Template + in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", in, err, want)
		}
	}
	for _, lang := range []string{"fr", "pt-BR", "zh-Hans", "yo", ""} {
		if _, err := Parse([]byte(Template + "language: \"" + lang + "\"\n")); err != nil {
			t.Errorf("language %q: %v", lang, err)
		}
	}
}
