package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const teamConfig = `version: 1
providers:
  fake:
    type: openai
    base_url: http://127.0.0.1:1
    keys_env: [FAKE_KEY]
agents:
  habakkuk:
    model: fake/big   # the main model
    permission: workspace
  sales:
    model: fake/small
    toolsets: [core, board]
    budget:
      daily: 2
channels:
  telegram:
    type: telegram
    agent: habakkuk
    token_env: TELEGRAM_BOT_TOKEN
    routes:
      "-100": sales   # Sales group
`

func teamHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(teamConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func readConfig(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func ptr[T any](v T) *T { return &v }

func TestSetAgentChangesOnlyWhatItIsGiven(t *testing.T) {
	home := teamHome(t)
	a, err := SetAgent(home, "habakkuk", AgentChange{
		Description: ptr("Answers everything\nfor the owner"),
		Model:       ptr("fake/bigger"),
		Fallbacks:   ptr([]string{"fake/small", " ", "fake/small"}),
		Toolsets:    ptr([]string{"core", "memory", "browser"}),
		Daily:       ptr(5.0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Model != "fake/bigger" || !slices.Equal(a.Fallbacks, []string{"fake/small"}) || !slices.Equal(a.Toolsets, []string{"core", "memory", "browser"}) {
		t.Fatalf("agent = %+v", a)
	}
	if a.Description != "Answers everything for the owner" {
		t.Fatalf("description kept a line break: %q", a.Description)
	}
	if a.Budget == nil || a.Budget.Daily.Dollars != 5 || a.Budget.Monthly.Set() {
		t.Fatalf("budget = %+v", a.Budget)
	}
	got := readConfig(t, home)
	for _, want := range []string{"# the main model", "permission: workspace", "daily: 5", "# Sales group"} {
		if !strings.Contains(got, want) {
			t.Errorf("config lost %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "!!") {
		t.Errorf("config has an explicit YAML tag:\n%s", got)
	}
	if bak, err := os.ReadFile(filepath.Join(home, "config.yaml.bak")); err != nil || string(bak) != teamConfig {
		t.Errorf("config.yaml.bak is not the previous file: %v", err)
	}
}

func TestSetAgentRemovesSettingsSetEmpty(t *testing.T) {
	home := teamHome(t)
	a, err := SetAgent(home, "sales", AgentChange{Toolsets: ptr([]string{}), Daily: ptr(0.0)})
	if err != nil {
		t.Fatal(err)
	}
	if a.Toolsets[0] != "core" || len(a.Toolsets) < 3 {
		t.Fatalf("toolsets should fall back to the defaults, got %v", a.Toolsets)
	}
	if a.Budget != nil && (a.Budget.Daily.Set() || a.Budget.Monthly.Set()) {
		t.Fatalf("budget should be gone, got %+v", a.Budget)
	}
	if got := readConfig(t, home); strings.Contains(got, "budget:") || strings.Contains(got, "toolsets:") {
		t.Fatalf("empty settings should be removed from the file:\n%s", got)
	}
}

func TestSetAgentRefusesWhatWouldNotLoad(t *testing.T) {
	home := teamHome(t)
	if _, err := SetAgent(home, "nobody", AgentChange{Model: ptr("fake/x")}); err == nil || !strings.Contains(err.Error(), `no agent "nobody"`) {
		t.Fatalf("unknown agent: %v", err)
	}
	if _, err := SetAgent(home, "sales", AgentChange{Model: ptr("ghost/x")}); err == nil {
		t.Fatal("a model from an unknown provider was saved")
	}
	if got := readConfig(t, home); got != teamConfig {
		t.Fatalf("a refused change touched the file:\n%s", got)
	}
}

func TestRoutesKeepTheirLabels(t *testing.T) {
	home := teamHome(t)
	routes, err := Routes(home, "telegram")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0] != (Route{Conversation: "-100", Agent: "sales", Label: "Sales group"}) {
		t.Fatalf("routes = %+v", routes)
	}
	ch, err := SetChannelRouting(home, "telegram", "sales", []Route{
		{Conversation: "-100", Agent: "habakkuk", Label: "Sales group"},
		{Conversation: "-200", Agent: "sales", Label: "Ops\nteam"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ch.Agent != "sales" || ch.Routes["-100"] != "habakkuk" || ch.Routes["-200"] != "sales" {
		t.Fatalf("channel = %+v", ch)
	}
	routes, _ = Routes(home, "telegram")
	if len(routes) != 2 || routes[0].Label != "Ops team" || routes[1].Label != "Sales group" {
		t.Fatalf("labels after save = %+v", routes)
	}
	if _, err := SetChannelRouting(home, "telegram", "", []Route{{Conversation: "-1", Agent: "ghost"}}); err == nil {
		t.Fatal("a route to an unknown agent was saved")
	}
	if _, err := SetChannelRouting(home, "telegram", "", []Route{{Conversation: "-1", Agent: "sales"}, {Conversation: "-1", Agent: "habakkuk"}}); err == nil {
		t.Fatal("a conversation routed twice was saved")
	}
	ch, err = SetChannelRouting(home, "telegram", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Routes) != 0 || strings.Contains(readConfig(t, home), "routes:") {
		t.Fatalf("no routes should remove the section: %+v", ch.Routes)
	}
}
