package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestIntegrationsParse(t *testing.T) {
	cfg, err := Parse(withHTTP(`http: {}
a2a:
  serve: true
  token_env: A2A_TOKEN
  agents: {habakkuk: "My assistant"}
  peers:
    research:
      url: https://agents.example.com/research
      headers: {Authorization: "Bearer ${RESEARCH_TOKEN}"}
homeassistant:
  url: http://homeassistant.local:8123/
  token_env: HA_TOKEN
  allow_domains: [light, switch, climate]
  confirm: [lock, alarm_control_panel, cover]
  triggers:
    - name: doorbell
      entity_id: binary_sensor.*_doorbell
      to: "on"
      deliver: web:home
mcp_servers:
  linear:
    url: https://mcp.linear.app/mcp
    auth: oauth
    scopes: [read]`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.A2A.Name != "Habakkuk" || cfg.A2A.Peers["research"].Timeout != 5*time.Minute {
		t.Fatalf("a2a defaults: %+v", cfg.A2A)
	}
	h := cfg.HomeAssistant
	if h.URL != "http://homeassistant.local:8123" || h.Triggers[0].EventType != "state_changed" || h.Triggers[0].Agent != "habakkuk" ||
		h.Triggers[0].Cooldown != 30*time.Second {
		t.Fatalf("homeassistant defaults: %+v %+v", h, h.Triggers[0])
	}
	names := cfg.SecretEnvNames()
	for _, want := range []string{"A2A_TOKEN", "RESEARCH_TOKEN", "HA_TOKEN"} {
		if !slices.Contains(names, want) {
			t.Errorf("secret names miss %s: %v", want, names)
		}
	}
}

func TestIntegrationsValidation(t *testing.T) {
	_, err := Parse(withHTTP(`a2a:
  serve: true
  agents: {ghost: x}
  peers:
    "bad name": {url: ftp://x}
homeassistant:
  url: homeassistant.local
  allow_domains: [Light]
  triggers:
    - name: t1
      event_type: zha_event
      to: "on"
      agent: ghost
      deliver: nowhere:1
      prompt: "{{.To"
mcp_servers:
  local:
    command: x
    auth: oauth
  other:
    url: https://x.example.com/mcp
    client_id: abc
  hdr:
    url: https://y.example.com/mcp
    auth: oauth
    headers: {Authorization: "Bearer ${X}"}`))
	if err == nil {
		t.Fatal("bad sections accepted")
	}
	for _, want := range []string{"a2a: serve needs the http section", `"ghost" is not a configured agent`, "1-32 letters",
		"url must be an http", "token_env is required", `domain "Light"`, `agent "ghost"`, `deliver "nowhere:1"`, "prompt:",
		"to and from apply to state_changed", "auth: oauth applies to url servers only", "need auth: oauth",
		"sets the Authorization header itself"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
}
