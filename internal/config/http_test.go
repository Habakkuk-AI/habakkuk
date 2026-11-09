package config

import (
	"strings"
	"testing"
)

func withHTTP(section string) []byte {
	return []byte(strings.Replace(Template, "\nworkers:", "\n"+section+"\nworkers:", 1))
}

func TestHTTPDefaultsAddWebChannel(t *testing.T) {
	cfg, err := Parse(withHTTP(`http:
  webhooks:
    alerts:
      secret_env: ALERTS_SECRET
      prompt: "Alert {{.title}}: {{.Body}}"
      deliver: web:ops`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Listen != DefaultListen {
		t.Fatalf("listen %q", cfg.HTTP.Listen)
	}
	web := cfg.Channels[WebAccount]
	if cfg.WebAccountName() != WebAccount || web.Agent != "habakkuk" || web.SessionIdle != 0 || web.AllowFrom[0] != "*" {
		t.Fatalf("web channel: %+v", web)
	}
	if cfg.HTTP.Webhooks["alerts"].Agent != "habakkuk" {
		t.Fatal("webhook agent not defaulted")
	}
}

func TestHTTPValidation(t *testing.T) {
	_, err := Parse(withHTTP(`http:
  listen: 0.0.0.0:8484
  webhooks:
    bad:
      agent: ghost
      prompt: "{{.Body"
      deliver: nowhere:1
    "no spaces": {secret_env: X}`))
	if err == nil {
		t.Fatal("bad http section accepted")
	}
	for _, want := range []string{"not a loopback address", `agent "ghost"`, "secret_env is required", "prompt:", `deliver "nowhere:1"`, "1-64 letters"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
	if _, err := Parse(withHTTP("http:\n  listen: 0.0.0.0:8484\n  token_env: HABAKKUK_HTTP_TOKEN")); err != nil {
		t.Fatalf("public address with a token: %v", err)
	}
	_, err = Parse(withHTTP("channels:\n  site:\n    type: web"))
	if err == nil || !strings.Contains(err.Error(), "needs the http section") {
		t.Fatalf("web channel without http: %v", err)
	}
}

func TestIsLoopback(t *testing.T) {
	for host, want := range map[string]bool{"127.0.0.1": true, "::1": true, "localhost": true, "127.0.0.2": true,
		"": false, "0.0.0.0": false, "192.168.1.5": false, "example.com": false} {
		if IsLoopback(host) != want {
			t.Errorf("IsLoopback(%q) = %v", host, !want)
		}
	}
}
