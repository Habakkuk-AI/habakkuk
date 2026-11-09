package config

import (
	"strings"
	"testing"
)

const homeBase = mediaBase + `channels:
  wa-marketing: {type: whatsappagent, token_env: WA_MARKETING_KEY, agent: habakkuk}
`

func withAgent(agent string) string {
	return strings.Replace(homeBase, "{model: p/m}", agent, 1)
}

func TestHomeDefaults(t *testing.T) {
	cfg, err := Parse([]byte(withAgent("{model: p/m, home: \"wa-marketing:creator\"}")))
	if err != nil {
		t.Fatal(err)
	}
	if a := cfg.Agents["habakkuk"]; a.Home != "wa-marketing:creator" || a.ReportHome != ReportHomeResults {
		t.Fatalf("agent: home %q report_home %q", a.Home, a.ReportHome)
	}
	cfg, err = Parse([]byte(withAgent("{model: p/m, home: \"wa-marketing:creator:t1\", report_home: all}")))
	if err != nil || cfg.Agents["habakkuk"].ReportHome != ReportHomeAll {
		t.Fatalf("all: %v", err)
	}
	cfg, err = Parse([]byte(homeBase))
	if err != nil || cfg.Agents["habakkuk"].ReportHome != "" {
		t.Fatalf("no home: report_home %q, %v", cfg.Agents["habakkuk"].ReportHome, err)
	}
	if _, err := Parse([]byte(withAgent("{model: p/m, report_home: off}"))); err != nil {
		t.Fatalf("report_home off without home: %v", err)
	}
}

func TestHomeValidation(t *testing.T) {
	for agent, want := range map[string]string{
		`{model: p/m, home: "telegram:123"}`:                         `names channel "telegram", which is not configured`,
		`{model: p/m, home: "wa-marketing"}`:                         "want CHANNEL:CONVERSATION",
		`{model: p/m, home: "wa-marketing:"}`:                        "want CHANNEL:CONVERSATION",
		`{model: p/m, home: ":creator"}`:                             "want CHANNEL:CONVERSATION",
		`{model: p/m, home: "wa-marketing:creator", report_home: x}`: `report_home "x" must be off, results or all`,
		`{model: p/m, report_home: all}`:                             "report_home all needs home",
	} {
		_, err := Parse([]byte(withAgent(agent)))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", agent, want, err)
		}
	}
}
